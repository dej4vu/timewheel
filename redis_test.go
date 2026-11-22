package timewheel

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/redis/goredis"
	"github.com/dej4vu/timewheel/pkg/redis/redigo"
)

const (
	// redis 服务器信息
	testNetwork = "tcp"
	testAddress = "localhost:6379"
)

// testStores 返回双驱动 store 列表；Redis 不可达时 skip 而非失败
func testStores(t *testing.T) []redis.Store {
	t.Helper()
	conn, err := net.DialTimeout("tcp", testAddress, 500*time.Millisecond)
	if err != nil {
		t.Skipf("redis not reachable at %s: %v", testAddress, err)
	}
	conn.Close()

	return []redis.Store{
		goredis.NewClient(testNetwork, testAddress, ""),
		redigo.NewClient(testNetwork, testAddress, ""),
	}
}

// driverName 返回 store 的驱动名（测试子用例命名用）
func driverName(store redis.Store) string {
	return fmt.Sprintf("%T", store)
}

// flushTimewheelKeys 清理全局 key 与全部分片 key（测试隔离用）。
// 通过 SCAN 发现遗留分片，避免上一次运行/中断留下的旧任务污染断言。
func flushTimewheelKeys(t *testing.T, store redis.Store, extraKeys ...string) {
	t.Helper()
	ctx := context.Background()
	s := newRedisScripts(store)

	keys := append([]string{registryKey, processingKey, deadKey}, extraKeys...)
	cursor := "0"
	for {
		next, minutes, err := s.scanStep(ctx, cursor)
		if err != nil {
			t.Fatalf("scan shards: %v", err)
		}
		for _, minute := range minutes {
			keys = append(keys, pendingKey(minute), delSetKey(minute))
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
