package timewheel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/util"
)

var log = slog.Default().With("TimeWheel", "core")

// redis key 常量
const (
	registryKey   = "timewheel_registry"
	processingKey = "timewheel_processing"
	deadKey       = "timewheel_dead"
)

// pendingKey 返回分钟分片 key
func pendingKey(minute string) string {
	return fmt.Sprintf("timewheel_task_{%s}", minute)
}

// delSetKey 返回分钟删除集合 key
func delSetKey(minute string) string {
	return fmt.Sprintf("timewheel_delset_{%s}", minute)
}

// RTimeWheel redis实现的分布式时间轮
type RTimeWheel struct {
	// 内置的单例工具，用于保证 stopc 只被关闭一次
	sync.Once
	// 任务处理函数
	handle func(context.Context, *RTaskElement) error
	// 用于停止时间轮的控制器 channel
	stopc chan struct{}
	// 触发定时扫描任务的定时器
	ticker *time.Ticker
	// redis存储接口
	store redis.Store
}

// NewRTimeWheel 构造 redis 实现的分布式时间轮
func NewRTimeWheel(store redis.Store, handle func(context.Context, *RTaskElement) error) *RTimeWheel {
	r := &RTimeWheel{
		ticker: time.NewTicker(time.Second),
		stopc:  make(chan struct{}),
		handle: handle,
		store:  store,
	}

	go r.run()
	return r
}

// Stop 停止时间轮
func (r *RTimeWheel) Stop() {
	r.Do(func() {
		close(r.stopc)
		r.ticker.Stop()
	})
}

// AddTask 添加定时任务
func (r *RTimeWheel) AddTask(ctx context.Context, key string, task *RTaskElement, executeAt time.Time) error {
	if err := addTaskPrecheck(key, task); err != nil {
		return err
	}

	// 拷贝后填充，不修改调用方对象
	copied := *task
	copied.Key = key
	copied.ExecuteAtUnix = executeAt.Unix()
	taskBody, err := json.Marshal(copied)
	if err != nil {
		return fmt.Errorf("failed to marshal task: %w", err)
	}

	minute := util.GetTimeMinuteStr(executeAt)
	_, err = r.store.Eval(ctx, redis.AddTaskLuaScript,
		[]string{pendingKey(minute), delSetKey(minute), registryKey},
		[]interface{}{
			// 以执行时刻的毫秒时间戳作为 zset score
			executeAt.UnixMilli(),
			// 信封 member
			encodeEnvelope(key, 0, "", taskBody),
			// 任务 key，用于清除删除集合中的标记
			key,
			// 分钟起始毫秒与分钟 member，用于登记 registry
			fmt.Sprintf("%d", util.MinuteStartMs(executeAt)),
			minute,
		})
	if err != nil {
		return fmt.Errorf("failed to add task: %w", err)
	}
	return nil
}

// RemoveTask 从 redis 时间轮中删除一个定时任务
func (r *RTimeWheel) RemoveTask(ctx context.Context, key string, executeAt time.Time) error {
	//定时任务距离当前时间的秒数+3600s
	ttl := int(time.Until(executeAt).Seconds()) + 3600

	// 标识任务已被删除
	_, err := r.store.Eval(ctx, redis.DeleteTaskLuaScript,
		[]string{delSetKey(util.GetTimeMinuteStr(executeAt))},
		[]interface{}{key, ttl},
	)
	return err
}

func (r *RTimeWheel) run() {
	for {
		select {
		case <-r.stopc:
			return
		case <-r.ticker.C:
			// tick 引擎（领取/结算/回收）在引擎重写任务中接入
		}
	}
}

func addTaskPrecheck(key string, task *RTaskElement) error {
	if key == "" {
		return fmt.Errorf("task key should not be empty")
	}
	if task.Msg == "" || task.Type == "" {
		return fmt.Errorf("msg:%s, type:%s should not be empty", task.Msg, task.Type)
	}
	return nil
}
