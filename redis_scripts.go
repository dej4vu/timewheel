package timewheel

import (
	"context"
	"fmt"
	"time"

	"github.com/demdxx/gocast"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/util"
)

// redisScripts 封装时间轮全部 Lua 状态迁移的调用侧。
// 返回值统一经 gocast 解析，抹平 redigo/goredis 的 EVAL 返回结构差异。
type redisScripts struct {
	store redis.Store
}

func newRedisScripts(store redis.Store) *redisScripts {
	return &redisScripts{store: store}
}

// addTask 写入 pending 分片并登记 registry
func (s *redisScripts) addTask(ctx context.Context, executeAtMs int64, member, key, minuteStr string, minuteStartMs int64) error {
	_, err := s.store.Eval(ctx, redis.AddTaskLuaScript,
		[]string{pendingKey(minuteStr), delSetKey(minuteStr), registryKey},
		[]interface{}{executeAtMs, member, key, minuteStartMs, minuteStr})
	if err != nil {
		return fmt.Errorf("failed to add task: %w", err)
	}
	return nil
}

// claim 原子领取 minuteStr 分片中到期任务，返回新信封列表
func (s *redisScripts) claim(ctx context.Context, minuteStr string, limit int, lease time.Duration, batchUUID string) ([]string, error) {
	reply, err := s.store.Eval(ctx, redis.ClaimTasksLuaScript,
		[]string{pendingKey(minuteStr), delSetKey(minuteStr), processingKey, registryKey, deadKey},
		[]interface{}{limit, lease.Milliseconds(), batchUUID, minuteStr})
	if err != nil {
		return nil, fmt.Errorf("failed to claim tasks: %w", err)
	}
	return gocast.ToStringSlice(reply), nil
}

// ack 删除 processing 中的任务记录；member 已被回收改写时返回 false（no-op）
func (s *redisScripts) ack(ctx context.Context, member string) (bool, error) {
	reply, err := s.store.Eval(ctx, redis.AckTaskLuaScript,
		[]string{processingKey}, []interface{}{member})
	if err != nil {
		return false, fmt.Errorf("failed to ack task: %w", err)
	}
	return gocast.ToInt(reply) == 1, nil
}

// fail 失败结算：applied=true 表示已按退避延期租约；deleted=true 表示任务已被删除标记移除
func (s *redisScripts) fail(ctx context.Context, member string, backoff time.Duration, minuteStr string) (applied, deleted bool, err error) {
	reply, err := s.store.Eval(ctx, redis.FailTaskLuaScript,
		[]string{processingKey, delSetKey(minuteStr)},
		[]interface{}{member, backoff.Milliseconds()})
	if err != nil {
		return false, false, fmt.Errorf("failed to fail task: %w", err)
	}
	switch gocast.ToInt(reply) {
	case 1:
		return true, false, nil
	case 2:
		return false, true, nil
	default:
		return false, false, nil
	}
}

// dead 将 processing 中的任务移入死信
func (s *redisScripts) dead(ctx context.Context, member string, retention time.Duration) (bool, error) {
	reply, err := s.store.Eval(ctx, redis.DeadTaskLuaScript,
		[]string{processingKey, deadKey},
		[]interface{}{member, int(retention.Seconds())})
	if err != nil {
		return false, fmt.Errorf("failed to dead-letter task: %w", err)
	}
	return gocast.ToInt(reply) == 1, nil
}

// recover 回收过期租约：超限进死信，否则按退避放回当前分钟分片
func (s *redisScripts) recover(ctx context.Context, curMinuteStr string, curMinuteStartMs int64, limit, maxAttempts int, backoffBase, backoffMax time.Duration, deadRetention time.Duration) error {
	_, err := s.store.Eval(ctx, redis.RecoverLeasesLuaScript,
		[]string{processingKey, pendingKey(curMinuteStr), deadKey, registryKey},
		[]interface{}{
			limit,
			backoffBase.Milliseconds(),
			backoffMax.Milliseconds(),
			maxAttempts,
			curMinuteStartMs,
			curMinuteStr,
			int(deadRetention.Seconds()),
		})
	if err != nil {
		return fmt.Errorf("failed to recover leases: %w", err)
	}
	return nil
}

// listMinutes 返回 registry 中起始时间不超过 nowMs 的分钟列表（升序）
func (s *redisScripts) listMinutes(ctx context.Context, nowMs int64, limit int) ([]string, error) {
	reply, err := s.store.Eval(ctx, redis.ZRangeByScoreLuaScript,
		[]string{registryKey},
		[]interface{}{"-inf", nowMs, 0, limit})
	if err != nil {
		return nil, fmt.Errorf("failed to list minutes: %w", err)
	}
	return gocast.ToStringSlice(reply), nil
}

// deleteKeys 删除指定 key（测试与运维用）
func (s *redisScripts) deleteKeys(ctx context.Context, keys ...string) error {
	_, err := s.store.Eval(ctx, redis.DeleteKeysLuaScript, keys, nil)
	if err != nil {
		return fmt.Errorf("failed to delete keys: %w", err)
	}
	return nil
}

// pushRaw 直接向 zset 写入成员（测试辅助，不走信封逻辑）
func (s *redisScripts) pushRaw(ctx context.Context, key string, score int64, member string) error {
	_, err := s.store.Eval(ctx,
		"return redis.call('zadd', KEYS[1], ARGV[1], ARGV[2])",
		[]string{key}, []interface{}{score, member})
	if err != nil {
		return fmt.Errorf("failed to zadd %s: %w", key, err)
	}
	return nil
}

// minuteOf 从执行时间推导分钟字符串与分钟起始毫秒
func minuteOf(executeAt time.Time) (minuteStr string, minuteStartMs int64) {
	return util.GetTimeMinuteStr(executeAt), util.MinuteStartMs(executeAt)
}
