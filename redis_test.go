package timewheel

import (
	"context"
	"net"

	"testing"
	"time"

	"github.com/demdxx/gocast"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/redis/goredis"
)

const (
	// redis 服务器信息
	testNetwork = "tcp"
	testAddress = "localhost:6379"
)

// testStore 返回 goredis store；Redis 不可达时 skip 而非失败
func testStore(t *testing.T) redis.Store {
	t.Helper()
	conn, err := net.DialTimeout("tcp", testAddress, 500*time.Millisecond)
	if err != nil {
		t.Skipf("redis not reachable at %s: %v", testAddress, err)
	}
	conn.Close()

	return goredis.NewClient(testNetwork, testAddress, "")
}

// testScanStepLua 测试自持的单步 SCAN 脚本（返回 [新游标, 分钟1, 分钟2, ...]）。
// 生产代码已删除 legacy 分片发现，测试隔离仍需发现遗留分片，故内联在此。
const testScanStepLua = `
local res = redis.call('scan', ARGV[1], 'MATCH', 'timewheel_task_*', 'COUNT', 500)
local out = { res[1] }
for _, k in ipairs(res[2]) do
    local minute = string.match(k, 'timewheel_task_%{(.*)%}')
    if minute then
        out[#out + 1] = minute
    end
end
return out
`

// flushTimewheelKeys 清理全局 key 与全部分片 key（测试隔离用）。
// 通过 SCAN 发现遗留分片，避免上一次运行/中断留下的旧任务污染断言。
func flushTimewheelKeys(t *testing.T, store redis.Store, extraKeys ...string) {
	t.Helper()
	ctx := context.Background()

	keys := append([]string{registryKey, processingKey, deadKey}, extraKeys...)
	cursor := "0"
	for {
		reply, err := store.Eval(ctx, testScanStepLua, nil, []interface{}{cursor})
		if err != nil {
			t.Fatalf("scan shards: %v", err)
		}
		items := gocast.ToInterfaceSlice(reply)
		if len(items) == 0 {
			t.Fatalf("invalid scan reply: %v", reply)
		}
		next := gocast.ToString(items[0])
		for _, minute := range items[1:] {
			keys = append(keys, pendingKey(gocast.ToString(minute)), delSetKey(gocast.ToString(minute)))
		}
		if next == "0" {
			break
		}
		cursor = next
	}

	for i := 0; i < len(keys); i += 500 {
		end := i + 500
		if end > len(keys) {
			end = len(keys)
		}
		if _, err := store.Eval(ctx, redis.DeleteKeysLuaScript, keys[i:end], nil); err != nil {
			t.Fatalf("flush keys %v: %v", keys[i:end], err)
		}
	}
}

// eventually 在 timeout 内每 interval 轮询一次 cond，返回是否满足
func eventually(t *testing.T, timeout, interval time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(interval)
	}
	return cond()
}
