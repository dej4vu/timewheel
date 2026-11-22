package timewheel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/util"
)

// fastOpts 面向集成测试的极速参数。
// lease 取 5s 而非贴近 handler 时长：at-least-once 语义允许租约过期后重投，
// 断言"恰好一次"的用例必须让租约远大于测试窗口，否则 -race 调度抖动会产生合法重投。
func fastOpts(extra ...RTimeWheelOption) []RTimeWheelOption {
	base := []RTimeWheelOption{
		WithTickInterval(50 * time.Millisecond),
		WithHandlerTimeout(200 * time.Millisecond),
		WithLease(5 * time.Second),
		WithBackoff(50*time.Millisecond, 200*time.Millisecond),
		WithClaimBatch(100),
		WithMaxConcurrency(32),
		WithStatsInterval(time.Hour), // 测试中默认关闭统计
	}
	return append(base, extra...)
}

// collectResults 收集 result hook 事件
type resultCollector struct {
	mu      sync.Mutex
	results []TaskResult
}

func (c *resultCollector) hook(_ context.Context, r TaskResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = append(c.results, r)
}

func (c *resultCollector) byOutcome(o Outcome) []TaskResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []TaskResult
	for _, r := range c.results {
		if r.Outcome == o {
			out = append(out, r)
		}
	}
	return out
}

func Test_EngineEndToEnd(t *testing.T) {
	stores := testStores(t)
	driver := stores[0] // goredis
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

	tw, err := NewRTimeWheelWithOptions(driver, handle, fastOpts()...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()
	ctx := context.Background()

	if err := tw.AddTask(ctx, "test1", &RTaskElement{Msg: "m1", Type: "t"}, now.Add(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := tw.AddTask(ctx, "test2", &RTaskElement{Msg: "m2", Type: "t"}, now.Add(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := tw.RemoveTask(ctx, "test2", now.Add(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := tw.AddTask(ctx, "test3", &RTaskElement{Msg: "m3", Type: "t"}, now.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return executed["test1"] == 1 && executed["test3"] == 1
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("executed=%v, want test1/test3 each once", executed)
	}

	// 再等一个执行窗口，确认 test2 未被执行、无重复
	time.Sleep(700 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if executed["test2"] != 0 {
		t.Errorf("removed task test2 executed %d times", executed["test2"])
	}
	if executed["test1"] != 1 || executed["test3"] != 1 {
		t.Errorf("duplicate execution: %v", executed)
	}
}

func Test_NotEarlierThanExecuteAt(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	executeAt := now.Add(1500 * time.Millisecond) // 亚秒执行时间
	minute := util.GetTimeMinuteStr(executeAt)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var mu sync.Mutex
	var firedAt time.Time
	handle := func(_ context.Context, task *RTaskElement) error {
		mu.Lock()
		defer mu.Unlock()
		firedAt = time.Now()
		return nil
	}

	tw, err := NewRTimeWheelWithOptions(driver, handle, fastOpts()...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "subsec", &RTaskElement{Msg: "m", Type: "t"}, executeAt); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !firedAt.IsZero()
	}) {
		t.Fatal("task not executed")
	}
	mu.Lock()
	defer mu.Unlock()
	if firedAt.Before(executeAt) {
		t.Errorf("fired at %v, before executeAt %v (early by %v)", firedAt, executeAt, executeAt.Sub(firedAt))
	}
}

func TestRetryWithBackoff(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var calls atomic.Int64
	handle := func(_ context.Context, task *RTaskElement) error {
		if calls.Add(1) <= 2 {
			return errors.New("transient failure")
		}
		return nil
	}
	rc := &resultCollector{}
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithMaxAttempts(5))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "retry-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeSuccess)) == 1
	}) {
		t.Fatalf("no success; results=%v", rc.results)
	}
	if got := len(rc.byOutcome(OutcomeRetryScheduled)); got != 2 {
		t.Errorf("retry_scheduled: got %d, want 2", got)
	}
	if calls.Load() != 3 {
		t.Errorf("handler calls: got %d, want 3", calls.Load())
	}
	succ := rc.byOutcome(OutcomeSuccess)[0]
	if succ.Attempt != 3 {
		t.Errorf("success attempt: got %d, want 3", succ.Attempt)
	}
}

func TestDeadLetterAfterMaxAttempts(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	handle := func(_ context.Context, task *RTaskElement) error {
		return errors.New("permanent failure")
	}
	rc := &resultCollector{}
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithMaxAttempts(3))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "dead-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeDeadLettered)) == 1
	}) {
		t.Fatalf("no dead_lettered; results=%v", rc.results)
	}
	dl := rc.byOutcome(OutcomeDeadLettered)[0]
	if dl.Attempt != 3 || dl.Err == nil {
		t.Errorf("dead_lettered result: %+v", dl)
	}
	if dead := zrangeAll(t, driver, deadKey); len(dead) != 1 {
		t.Errorf("dead zset: got %d, want 1", len(dead))
	}
}

func TestHandlerPanicCountsAsFailure(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))

	var calls atomic.Int64
	handle := func(_ context.Context, task *RTaskElement) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return nil
	}
	rc := &resultCollector{}
	tw, err := NewRTimeWheelWithOptions(driver, handle, append(fastOpts(WithResultHook(rc.hook)), WithMaxAttempts(5))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Stop()

	if err := tw.AddTask(context.Background(), "panic-1", &RTaskElement{Msg: "m", Type: "t"}, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return len(rc.byOutcome(OutcomeSuccess)) == 1
	}) {
		t.Fatalf("no success after panic; results=%v", rc.results)
	}
	if got := len(rc.byOutcome(OutcomeRetryScheduled)); got != 1 {
		t.Errorf("panic should schedule one retry, got %d", got)
	}
}

func TestRemoveTaskPending(t *testing.T) {
	driver := testStores(t)[0]
	now := time.Now()
	executeAt := now.Add(400 * time.Millisecond)
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
	ctx := context.Background()

	if err := tw.AddTask(ctx, "rm-1", &RTaskElement{Msg: "m", Type: "t"}, executeAt); err != nil {
		t.Fatal(err)
	}
	if err := tw.RemoveTask(ctx, "rm-1", executeAt); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1 * time.Second)
	if n := calls.Load(); n != 0 {
		t.Errorf("removed task executed %d times", n)
	}
}
