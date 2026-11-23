package timewheel

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/demdxx/gocast"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/util"
)

func Test_LuaScript(t *testing.T) {
	scripts := []struct {
		name string
		src  string
	}{
		{"AddTaskLuaScript", redis.AddTaskLuaScript},
		{"ClaimTasksLuaScript", redis.ClaimTasksLuaScript},
		{"AckTaskLuaScript", redis.AckTaskLuaScript},
		{"FailTaskLuaScript", redis.FailTaskLuaScript},
		{"DeadTaskLuaScript", redis.DeadTaskLuaScript},
		{"RecoverLeasesLuaScript", redis.RecoverLeasesLuaScript},
		{"ZRangeByScoreLuaScript", redis.ZRangeByScoreLuaScript},
		{"StatsLuaScript", redis.StatsLuaScript},
		{"DeleteKeysLuaScript", redis.DeleteKeysLuaScript},
		{"ScanStepLuaScript", redis.ScanStepLuaScript},
		{"RegisterMinuteLuaScript", redis.RegisterMinuteLuaScript},
		{"DeleteTaskLuaScript", redis.DeleteTaskLuaScript},
	}
	for _, s := range scripts {
		if s.src == "" {
			t.Errorf("%s should not be empty", s.name)
		}
		t.Logf("%s :%v", s.name, s.src)
	}

	// Redis 内置 Lua 为 5.1：不支持十六进制字符转义，分隔符必须写十进制 \31；
	// 任何 \xNN 转义都会导致语法错误或分割失败。
	if !strings.Contains(redis.ClaimTasksLuaScript, `\31`) {
		t.Errorf(`claim script must use decimal escape \31 for the 0x1f separator`)
	}
	for _, s := range scripts {
		if strings.Contains(s.src, `\x`) {
			t.Errorf("%s must not use lua-5.2 hex escapes (redis 5.0 embeds lua 5.1)", s.name)
		}
	}
}

// Test_RedisTimeWheel_AddTask 验证 AddTask/RemoveTask 在 Redis 侧的写入结构
// （端到端触发用例在引擎重写后恢复）
func Test_RedisTimeWheel_AddTask(t *testing.T) {
	driver := testStore(t)
	executeAt := time.Now().Add(30 * time.Second)
	minute := util.GetTimeMinuteStr(executeAt)
	flushTimewheelKeys(t, driver, pendingKey(minute), delSetKey(minute))
	ctx := context.Background()
	rTimeWheel := NewRTimeWheel(driver, handle)
	defer rTimeWheel.Stop()

	if err := rTimeWheel.AddTask(ctx, "test1", &RTaskElement{
		Msg:  "hello",
		Type: "test",
	}, executeAt); err != nil {
		t.Fatal(err)
	}
	if err := rTimeWheel.AddTask(ctx, "test2", &RTaskElement{
		Msg:  "world",
		Type: "test",
	}, executeAt); err != nil {
		t.Fatal(err)
	}
	if err := rTimeWheel.RemoveTask(ctx, "test2", executeAt); err != nil {
		t.Fatal(err)
	}

	// 分钟分片应有两个信封 member，score 为毫秒
	members := zrangeAll(t, driver, pendingKey(minute))
	if len(members) != 2 {
		t.Fatalf("pending members: got %d, want 2", len(members))
	}
	for _, m := range members {
		env, err := decodeEnvelope(m)
		if err != nil {
			t.Fatalf("decodeEnvelope(%q): %v", m, err)
		}
		if env.Attempt != 0 || env.ClaimID != "" {
			t.Errorf("fresh task should have attempt=0 and empty claimID, got %+v", env)
		}
		if _, err := decodeTaskPayload(env.Payload); err != nil {
			t.Errorf("payload should be valid task JSON: %v", err)
		}
	}

	// registry 应登记了该分钟
	minutes := zrangeAll(t, driver, registryKey)
	found := false
	for _, m := range minutes {
		if m == minute {
			found = true
		}
	}
	if !found {
		t.Errorf("registry should contain minute %q, got %v", minute, minutes)
	}

	// 删除集合应有 test2 标记
	marked, err := driver.Eval(ctx, "return redis.call('smembers', KEYS[1])",
		[]string{delSetKey(minute)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	markedKeys := gocast.ToStringSlice(marked)
	if len(markedKeys) != 1 || markedKeys[0] != "test2" {
		t.Errorf("delete set should contain [test2], got %v", markedKeys)
	}
}

// zrangeAll 经通用脚本读取 zset 全量 member（测试辅助）
func zrangeAll(t *testing.T, store redis.Store, key string) []string {
	t.Helper()
	reply, err := store.Eval(context.Background(), redis.ZRangeByScoreLuaScript,
		[]string{key}, []interface{}{"-inf", "+inf", 0, 1000})
	if err != nil {
		t.Fatalf("zrangeAll(%s): %v", key, err)
	}
	return gocast.ToStringSlice(reply)
}

func handle(ctx context.Context, task *RTaskElement) error {
	slog.With("TimeWheel", "test").Info("task", slog.Any("RTaskElement", task))
	return nil
}
