# timewheel
<b>timewheel: 纯 golang 实现的时间轮框架</b><br/><br/>

## 核心能力
<b>基于 golang time ticker + 环形数组实现了单机版时间轮工具</b><br/><br/>
代码主要来源于<a href="https://github.com/xiaoxuxiansheng/timewheel">timewheel</a><br/><br/>
<b>基于 golang time ticker + redis zset 实现了分布式版时间轮工具（at-least-once）</b><br/><br/>
参考<a href="https://github.com/xiaoxuxiansheng/timewheel">timewheel</a>重构 Redis 版：任务领取/执行/重试/回收全部经 Lua 原子状态机（pending → processing → dead），支持执行后 ACK、失败指数退避重试、进程崩溃后的租约回收与优雅停止。

设计文档参见 `docs/design/2026-11-22-redis时间轮-at-least-once-可靠性设计.md`。

## 可靠性语义（Redis 版）

| 语义 | 说明 |
|------|------|
| 投递语义 | **at-least-once**：ACK 失败、租约过期会产生重复执行，**消费端必须按任务 key 幂等** |
| 时间语义 | 任务**不早于 `executeAt`** 执行，最晚延迟一个 tick（默认 1s）；score 为毫秒精度 |
| 失败重试 | handler 返回 error 或 panic 按指数退避重试（默认 `1s·2^(n-1)` 封顶 1m + 抖动），超过 `maxAttempts`（默认 10）进入死信 |
| 崩溃恢复 | 领取后未结算的任务持有租约（默认 60s），到期由任意实例自动回收重投 |
| 优雅停止 | `Shutdown(ctx)` 停止领取并等待在途任务；超时未完成的由租约回收，不静默丢弃 |
| 删除语义 | `RemoveTask` 对未领取任务完全生效；已领取的仍会执行一次，失败后不再重试 |

### 部署要求

- **Redis ≥ 5.0**（依赖脚本内 `TIME` 与效果复制；不使用 6.2+ 命令）
- 单机或主从部署；Redis Cluster 不在支持范围（全局 processing/registry 跨 slot）

### Redis Key 一览

| Key | 类型 | 说明 |
|-----|------|------|
| `timewheel_task_{YYYY-MM-DD-HH:MM}` | zset | 待执行任务，按执行分钟分片；score=executeAt 毫秒 |
| `timewheel_registry` | zset | 有待处理任务的分钟索引（宕机积压自愈入口） |
| `timewheel_processing` | zset | 已领取/重试等待任务；score=租约到期毫秒 |
| `timewheel_dead` | zset | 死信（默认保留 7 天）；member 含任务与错误上下文 |
| `timewheel_delset_{YYYY-MM-DD-HH:MM}` | set | 分钟删除标记 |

### 删除模式选择建议

- 低删除率：直接 `RemoveTask`（成本 O(1)，扫描时过滤）。
- 高删除率（如订单支付后取消关单，>90% 任务会被删除）：建议**不删除 + handler 幂等 no-op**（执行时检查业务状态直接返回成功），避免删除集合膨胀。

## 使用示例

### 单机版时间轮

参见 `./time_wheel_test.go`：

```go
timeWheel := NewTimeWheel(10, 500*time.Millisecond)
defer timeWheel.Stop()

timeWheel.AddTask("test1", func() { ... }, time.Now().Add(time.Second))
```

### Redis 版时间轮（可靠模式）

```go
client := goredis.NewClient("tcp", "localhost:6379", "")

// handler 必须幂等（按 task.Key 去重）
handle := func(ctx context.Context, task *timewheel.RTaskElement) error {
    return closeOrder(ctx, task.Key) // 返回 error 将按退避自动重试
}

// 默认配置即可靠模式
tw := timewheel.NewRTimeWheel(client, handle)
defer tw.Stop()

err := tw.AddTask(ctx, "order:1001",
    timewheel.NewRTaskElement("订单超时关闭", "order"),
    time.Now().Add(30*time.Minute))
```

### 自定义配置与可观测性

```go
tw, err := timewheel.NewRTimeWheelWithOptions(client, handle,
    timewheel.WithMaxAttempts(10),
    timewheel.WithLease(60*time.Second),            // 必须 > HandlerTimeout
    timewheel.WithHandlerTimeout(30*time.Second),   // handler 应尊重 ctx
    timewheel.WithBackoff(time.Second, time.Minute),
    timewheel.WithMaxConcurrency(500),              // 单实例在途上限
    timewheel.WithResultHook(func(ctx context.Context, r timewheel.TaskResult) {
        // Outcome: success | retry_scheduled | dead_lettered | ack_failed | removed
        metrics.Count(string(r.Outcome), 1)
    }),
)

// 优雅停止：等待在途任务结算
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
if err := tw.Shutdown(shutdownCtx); err != nil {
    log.Warn("shutdown timeout, in-flight tasks will be lease-recovered", "err", err)
}
```

### 默认参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `TickInterval` | 1s | 扫描间隔 |
| `ClaimBatch` | 2000 | 单 tick 领取上限 |
| `MaxSlicesPerTick` | 5 | 单 tick 处理的分钟分片数 |
| `MaxConcurrency` | 500 | 单实例在途 handler 上限 |
| `HandlerTimeout` | 30s | handler 执行超时（handler 应尊重 ctx） |
| `Lease` | 60s | 任务租约，强制大于 HandlerTimeout |
| `MaxAttempts` | 10 | 含首次执行的最大尝试次数；设 1 关闭重试 |
| `Backoff` | 1s / 1m | 指数退避 base/max + ±20% 抖动 |
| `DeadRetention` | 7d | 死信保留时长 |

### 死信处理

死信存放于 `timewheel_dead`（member 为任务信封）。人工重投：

```sh
# 取出最早一条死信（member 含 v1|key|attempt|claimID|任务JSON）
redis-cli ZRANGE timewheel_dead 0 0
# 解析 JSON 后重投（attempt 归零）
redis-cli ZADD "timewheel_task_{当前分钟}" <executeAtMs> "v1\x1f<key>\x1f0\x1f\x1f<任务JSON>"
redis-cli ZADD timewheel_registry <当前分钟起始ms> "<当前分钟>"
```

## 已知边界

- handler 必须尊重 ctx 且 P99 执行时长 < `Lease`；超租约的长任务会被回收重投，可能与仍在执行的旧 goroutine 并发。
- 重试等待期间对同 key 重复 `AddTask` 会产生一条新任务（attempt=0），与旧重试任务并存。
- Redis 版不做租约续期（heartbeat）；长任务场景请调大 `Lease` 或拆分任务。

## 测试

测试分三层，引擎控制流不依赖真实 Redis：

- **纯单测**：信封编解码、配置校验、退避计算；
- **fake Store 引擎测试**：结算分支、panic、ACK 重试、领取限流、Shutdown 顺序——进程内 fake 驱动，确定性、毫秒级；
- **真 Redis 集成**：Lua 语义与端到端行为（执行时机、崩溃恢复、删除、并发领取）。Redis 不可达时自动 skip。

```sh
go test -race ./...   # 需本机 redis-server（默认 localhost:6379）
```

## 多实例压测示例

参见 `./examples/redis`（多节点同时消费同一 Redis）。
