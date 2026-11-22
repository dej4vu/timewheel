package timewheel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/util"
)

func TestCrashRecoveryByLease(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	// 模拟崩溃：AddTask 后用脚本层直接领取，且不结算（无 ACK/fail）
	s := newRedisScripts(driver)
	legacyTask := &RTaskElement{Key: "crash-1", Msg: "m", Type: "t", ExecuteAtUnix: now.Add(-time.Second).Unix()}
	payload, _ := json.Marshal(legacyTask)
	if err := s.addTask(context.Background(), now.Add(-time.Second).UnixMilli(),
		encodeEnvelope("crash-1", 0, "", payload), "crash-1", minute, util.MinuteStartMs(now)); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.claim(context.Background(), minute, 10, 400*time.Millisecond, "crash-batch")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("simulate-crash claim: %v, %d", err, len(claimed))
	}

	var mu sync.Mutex
	attempts := []int{}
	rc := &resultCollector{}
	handle := func(_ context.Context, task *RTaskElement) error {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, 1)
		return nil
	}
	// 新实例接手：租约 400ms 到期后由 recover 重投
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithLease(2*time.Second))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeSuccess)) == 1
	}) {
		t.Fatalf("lease not recovered; results=%v", rc.results)
	}
	succ := rc.byOutcome(OutcomeSuccess)[0]
	if succ.Attempt != 2 {
		t.Errorf("recovered attempt: got %d, want 2 (crash claim was 1st)", succ.Attempt)
	}
}

func TestNoDuplicateWithinLease(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var calls atomic.Int64
	slow := func(_ context.Context, task *RTaskElement) error {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond) // < lease(500ms)
		return nil
	}

	// 两个实例同库并发扫描
	opts := fastOpts(WithMaxConcurrency(1))
	tw1, err := NewRTimeWheelWithOptions(driver, slow, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw1.Stop()
	tw2, err := NewRTimeWheelWithOptions(driver, slow, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw2.Stop()

	if err := tw1.AddTask(context.Background(), "dup-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	// 等待执行完成（200ms 任务 + 500ms 缓冲）后校验恰好一次
	time.Sleep(1200 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("task executed %d times within lease, want 1", n)
	}
}

func TestGracefulShutdownWaits(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	rc := &resultCollector{}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	handle := func(_ context.Context, task *RTaskElement) error {
		started <- struct{}{}
		<-release
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithHandlerTimeout(5*time.Second), WithLease(10*time.Second))...)
	if err != nil {
		t.Fatal(err)
	}

	if err := tw.AddTask(context.Background(), "sd-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	// 确定性等待任务被领取、handler 进入执行
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("task not claimed in time")
	}

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownErr <- tw.Shutdown(ctx)
	}()
	time.Sleep(200 * time.Millisecond) // Shutdown 应阻塞等待 handler
	close(release)
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := len(rc.byOutcome(OutcomeSuccess)); got != 1 {
		t.Errorf("in-flight task should settle before Shutdown returns; success=%d results=%v", got, rc.results)
	}
}

func TestShutdownTimeoutLeavesSettling(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	rc := &resultCollector{}
	started := make(chan struct{}, 1)
	handle := func(_ context.Context, task *RTaskElement) error {
		started <- struct{}{}
		time.Sleep(600 * time.Millisecond)
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithHandlerTimeout(5*time.Second), WithLease(10*time.Second))...)
	if err != nil {
		t.Fatal(err)
	}

	if err := tw.AddTask(context.Background(), "sd-2", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started: // 任务已被领取、handler 运行中
	case <-time.After(5 * time.Second):
		t.Fatal("task not claimed in time")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := tw.Shutdown(ctx); err == nil {
		t.Fatalf("Shutdown with short ctx should time out, got nil")
	}

	// 不静默丢弃：handler 完成后仍会 ACK 结算
	if !eventually(t, 3*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeSuccess)) == 1
	}) {
		t.Fatalf("task dropped after Shutdown timeout; results=%v", rc.results)
	}
}

func TestBacklogAcrossMinutes(t *testing.T) {
	driver := testStores(t)[0]
	// 任务挂在 3 分钟前的旧分片（宕机积压场景）
	executeAt := time.Now().Add(-3 * time.Minute)
	minute := util.GetTimeMinuteStr(executeAt)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var calls atomic.Int64
	handle := func(_ context.Context, task *RTaskElement) error {
		calls.Add(1)
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, fastOpts()...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "backlog-1", &RTaskElement{Msg: "m", Type: "t"}, executeAt); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return calls.Load() == 1
	}) {
		t.Fatalf("backlogged task not executed, calls=%d", calls.Load())
	}
}

func TestTickNoStacking(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var active, maxActive atomic.Int64
	handle := func(_ context.Context, task *RTaskElement) error {
		cur := active.Add(1)
		for {
			old := maxActive.Load()
			if cur <= old || maxActive.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
		active.Add(-1)
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, fastOpts(WithMaxConcurrency(2))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("stk-%d", i)
		if err := tw.AddTask(context.Background(), key, &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(1500 * time.Millisecond)
	if m := maxActive.Load(); m > 2 {
		t.Errorf("max concurrent handlers: got %d, want <= 2 (maxConcurrency)", m)
	}
}

func TestMultiInstanceExclusiveClaim(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var mu sync.Mutex
	executed := map[string]int{}
	handle := func(_ context.Context, task *RTaskElement) error {
		mu.Lock()
		defer mu.Unlock()
		executed[task.Key]++
		return nil
	}
	opts := fastOpts(WithMaxConcurrency(4))
	tw1, err := NewRTimeWheelWithOptions(driver, handle, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw1.Stop()
	tw2, err := NewRTimeWheelWithOptions(driver, handle, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw2.Stop()

	const total = 50
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("multi-%d", i)
		if err := tw1.AddTask(context.Background(), key, &RTaskElement{Msg: "m", Type: "t"}, now.Add(150*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}

	if !eventually(t, 8*time.Second, 50*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(executed) == total
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("executed %d/%d: %v", len(executed), total, executed)
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for key, n := range executed {
		if n != 1 {
			t.Errorf("task %s executed %d times", key, n)
		}
	}
}

func TestRemoveWhileProcessing(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var inHandler atomic.Bool
	rc := &resultCollector{}
	executeAt := now.Add(150 * time.Millisecond)
	handle := func(_ context.Context, task *RTaskElement) error {
		inHandler.Store(true)
		time.Sleep(300 * time.Millisecond)
		return errors.New("fail after removal")
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithMaxAttempts(5))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()
	ctx := context.Background()

	if err := tw.AddTask(ctx, "rmproc-1", &RTaskElement{Msg: "m", Type: "t"}, executeAt); err != nil {
		t.Fatal(err)
	}
	// 等 handler 开始执行后取消任务
	if !eventually(t, 3*time.Second, 20*time.Millisecond, func() bool { return inHandler.Load() }) {
		t.Fatal("handler not started")
	}
	if err := tw.RemoveTask(ctx, "rmproc-1", executeAt); err != nil {
		t.Fatal(err)
	}

	// 失败结算应发现删除标记：removed，不再重试
	if !eventually(t, 3*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeRemoved)) == 1
	}) {
		t.Fatalf("no removed outcome; results=%v", rc.results)
	}
	time.Sleep(600 * time.Millisecond)
	if got := len(rc.byOutcome(OutcomeRetryScheduled)); got != 0 {
		t.Errorf("removed task should not retry, retry_scheduled=%d", got)
	}
}

func TestLegacyMemberUpgrade(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	// 手写旧格式：裸 JSON member + 秒级 score + registry 登记
	s := newRedisScripts(driver)
	legacy := `{"key":"legacy-up-1","msg":"m","type":"t","executeAtUnix":` + fmt.Sprint(now.Add(-3*time.Second).Unix()) + `}`
	if err := s.pushRaw(context.Background(), pendingKey(minute), now.Add(-3*time.Second).Unix(), legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.pushRaw(context.Background(), registryKey, util.MinuteStartMs(now), minute); err != nil {
		t.Fatal(err)
	}

	rc := &resultCollector{}
	var gotKey atomic.Value
	handle := func(_ context.Context, task *RTaskElement) error {
		gotKey.Store(task.Key)
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, fastOpts(WithResultHook(rc.hook))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeSuccess)) == 1
	}) {
		t.Fatalf("legacy member not consumed; results=%v", rc.results)
	}
	if k, _ := gotKey.Load().(string); k != "legacy-up-1" {
		t.Errorf("legacy task key: got %q", k)
	}
	if succ := rc.byOutcome(OutcomeSuccess)[0]; succ.Attempt != 1 {
		t.Errorf("legacy attempt: got %d, want 1", succ.Attempt)
	}
}

func TestMalformedMemberNotExecuted(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var calls atomic.Int64
	handle := func(_ context.Context, task *RTaskElement) error {
		calls.Add(1)
		return nil
	}
	tw, err := NewRTimeWheelWithOptions(driver, handle, fastOpts()...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	// 坏信封 member（段数不足）挂入分片并登记 registry
	s := newRedisScripts(driver)
	ctx := context.Background()
	if err := s.pushRaw(ctx, pendingKey(minute), now.Add(-time.Second).UnixMilli(), "v1\x1fbroken"); err != nil {
		t.Fatal(err)
	}
	if err := s.pushRaw(ctx, registryKey, util.MinuteStartMs(now), minute); err != nil {
		t.Fatal(err)
	}

	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(zrangeAll(t, driver, deadKey)) == 1
	}) {
		t.Fatalf("malformed member not dead-lettered")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("malformed member executed %d times, want 0", n)
	}
}
