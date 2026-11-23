package timewheel

// 责任边界：本文件用 fake Store 裁决引擎控制流（结算分支、重试/死信判定、
// ACK 重试、领取限流、Shutdown 顺序）。Lua 语义（zset/score/原子性/租约回收）
// 的唯一裁判是真实 Redis 集成用例；fake 应答一律用 encodeEnvelope 构造，
// 与生产 member 同源，避免应答形状漂移。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/util"
)

// fakeCall 记录一次 Eval 调用
type fakeCall struct {
	script string
	keys   []string
	args   []interface{}
}

// fakeStore 按脚本常量分发：应答队列优先，其次错误注入，最后默认应答
type fakeStore struct {
	mu       sync.Mutex
	calls    []fakeCall
	replies  map[string][]interface{}
	errs     map[string][]error
	defaults map[string]interface{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		replies: map[string][]interface{}{},
		errs:    map[string][]error{},
		defaults: map[string]interface{}{
			redis.AddTaskLuaScript:       nil,
			redis.AckTaskLuaScript:       1,
			redis.FailTaskLuaScript:      1,
			redis.DeadTaskLuaScript:      1,
			redis.RecoverLeasesLuaScript: nil,
			// 默认 registry 含一个分钟入口，供 tick 驱动 claim
			redis.ZRangeByScoreLuaScript: []interface{}{"M"},
		},
	}
}

func (f *fakeStore) Eval(_ context.Context, src string, keys []string, args []interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{script: src, keys: keys, args: args})
	if q := f.errs[src]; len(q) > 0 {
		err := q[0]
		f.errs[src] = q[1:]
		return nil, err
	}
	if q := f.replies[src]; len(q) > 0 {
		reply := q[0]
		f.replies[src] = q[1:]
		return reply, nil
	}
	return f.defaults[src], nil
}

func (f *fakeStore) queueReply(script string, reply interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[script] = append(f.replies[script], reply)
}

func (f *fakeStore) queueError(script string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[script] = append(f.errs[script], err)
}

func (f *fakeStore) callsFor(script string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.script == script {
			out = append(out, c)
		}
	}
	return out
}

// fakeMember 构造与生产同源的 claim 产出信封
func fakeMember(key string, attempt int) string {
	return encodeEnvelope(key, attempt, "fake:1", newPayload(key))
}

// newFakeEngine 构造接 fake store 的引擎；tickInterval 取 1h 阻断后台 ticker，
// 测试自行同步调用 tick()/executeAndSettle() 保证确定性
func newFakeEngine(t *testing.T, store *fakeStore, handle func(context.Context, *RTaskElement) error, extra ...RTimeWheelOption) (*RTimeWheel, *resultCollector) {
	t.Helper()
	rc := &resultCollector{}
	opts := append(fastOpts(WithTickInterval(time.Hour), WithResultHook(rc.hook)), extra...)
	tw, err := NewRTimeWheelWithOptions(store, handle, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Stop)
	return tw, rc
}

// runSettle 直调 executeAndSettle 前补齐 tick() 侧的 wg/inflight 计数
func runSettle(tw *RTimeWheel, member string) {
	tw.wg.Add(1)
	tw.inflight.Add(1)
	tw.executeAndSettle(member)
}

func TestSettleSuccessAcks(t *testing.T) {
	store := newFakeStore()
	tw, rc := newFakeEngine(t, store, func(context.Context, *RTaskElement) error { return nil })

	member := fakeMember("k1", 1)
	runSettle(tw, member)

	acks := store.callsFor(redis.AckTaskLuaScript)
	if len(acks) != 1 || acks[0].args[0] != member {
		t.Fatalf("ack calls: %+v", acks)
	}
	if got := rc.byOutcome(OutcomeSuccess); len(got) != 1 || got[0].Attempt != 1 {
		t.Fatalf("success results: %+v", rc.results)
	}
	if n := len(store.callsFor(redis.FailTaskLuaScript)) + len(store.callsFor(redis.DeadTaskLuaScript)); n != 0 {
		t.Fatalf("unexpected fail/dead calls: %d", n)
	}
}

func TestSettleFailureSchedulesRetry(t *testing.T) {
	store := newFakeStore()
	tw, rc := newFakeEngine(t, store,
		func(context.Context, *RTaskElement) error { return errors.New("transient") },
		WithMaxAttempts(5))

	member := fakeMember("k1", 1)
	runSettle(tw, member)

	fails := store.callsFor(redis.FailTaskLuaScript)
	if len(fails) != 1 || fails[0].args[0] != member {
		t.Fatalf("fail calls: %+v", fails)
	}
	// 退避目标分钟必须来自任务的 executeAt
	wantDelSet := delSetKey(util.GetTimeMinuteStr(time.Unix(1700000000, 0)))
	if fails[0].keys[1] != wantDelSet {
		t.Fatalf("fail delSet key: got %s, want %s", fails[0].keys[1], wantDelSet)
	}
	if got := rc.byOutcome(OutcomeRetryScheduled); len(got) != 1 || got[0].Err == nil {
		t.Fatalf("retry results: %+v", rc.results)
	}
	if len(store.callsFor(redis.DeadTaskLuaScript)) != 0 {
		t.Fatal("maxAttempts not reached, dead must not fire")
	}
}

func TestSettleFailureAtMaxAttemptsDeadLetters(t *testing.T) {
	store := newFakeStore()
	tw, rc := newFakeEngine(t, store,
		func(context.Context, *RTaskElement) error { return errors.New("permanent") },
		WithMaxAttempts(3))

	member := fakeMember("k1", 3) // attempt == maxAttempts
	runSettle(tw, member)

	deads := store.callsFor(redis.DeadTaskLuaScript)
	if len(deads) != 1 || deads[0].args[0] != member {
		t.Fatalf("dead calls: %+v", deads)
	}
	if got := rc.byOutcome(OutcomeDeadLettered); len(got) != 1 || got[0].Attempt != 3 || got[0].Err == nil {
		t.Fatalf("dead results: %+v", rc.results)
	}
	if len(store.callsFor(redis.FailTaskLuaScript)) != 0 {
		t.Fatal("at maxAttempts, fail(retry) must not fire")
	}
}

func TestPanicSettlesAsFailure(t *testing.T) {
	store := newFakeStore()
	tw, rc := newFakeEngine(t, store,
		func(context.Context, *RTaskElement) error { panic("boom") },
		WithMaxAttempts(5))

	runSettle(tw, fakeMember("k1", 1))

	if len(store.callsFor(redis.FailTaskLuaScript)) != 1 {
		t.Fatalf("panic must settle as failure; calls: %+v", store.calls)
	}
	if got := rc.byOutcome(OutcomeRetryScheduled); len(got) != 1 {
		t.Fatalf("panic results: %+v", rc.results)
	}
}

func TestAckErrorRetriesThenAckFailed(t *testing.T) {
	store := newFakeStore()
	tw, rc := newFakeEngine(t, store, func(context.Context, *RTaskElement) error { return nil })

	for i := 0; i < 3; i++ {
		store.queueError(redis.AckTaskLuaScript, fmt.Errorf("net %d", i))
	}
	runSettle(tw, fakeMember("k1", 1))

	if got := len(store.callsFor(redis.AckTaskLuaScript)); got != 3 {
		t.Fatalf("ack attempts: got %d, want 3", got)
	}
	if got := rc.byOutcome(OutcomeAckFailed); len(got) != 1 || got[0].Err == nil {
		t.Fatalf("ack_failed results: %+v", rc.results)
	}
	if len(rc.byOutcome(OutcomeSuccess)) != 0 {
		t.Fatal("ack never succeeded, success must not emit")
	}
}

func TestFailDeletedEmitsRemoved(t *testing.T) {
	store := newFakeStore()
	store.queueReply(redis.FailTaskLuaScript, 2) // 已删除：移除不重试
	tw, rc := newFakeEngine(t, store,
		func(context.Context, *RTaskElement) error { return errors.New("x") })

	runSettle(tw, fakeMember("k1", 1))

	if got := rc.byOutcome(OutcomeRemoved); len(got) != 1 {
		t.Fatalf("removed results: %+v", rc.results)
	}
	if len(rc.byOutcome(OutcomeRetryScheduled)) != 0 {
		t.Fatal("deleted task must not schedule retry")
	}
}

func TestClaimBoundedByConcurrency(t *testing.T) {
	store := newFakeStore()
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	handle := func(_ context.Context, _ *RTaskElement) error {
		entered <- struct{}{}
		<-release
		return nil
	}
	tw, _ := newFakeEngine(t, store, handle, WithMaxConcurrency(3), WithClaimBatch(100))

	// 第一 tick：avail=3，领取 2 笔（应答只给 2 个 member）
	store.queueReply(redis.ClaimTasksLuaScript, []interface{}{fakeMember("k1", 1), fakeMember("k2", 1)})
	tw.tick()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("handler not started")
		}
	}

	// 第二 tick：inflight=2，avail=1 → claim limit 必须收敛到 1
	store.queueReply(redis.ClaimTasksLuaScript, []interface{}{fakeMember("k3", 1)})
	tw.tick()

	claims := store.callsFor(redis.ClaimTasksLuaScript)
	if len(claims) != 2 {
		t.Fatalf("claim calls: %d", len(claims))
	}
	if got := claims[1].args[0]; got != 1 {
		t.Fatalf("second claim limit: got %v, want 1 (maxConcurrency - inflight)", got)
	}
	close(release)
}

func TestShutdownWaitsForInflight(t *testing.T) {
	store := newFakeStore()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handle := func(_ context.Context, _ *RTaskElement) error {
		entered <- struct{}{}
		<-release
		return nil
	}
	tw, _ := newFakeEngine(t, store, handle)

	store.queueReply(redis.ClaimTasksLuaScript, []interface{}{fakeMember("k1", 1)})
	tw.tick()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not started")
	}

	res := make(chan error, 1)
	go func() {
		res <- tw.Shutdown(context.Background())
	}()
	select {
	case err := <-res:
		t.Fatalf("Shutdown returned %v before in-flight handler settled", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after release")
	}
}
