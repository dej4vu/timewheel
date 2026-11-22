package timewheel

import (
	"context"
	"fmt"
	"time"
)

// RTimeWheelOptions 时间轮配置。字段私有，通过 With* 选项设置，经 newRTimeWheelOptions 校验。
type RTimeWheelOptions struct {
	tickInterval     time.Duration
	claimBatch       int
	maxSlicesPerTick int
	maxConcurrency   int
	handlerTimeout   time.Duration
	lease            time.Duration
	maxAttempts      int
	backoffBase      time.Duration
	backoffMax       time.Duration
	deadRetention    time.Duration
	statsInterval    time.Duration
	resultHook       func(context.Context, TaskResult)
	statsHook        func(context.Context, Stats)
}

// RTimeWheelOption 时间轮配置选项
type RTimeWheelOption func(*RTimeWheelOptions)

// WithTickInterval 扫描间隔，默认 1s
func WithTickInterval(d time.Duration) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.tickInterval = d }
}

// WithClaimBatch 单次领取上限，默认 2000
func WithClaimBatch(n int) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.claimBatch = n }
}

// WithMaxSlicesPerTick 单个 tick 最多处理的分钟分片数，默认 5
func WithMaxSlicesPerTick(n int) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.maxSlicesPerTick = n }
}

// WithMaxConcurrency 在途 handler 并发上限，默认 500
func WithMaxConcurrency(n int) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.maxConcurrency = n }
}

// WithHandlerTimeout 单笔 handler 的执行超时，默认 30s
func WithHandlerTimeout(d time.Duration) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.handlerTimeout = d }
}

// WithLease 任务租约时长，默认 60s。必须大于 HandlerTimeout。
func WithLease(d time.Duration) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.lease = d }
}

// WithMaxAttempts 含首次执行在内的最大尝试次数，默认 10；设为 1 关闭重试
func WithMaxAttempts(n int) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.maxAttempts = n }
}

// WithBackoff 指数退避参数：base 默认 1s，max 默认 1m
func WithBackoff(base, max time.Duration) RTimeWheelOption {
	return func(o *RTimeWheelOptions) {
		o.backoffBase = base
		o.backoffMax = max
	}
}

// WithDeadRetention 死信保留时长，默认 7 天
func WithDeadRetention(d time.Duration) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.deadRetention = d }
}

// WithStatsInterval 统计快照间隔，默认 10s
func WithStatsInterval(d time.Duration) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.statsInterval = d }
}

// WithResultHook 设置单笔任务结果钩子
func WithResultHook(hook func(context.Context, TaskResult)) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.resultHook = hook }
}

// WithStatsHook 设置周期统计钩子
func WithStatsHook(hook func(context.Context, Stats)) RTimeWheelOption {
	return func(o *RTimeWheelOptions) { o.statsHook = hook }
}

// newRTimeWheelOptions 构造并校验配置
func newRTimeWheelOptions(opts ...RTimeWheelOption) (*RTimeWheelOptions, error) {
	o := &RTimeWheelOptions{
		tickInterval:     time.Second,
		claimBatch:       2000,
		maxSlicesPerTick: 5,
		maxConcurrency:   500,
		handlerTimeout:   30 * time.Second,
		lease:            60 * time.Second,
		maxAttempts:      10,
		backoffBase:      time.Second,
		backoffMax:       time.Minute,
		deadRetention:    7 * 24 * time.Hour,
		statsInterval:    10 * time.Second,
	}
	for _, opt := range opts {
		opt(o)
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *RTimeWheelOptions) validate() error {
	if o.tickInterval <= 0 {
		return fmt.Errorf("timewheel: tickInterval must be positive, got %v", o.tickInterval)
	}
	if o.claimBatch < 1 {
		return fmt.Errorf("timewheel: claimBatch must be >= 1, got %d", o.claimBatch)
	}
	if o.maxSlicesPerTick < 1 {
		return fmt.Errorf("timewheel: maxSlicesPerTick must be >= 1, got %d", o.maxSlicesPerTick)
	}
	if o.maxConcurrency < 1 {
		return fmt.Errorf("timewheel: maxConcurrency must be >= 1, got %d", o.maxConcurrency)
	}
	if o.handlerTimeout <= 0 {
		return fmt.Errorf("timewheel: handlerTimeout must be positive, got %v", o.handlerTimeout)
	}
	// 租约必须覆盖 handler 执行窗口，否则租约期内防重入承诺失效
	if o.lease <= o.handlerTimeout {
		return fmt.Errorf("timewheel: lease (%v) must be greater than handlerTimeout (%v)", o.lease, o.handlerTimeout)
	}
	if o.maxAttempts < 1 {
		return fmt.Errorf("timewheel: maxAttempts must be >= 1, got %d", o.maxAttempts)
	}
	if o.backoffBase <= 0 || o.backoffMax <= 0 {
		return fmt.Errorf("timewheel: backoff durations must be positive, got base %v max %v", o.backoffBase, o.backoffMax)
	}
	if o.backoffBase > o.backoffMax {
		return fmt.Errorf("timewheel: backoffBase (%v) must not exceed backoffMax (%v)", o.backoffBase, o.backoffMax)
	}
	if o.deadRetention <= 0 {
		return fmt.Errorf("timewheel: deadRetention must be positive, got %v", o.deadRetention)
	}
	if o.statsInterval <= 0 {
		return fmt.Errorf("timewheel: statsInterval must be positive, got %v", o.statsInterval)
	}
	return nil
}
