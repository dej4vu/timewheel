package timewheel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/util"
)

// ackBrokenStore 注入 ACK 脚本失败（其余调用透传真实 store）
type ackBrokenStore struct {
	redis.Store
	failAck atomic.Bool
}

func (s *ackBrokenStore) Eval(ctx context.Context, src string, keys []string, args []interface{}) (interface{}, error) {
	if src == redis.AckTaskLuaScript && s.failAck.Load() {
		return nil, errors.New("injected ack failure")
	}
	return s.Store.Eval(ctx, src, keys, args)
}

func TestStatsHookSnapshot(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var snapshots atomic.Int64
	var lastLag atomic.Int64
	statsHook := func(_ context.Context, s Stats) {
		snapshots.Add(1)
		lastLag.Store(s.ClaimLagMs)
	}

	var calls atomic.Int64
	handle := func(_ context.Context, task *RTaskElement) error {
		calls.Add(1)
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle,
		fastOpts(WithStatsInterval(100*time.Millisecond), WithStatsHook(statsHook))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "stat-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return calls.Load() == 1 && snapshots.Load() >= 1
	}) {
		t.Fatalf("stats not emitted or task not run; snapshots=%d calls=%d", snapshots.Load(), calls.Load())
	}
	if lag := lastLag.Load(); lag < 0 {
		t.Errorf("claim lag should be non-negative, got %d", lag)
	}
}

func TestAckFailureReportsOutcome(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	broken := &ackBrokenStore{Store: driver}
	broken.failAck.Store(true)

	rc := &resultCollector{}
	handle := func(_ context.Context, task *RTaskElement) error { return nil }
	tw, err := NewRTimeWheelWithOptions(broken, handle, append(fastOpts(WithResultHook(rc.hook)), WithLease(2*time.Second))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "ackfail-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	// handler 成功但 ACK 注入失败 3 次：应报告 ack_failed（任务留 processing 由租约回收）
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeAckFailed)) == 1
	}) {
		t.Fatalf("no ack_failed outcome; results=%v", rc.results)
	}

	// 修复 ACK 后（取消注入），租约到期任务被回收重投并最终成功
	broken.failAck.Store(false)
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeSuccess)) == 1
	}) {
		t.Fatalf("task not redelivered after ack failure; results=%v", rc.results)
	}
}
