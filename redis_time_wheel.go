package timewheel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
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

// RTimeWheel redis 实现的分布式时间轮（at-least-once）。
//
// 语义：任务不早于 executeAt 执行，最晚延迟一个 tick；handler 失败按指数退避重试，
// 达到最大尝试次数进入死信；进程崩溃后由租约回收重投。消费端必须按任务 key 幂等。
type RTimeWheel struct {
	// stopOnce 保证停止操作只执行一次
	stopOnce sync.Once
	// 任务处理函数
	handle func(context.Context, *RTaskElement) error
	// 配置
	opts *RTimeWheelOptions
	// redis 脚本调用层
	scripts *redisScripts
	// 扫描定时器
	ticker *time.Ticker
	// 停止信号
	stopc chan struct{}
	// run goroutine 退出信号
	runDone chan struct{}
	// 在途 executeAndSettle 的等待组
	wg sync.WaitGroup
	// 在途 handler 计数（并发领取配额）
	inflight atomic.Int64
	// 消费者标识（日志与批次号）
	consumerID string
	// 批次序号
	batchSeq atomic.Int64
}

// NewRTimeWheel 构造 redis 实现的分布式时间轮（默认配置即可靠模式）
func NewRTimeWheel(store redis.Store, handle func(context.Context, *RTaskElement) error) *RTimeWheel {
	r, err := NewRTimeWheelWithOptions(store, handle)
	if err != nil {
		// 默认配置恒通过校验，此处仅为防御
		panic(fmt.Sprintf("timewheel: invalid default options: %v", err))
	}
	return r
}

// NewRTimeWheelWithOptions 构造可配置的分布式时间轮
func NewRTimeWheelWithOptions(store redis.Store, handle func(context.Context, *RTaskElement) error, opts ...RTimeWheelOption) (*RTimeWheel, error) {
	o, err := newRTimeWheelOptions(opts...)
	if err != nil {
		return nil, err
	}
	r := &RTimeWheel{
		handle:     handle,
		opts:       o,
		scripts:    newRedisScripts(store),
		ticker:     time.NewTicker(o.tickInterval),
		stopc:      make(chan struct{}),
		runDone:    make(chan struct{}),
		consumerID: defaultConsumerID(),
	}
	go r.run()
	return r, nil
}

// Stop 停止扫描并立即返回，不等待在途任务（在途任务由租约回收保护）
func (r *RTimeWheel) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopc)
		r.ticker.Stop()
	})
}

// Shutdown 优雅停止：停止领取，等待在途任务结算完成。
// ctx 超时返回 ctx.Err()，未完成任务留在 processing 由租约回收。
func (r *RTimeWheel) Shutdown(ctx context.Context) error {
	r.Stop()

	// 先等 run goroutine 退出，再等在途任务：避免 tick 尚未 wg.Add 时
	// wg.Wait 抢先观察到零计数而提前返回（Add 与 Wait 并发的经典误用）
	select {
	case <-r.runDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	settled := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(settled)
	}()
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

	minuteStr, minuteStartMs := minuteOf(executeAt)
	return r.scripts.addTask(ctx, executeAt.UnixMilli(),
		encodeEnvelope(key, 0, "", taskBody), key, minuteStr, minuteStartMs)
}

// RemoveTask 取消一个定时任务。
// 对未被领取的任务完全生效；已领取（执行中）的任务仍会执行一次，失败后不再重试。
func (r *RTimeWheel) RemoveTask(ctx context.Context, key string, executeAt time.Time) error {
	// 定时任务距离当前时间的秒数+3600s
	ttl := int(time.Until(executeAt).Seconds()) + 3600

	// 标识任务已被删除
	_, err := r.store().Eval(ctx, redis.DeleteTaskLuaScript,
		[]string{delSetKey(util.GetTimeMinuteStr(executeAt))},
		[]interface{}{key, ttl},
	)
	return err
}

func (r *RTimeWheel) store() redis.Store {
	return r.scripts.store
}

func (r *RTimeWheel) run() {
	defer close(r.runDone)
	for {
		select {
		case <-r.stopc:
			return
		case <-r.ticker.C:
			r.tick()
		}
	}
}

// tick 单次扫描：回收过期租约 -> 按 registry 领取各分钟分片到期任务。
// tick 在 run goroutine 内同步执行，天然不叠加。
// registry 由 AddTask/recover 在同一 Lua 内原子登记，是分片发现的唯一入口。
func (r *RTimeWheel) tick() {
	ctx := context.Background()
	now := time.Now()
	minuteStr, minuteStartMs := minuteOf(now)

	if err := r.scripts.recover(ctx, minuteStr, minuteStartMs,
		r.opts.claimBatch, r.opts.maxAttempts,
		r.opts.backoffBase, r.opts.backoffMax, r.opts.deadRetention); err != nil {
		log.Error("recover leases", slog.Any("error", err))
	}

	avail := r.opts.maxConcurrency - int(r.inflight.Load())
	if avail > 0 {
		minutes, err := r.scripts.listMinutes(ctx, now.UnixMilli(), r.opts.maxSlicesPerTick)
		if err != nil {
			log.Error("list minutes", slog.Any("error", err))
		}
		batchID := r.nextBatchID()
		for _, minute := range minutes {
			if avail <= 0 {
				break
			}
			claimed, err := r.scripts.claim(ctx, minute,
				min(r.opts.claimBatch, avail), r.opts.lease, batchID)
			if err != nil {
				log.Error("claim tasks", slog.Any("error", err), slog.Any("minute", minute))
				break
			}
			for _, member := range claimed {
				r.wg.Add(1)
				r.inflight.Add(1)
				go r.executeAndSettle(member)
			}
			avail -= len(claimed)
		}
	}
}

// executeAndSettle 执行单笔任务并按结果结算（ACK / 租约延期 / 死信）
func (r *RTimeWheel) executeAndSettle(member string) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("executeAndSettle panic", slog.Any("panic", p))
		}
		r.inflight.Add(-1)
		r.wg.Done()
	}()

	start := time.Now()
	env, err := decodeEnvelope(member)
	if err != nil {
		// claim 产出的信封必然合法，此处为防御路径
		r.settleDead(context.Background(), member, env, err, start)
		return
	}
	task, err := decodeTaskPayload(env.Payload)
	if err != nil {
		r.settleDead(context.Background(), member, env, err, start)
		return
	}

	hctx, cancel := context.WithTimeout(context.Background(), r.opts.handlerTimeout)
	defer cancel()
	handlerStart := time.Now()
	herr := r.safeHandle(hctx, task)
	duration := time.Since(handlerStart)

	// 结算用独立 ctx，不受 handler 超时影响
	sctx := context.Background()
	switch {
	case herr == nil:
		r.settleAck(sctx, member, env, task, duration)
	case env.Attempt >= r.opts.maxAttempts:
		if _, derr := r.scripts.dead(sctx, member, r.opts.deadRetention); derr != nil {
			log.Error("dead-letter task", slog.Any("error", derr), slog.Any("task key", task.Key))
		}
		r.emitResult(task, env.Attempt, OutcomeDeadLettered, herr, duration)
	default:
		applied, deleted, ferr := r.scripts.fail(sctx, member,
			r.backoffOf(env.Attempt), util.GetTimeMinuteStr(time.Unix(task.ExecuteAtUnix, 0)))
		if ferr != nil {
			// 结算失败：任务留在 processing，由租约回收兜底
			log.Error("fail settle", slog.Any("error", ferr), slog.Any("task key", task.Key))
		}
		switch {
		case deleted:
			r.emitResult(task, env.Attempt, OutcomeRemoved, herr, duration)
		case applied:
			r.emitResult(task, env.Attempt, OutcomeRetryScheduled, herr, duration)
		}
	}
}

// settleAck ACK 成功路径，含网络错误重试
func (r *RTimeWheel) settleAck(ctx context.Context, member string, env envelope, task *RTaskElement, duration time.Duration) {
	ok := false
	var err error
	for retry := 0; retry < 3 && !ok; retry++ {
		ok, err = r.scripts.ack(ctx, member)
		if err == nil {
			break
		}
		log.Error("ack retry", slog.Any("error", err), slog.Any("task key", task.Key))
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !ok {
		// ACK 未送达：任务留 processing，由租约回收重投（at-least-once）
		r.emitResult(task, env.Attempt, OutcomeAckFailed, err, duration)
		return
	}
	r.emitResult(task, env.Attempt, OutcomeSuccess, nil, duration)
}

func (r *RTimeWheel) settleDead(ctx context.Context, member string, env envelope, cause error, start time.Time) {
	if _, err := r.scripts.dead(ctx, member, r.opts.deadRetention); err != nil {
		log.Error("dead-letter malformed task", slog.Any("error", err))
	}
	r.emitResult(nil, env.Attempt, OutcomeDeadLettered, cause, time.Since(start))
}

// safeHandle 执行 handler，panic 转为错误（计为失败参与重试）
func (r *RTimeWheel) safeHandle(ctx context.Context, task *RTaskElement) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return r.handle(ctx, task)
}

// backoffOf 计算第 attempt 次失败后的退避：base*2^(attempt-1) 封顶 max，附 ±20% 抖动
func (r *RTimeWheel) backoffOf(attempt int) time.Duration {
	shift := uint(attempt - 1)
	if attempt < 1 {
		shift = 0
	}
	d := r.opts.backoffBase * (1 << shift)
	if d > r.opts.backoffMax || d <= 0 {
		d = r.opts.backoffMax
	}
	jitter := 0.8 + 0.4*rand.Float64() // [0.8, 1.2)
	return time.Duration(float64(d) * jitter)
}

// nextBatchID 生成本次 tick 的批次号（嵌入 claimID）
func (r *RTimeWheel) nextBatchID() string {
	return fmt.Sprintf("%s-%d-%d", r.consumerID, time.Now().UnixNano(), r.batchSeq.Add(1))
}

func (r *RTimeWheel) emitResult(task *RTaskElement, attempt int, outcome Outcome, err error, duration time.Duration) {
	if r.opts.resultHook == nil {
		return
	}
	safeResultHook(context.Background(), r.opts.resultHook,
		TaskResult{Task: task, Attempt: attempt, Outcome: outcome, Err: err, Duration: duration})
}

func safeResultHook(ctx context.Context, hook func(context.Context, TaskResult), r TaskResult) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("result hook panic", slog.Any("panic", p))
		}
	}()
	hook(ctx, r)
}

// defaultConsumerID 生成消费者标识：hostname-pid
func defaultConsumerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
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
