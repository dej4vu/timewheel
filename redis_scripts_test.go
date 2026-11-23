package timewheel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/util"
)

func newPayload(key string) []byte {
	return []byte(fmt.Sprintf(`{"key":%q,"msg":"m","type":"t","executeAtUnix":1700000000}`, key))
}

// zscoreOf 读取 zset 成员 score；不存在返回 (0,false)（测试辅助）
func zscoreOf(t *testing.T, s *redisScripts, key, member string) (float64, bool) {
	t.Helper()
	reply, err := s.store.Eval(context.Background(),
		"return redis.call('zscore', KEYS[1], ARGV[1])", []string{key}, []interface{}{member})
	if err != nil {
		// goredis 对不存在的成员返回 redis: nil 错误
		if strings.Contains(err.Error(), "nil") {
			return 0, false
		}
		t.Fatalf("zscore: %v", err)
	}
	if reply == nil {
		return 0, false
	}
	str := ""
	switch v := reply.(type) {
	case []byte:
		str = string(v)
	default:
		str = fmt.Sprintf("%v", v)
	}
	if str == "" || str == "<nil>" {
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscanf(str, "%g", &f); err != nil {
		t.Fatalf("zscore %q: %v", str, err)
	}
	return f, true
}

func TestScriptAddAndClaim(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	startMs := util.MinuteStartMs(now)
	s := newRedisScripts(driver)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	ctx := context.Background()

	// 三笔任务（已到期），key-del 已被标记删除
	for _, key := range []string{"k-keep", "k-del", "k-keep2"} {
		if err := s.addTask(ctx, now.Add(-time.Second).UnixMilli(),
			encodeEnvelope(key, 0, "", newPayload(key)), key, minute, startMs); err != nil {
			t.Fatal(err)
		}
	}
	markDeleted(t, s, minute, "k-del")

	claimed, err := s.claim(ctx, minute, 10, time.Second, "batch-uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed: got %d, want 2 (k-del filtered)", len(claimed))
	}
	keys := map[string]bool{}
	for _, m := range claimed {
		env, err := decodeEnvelope(m)
		if err != nil {
			t.Fatalf("decode claimed: %v", err)
		}
		if env.Attempt != 1 {
			t.Errorf("attempt: got %d, want 1", env.Attempt)
		}
		if env.ClaimID == "" || !strings.HasPrefix(env.ClaimID, "batch-uuid-1:") {
			t.Errorf("claimID: got %q, want prefix batch-uuid-1:<i>", env.ClaimID)
		}
		keys[env.Key] = true
		// processing 应持有该 member，score 为租约
		if _, ok := zscoreOf(t, s, processingKey, m); !ok {
			t.Errorf("processing should hold claimed member %q", env.Key)
		}
	}
	if keys["k-del"] {
		t.Errorf("deleted task should not be claimed")
	}
	// pending 分片应清空
	if members := zrangeAll(t, driver, pendingKey(minute)); len(members) != 0 {
		t.Errorf("pending should be empty after claim, got %v", members)
	}
}

func markDeleted(t *testing.T, s *redisScripts, minute, key string) {
	t.Helper()
	if _, err := s.store.Eval(context.Background(),
		"return redis.call('sadd', KEYS[1], ARGV[1])", []string{delSetKey(minute)}, []interface{}{key}); err != nil {
		t.Fatalf("sadd delset: %v", err)
	}
}

func TestClaimBatchOverUnpackLimit(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	s := newRedisScripts(driver)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	ctx := context.Background()

	const total = 600 // 跨越 500 元素分批边界
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("k-%d", i)
		if err := s.addTask(ctx, now.Add(-time.Second).UnixMilli(),
			encodeEnvelope(key, 0, "", newPayload(key)), key, minute,
			util.MinuteStartMs(now)); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := s.claim(ctx, minute, total, time.Second, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != total {
		t.Fatalf("claimed: got %d, want %d", len(claimed), total)
	}
}

func TestClaimAtomicBatch(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	ctx := context.Background()
	s0 := newRedisScripts(driver)
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("k-%d", i)
		if err := s0.addTask(ctx, now.Add(-time.Second).UnixMilli(),
			encodeEnvelope(key, 0, "", newPayload(key)), key, minute,
			util.MinuteStartMs(now)); err != nil {
			t.Fatal(err)
		}
	}

	// 同一 store 上两个并发 claim 实例：领取互斥、无重复
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := newRedisScripts(driver)
			claimed, err := s.claim(ctx, minute, 100, time.Second, fmt.Sprintf("b%d", i))
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range claimed {
				env, err := decodeEnvelope(m)
				if err != nil {
					t.Errorf("decode: %v", err)
					continue
				}
				if seen[env.Key] {
					t.Errorf("task %s claimed twice", env.Key)
				}
				seen[env.Key] = true
			}
		}(i)
	}
	wg.Wait()
	if len(seen) != 100 {
		t.Fatalf("unique claimed: got %d, want 100", len(seen))
	}
}

func TestAckAbaNoop(t *testing.T) {
	driver := testStore(t)
	flushTimewheelKeys(t, driver)
	s := newRedisScripts(driver)
	ctx := context.Background()

	member := encodeEnvelope("k1", 1, "b:1", newPayload("k1"))
	if err := s.pushRaw(ctx, processingKey, 1000, member); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ack(ctx, member)
	if err != nil || !ok {
		t.Fatalf("ack existing: ok=%v err=%v", ok, err)
	}
	// 已被移除（模拟 recover 改写后旧 member 消失）
	ok, err = s.ack(ctx, member)
	if err != nil {
		t.Fatalf("ack missing: %v", err)
	}
	if ok {
		t.Errorf("ack on missing member should be no-op")
	}
}

func TestFailDelayAndDeleted(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	s := newRedisScripts(driver)
	ctx := context.Background()

	member := encodeEnvelope("k1", 2, "b:1", newPayload("k1"))
	if err := s.pushRaw(ctx, processingKey, 1, member); err != nil {
		t.Fatal(err)
	}

	// 延期：score 更新为 now+backoff，member 不变
	backoffMs := 5 * time.Second
	applied, deleted, err := s.fail(ctx, member, backoffMs, minute)
	if err != nil || !applied || deleted {
		t.Fatalf("fail: applied=%v deleted=%v err=%v", applied, deleted, err)
	}
	score, ok := zscoreOf(t, s, processingKey, member)
	if !ok {
		t.Fatalf("member should remain in processing")
	}
	if score < float64(time.Now().UnixMilli()+backoffMs.Milliseconds()-1000) {
		t.Errorf("lease score should be delayed, got %v", score)
	}

	// 已删除：fail 应移除而非延期
	member2 := encodeEnvelope("k2", 1, "b:2", newPayload("k2"))
	if err := s.pushRaw(ctx, processingKey, 1, member2); err != nil {
		t.Fatal(err)
	}
	markDeleted(t, s, minute, "k2")
	applied, deleted, err = s.fail(ctx, member2, backoffMs, minute)
	if err != nil || applied || !deleted {
		t.Fatalf("fail deleted task: applied=%v deleted=%v err=%v", applied, deleted, err)
	}
	if _, ok := zscoreOf(t, s, processingKey, member2); ok {
		t.Errorf("deleted task should be removed from processing")
	}
}

func TestRecoverRequeuesToCurrentMinute(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	s := newRedisScripts(driver)
	ctx := context.Background()

	// 过期租约条目（attempt=1），registry 为空
	member := encodeEnvelope("k1", 1, "b:1", newPayload("k1"))
	if err := s.pushRaw(ctx, processingKey, time.Now().Add(-time.Second).UnixMilli(), member); err != nil {
		t.Fatal(err)
	}

	if err := s.recover(ctx, minute, util.MinuteStartMs(now), 10, 10,
		time.Millisecond, 10*time.Millisecond, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := zscoreOf(t, s, processingKey, member); ok {
		t.Fatalf("expired lease should be removed from processing")
	}

	// registry 应能看到当前分钟（空 registry 状态下 recover 补登记）
	minutes, err := s.listMinutes(ctx, time.Now().UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range minutes {
		if m == minute {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry should contain current minute %q after recover, got %v", minute, minutes)
	}

	// 放回的任务应可被再次领取，attempt 递增
	claimed, err := s.claim(ctx, minute, 10, time.Second, "b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("requeued task should be claimable, got %d", len(claimed))
	}
	env, _ := decodeEnvelope(claimed[0])
	if env.Key != "k1" || env.Attempt != 2 || env.ClaimID != "b2:1" {
		t.Errorf("requeued envelope mismatch: %+v", env)
	}
}

func TestRecoverDeadOnMaxAttempts(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	s := newRedisScripts(driver)
	ctx := context.Background()

	member := encodeEnvelope("k1", 5, "b:1", newPayload("k1"))
	if err := s.pushRaw(ctx, processingKey, time.Now().Add(-time.Second).UnixMilli(), member); err != nil {
		t.Fatal(err)
	}
	if err := s.recover(ctx, minute, util.MinuteStartMs(now), 10, 5,
		time.Millisecond, 10*time.Millisecond, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := zscoreOf(t, s, processingKey, member); ok {
		t.Errorf("processing should be empty")
	}
	if dead := zrangeAll(t, driver, deadKey); len(dead) != 1 {
		t.Fatalf("dead: got %d, want 1", len(dead))
	}
}

func TestClaimLegacyMember(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	s := newRedisScripts(driver)
	ctx := context.Background()

	// 旧格式：裸 JSON member，score 为秒级时间戳（已过期）
	legacy := `{"key":"legacy-1","msg":"m","type":"t","executeAtUnix":1700000000}`
	if err := s.pushRaw(ctx, pendingKey(minute), time.Now().Add(-time.Minute).Unix(), legacy); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.claim(ctx, minute, 10, time.Second, "lb")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("legacy member should be claimed, got %d", len(claimed))
	}
	env, err := decodeEnvelope(claimed[0])
	if err != nil {
		t.Fatal(err)
	}
	if env.Key != "legacy-1" || env.Attempt != 1 {
		t.Errorf("legacy claim envelope: %+v", env)
	}
	task, err := decodeTaskPayload(env.Payload)
	if err != nil || task.Key != "legacy-1" {
		t.Errorf("legacy payload: %v %+v", err, task)
	}
}

func TestClaimMalformedToDead(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	s := newRedisScripts(driver)
	ctx := context.Background()

	// 信封段数错误 与 坏 JSON 裸 member
	bad1 := "v1\x1fk1\x1f1" // 段数不足
	bad2 := `{"key":`       // 损坏 JSON
	for i, m := range []string{bad1, bad2} {
		if err := s.pushRaw(ctx, pendingKey(minute), time.Now().Add(-time.Second).UnixMilli(), m); err != nil {
			t.Fatal(i, err)
		}
	}

	claimed, err := s.claim(ctx, minute, 10, time.Second, "mb")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("malformed should not be claimed, got %v", claimed)
	}
	if dead := zrangeAll(t, driver, deadKey); len(dead) != 2 {
		t.Fatalf("malformed members should go to dead, got %d", len(dead))
	}
}

func TestClaimEmptySliceRegistryCleanup(t *testing.T) {
	driver := testStore(t)
	now := time.Now()
	minute := util.GetTimeMinuteStr(now)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	s := newRedisScripts(driver)
	ctx := context.Background()

	key := "k1"
	if err := s.addTask(ctx, now.Add(-time.Second).UnixMilli(),
		encodeEnvelope(key, 0, "", newPayload(key)), key, minute,
		util.MinuteStartMs(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(ctx, minute, 10, time.Second, "cb"); err != nil {
		t.Fatal(err)
	}

	// 分片已空：再次 claim（due 为空）应触发 registry 清理
	if _, err := s.claim(ctx, minute, 10, time.Second, "cb2"); err != nil {
		t.Fatal(err)
	}
	minutes, err := s.listMinutes(ctx, time.Now().UnixMilli(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range minutes {
		if m == minute {
			t.Errorf("empty slice %q should be removed from registry, got %v", minute, minutes)
		}
	}
}
