# Redis 时间轮 At-Least-Once 可靠性改造设计

- 版本：v1.0（2026-11-22）
- 关联：GitHub issue #2《分布式时间轮需要补齐 at-least-once 语义：执行后 ACK、失败重试与租约回收》及其评论（提前 ~1 秒执行的实测 bug）
- 状态：待评审

---

## 1. 背景与问题

### 1.1 现有架构

- 任务按执行时间所在**分钟**分片存放：`timewheel_task_{YYYY-MM-DD-HH:MM}`（zset，member = 任务 JSON，score = `executeAt.Unix()` 秒）。
- 删除走**分钟删除集合**：`timewheel_delset_{...}`（set，TTL = 执行时间 + 3600s），扫描时过滤。
- 每秒 tick：Lua 一次性 `ZRANGE` + `ZREMRANGEBYSCORE` 取出并**删除**当前秒（实际是 `[now, now+1]`）区间任务，进程内并发执行 handler。
- handler 失败仅打日志；`Stop()` 不等待在途任务。

### 1.2 缺陷清单

| # | 缺陷 | 位置 / 说明 |
|---|------|------------|
| 1 | 先删后执行，失败不可恢复 | `range_tasks.lua` 返回前 `ZREMRANGEBYSCORE`，任务已离开 Redis |
| 2 | 提前 ~1 秒执行 | 扫描区间 `[now, now+1]` 为闭区间，score=S 的任务在 S-1 秒即被领取删除（issue 评论实测） |
| 3 | 漏扫即永久丢 | 只扫 `[now, now+1]`，tick 跳过或 Redis 超时后，已过期任务永不再被扫到 |
| 4 | 跨分钟滞留无入口 | 只扫当前分钟分片，上一分钟未执行的任务留在旧分片 |
| 5 | 坏 JSON 被当空任务执行 | `json.Unmarshal` 失败后未 `continue`，零值任务仍交给 handler |
| 6 | tick goroutine 叠加 | 每秒 `go executeTasks()`，上一批未结束继续叠加 |
| 7 | `Stop()` 不 drain | example 需手动 `sleep 30s` 兜底 |
| 8 | 现有代码在 Redis 5.0 上不可用 | `ZRANGE key min max BYSCORE` 为 Redis 6.2+ 语法 |
| 9 | handler panic 不计入失败 | `recover` 后仅打日志，任务状态无差别被删除 |

## 2. 设计目标与约束

### 2.1 目标

1. at-least-once：handler 失败可重试、进程崩溃可恢复（租约）、任务不因扫描/生命周期缺陷丢失。
2. 时间语义正确：**不早于 `executeAt` 执行**，最多晚一个 tick；毫秒精度。
3. 面向大任务量：Redis 命令量与任务数的比值尽可能低，避免大 key，内存单份。
4. 修复 §1.2 全部缺陷。

### 2.2 硬约束

| 约束 | 说明 |
|------|------|
| Redis 基线 5.0 | 依赖 5.0 默认的脚本效果复制（脚本内可用 `TIME`）；**不得使用** `SMISMEMBER`(6.2)、`ZRANGE ... BYSCORE`(6.2)、`ZPOPMIN` 之外新语法；Lua 为 5.1（`unpack` 栈上限 8000） |
| 部署形态 | 单机 / 主从。Cluster 非目标（全局 processing/registry 跨 slot） |
| API 兼容 | `AddTask` / `RemoveTask` / `NewRTimeWheel` 签名不变 |
| 平滑升级 | 沿用现有 pending key 布局与删除集合，**旧存量任务可继续被消费** |

### 2.3 非目标

- 恰好一次（exactly-once）：ACK 失败、租约过期重投会产生重复执行，消费端必须幂等。
- Redis Cluster 分片部署。
- 租约续期（heartbeat）：要求 handler 尊重 ctx 且 P99 < lease；长任务场景后续再议。
- dead letter 重投 API：v1 提供手动重投的 README 说明，不做接口。

## 3. 方案选型

| 方案 | 思路 | 结论 |
|------|------|------|
| A 最小修复 | 只修扫描区间、跨分钟补扫、Stop drain | 不满足 at-least-once，仅过渡 |
| B 全分片状态机 | pending/processing 均按分钟分片 + registry | Cluster 兼容，但四套状态对齐复杂度最高 |
| C 全局单 zset | 放弃分片，member=key + data hash | 代码最简，但丢失分片的压力优势（全局大 key、内存双份、命令量 ~8N/任务），且存量数据不可平滑升级 |
| **D（定案）** | **分片 pending + 分钟 registry + 全局 processing + 保留删除集合** | 继承现有压力设计，批量 claim 命令量与 N 解耦，平滑升级 |

选 D 的决定性依据：本库面向大任务量设计，分钟分片使扫描 zset 体量有界、payload 单份存储、RDB/AOF 无大 key；registry 以每分钟 2-3 条命令的代价同时解决跨分钟滞留与任意时长宕机积压。

## 4. 数据模型

### 4.1 Key 规范

| Key | 类型 | 说明 |
|-----|------|------|
| `timewheel_task_{YYYY-MM-DD-HH:MM}` | zset | pending 分片（**沿用现有**）。member = 信封（§4.2），score = `executeAt.UnixMilli()` |
| `timewheel_registry` | zset | 有待处理任务的分钟。member = 分钟字符串，score = 分钟起始毫秒。**新增** |
| `timewheel_processing` | zset | 全局。member = 信封（含 claimID），score = `leaseUntil`（毫秒）。**新增** |
| `timewheel_dead` | zset | 全局。member = 信封 JSON，score = 入队毫秒。**新增**，TTL = retention（默认 7d，每次写入刷新） |
| `timewheel_delset_{YYYY-MM-DD-HH:MM}` | set | 分钟删除集合（**沿用现有**，含 EXPIRE） |

分片 key 语义从"到期分钟"扩展为"扫描入口"：任务重试时放回**当前分钟**分片、score 设为下次可执行时间（§7.2），到期自然被领取。

### 4.2 信封（member 格式）

```
v1\x1f<key>\x1f<attempt>\x1f<claimID>\x1f<原始任务JSON>
```

- 分隔符 `\x1f`（ASCII unit separator），业务 key 约定不得包含 `\x1f`。
- `attempt`：整数，claim 时 +1；初始写入为 0。
- `claimID`：`<batchUUID>:<i>`，Go 端每 tick 生成 batchUUID 传入；未领取状态为空段。
- 原始任务 JSON 即 `RTaskElement` 序列化（现有格式，不变）。

设计要点：**Lua 只做字符串 split/拼接，不做 cjson decode/encode**。目的：
1. 规避 `cjson.encode` `%.14g` 数字格式化风险；
2. 规避中文 `\uXXXX` 转义导致的 member 体积膨胀（大任务量下直接推高带宽）；
3. claimID 嵌入 member 后，ACK/fail/recover 的防 ABA（防止旧消费者误删新租约）只需完整 member 匹配，无需额外索引。

同 key 重复 `AddTask`：同参数时 member 完全一致，`ZADD` 天然幂等去重；不同 executeAt 时为两条独立任务（与现有行为一致）。重试等待期间重复 AddTask 不与旧信封去重（attempt 不同），文档声明。

### 4.3 状态机

```
AddTask ──> pending(分片)
                │ claim（原子，attempt+1，写租约）
                ▼
            processing（score = leaseUntil）
                │ handler 成功 ── ACK ──> 删除
                │ handler 失败 ── fail ──> 租约延期（score = now+backoff，member 不变）
                │ 进程崩溃 / 不干预
                ▼
            租约到期 ── recover（原子）──> attempt < max ? 放回当前分钟分片(score=now+backoff)
                                          └─ attempt ≥ max ──> dead
```

崩溃恢复与失败重试收敛为**同一条路径**（租约到期回收），状态机只有两条迁移边。任务在整个重试等待期内持有租约，防并发重入窗口 = 退避时长 + 执行时长。

## 5. Lua 脚本设计

共 6 个脚本，全部经现有 `Store.Eval` 调用，`Store` 接口不变。时间基准统一取脚本内 `redis.call('TIME')`（毫秒），避免多节点时钟偏差。

### 5.1 add_task.lua（改）

```lua
-- KEYS: [1]pending分片 [2]删除集合 [3]registry
-- ARGV: [1]executeAtMs [2]信封member [3]业务key [4]分钟起始ms
redis.call('SREM', KEYS[2], ARGV[3])                      -- 清除历史删除标记
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZADD', KEYS[3], ARGV[4], <分钟字符串>)         -- 每次刷新，幂等
-- 3 条命令
```

### 5.2 claim_tasks.lua（核心）

```lua
-- KEYS: [1]pending分片 [2]该分钟删除集合 [3]processing
-- ARGV: [1]limit [2]leaseMs [3]batchUUID
local t = redis.call('TIME')
local nowMs = t[1]*1000 + math.floor(t[2]/1000)
local leaseUntil = nowMs + tonumber(ARGV[2])

-- ① 到期任务
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', nowMs, 'LIMIT', 0, ARGV[1])

-- ② 逐任务：SISMEMBER 过滤删除标记；split 信封取 key/attempt/JSON；拼接新信封(attempt+1, claimID)
local claimed, gone = {}, {}
for i, m in ipairs(due) do
    local seg = split(m, '\x1f')             -- v1|key|attempt|claimID|json
    if redis.call('SISMEMBER', KEYS[2], seg[2]) == 1 then
        gone[#gone+1] = m                    -- 已删除：直接出队
    else
        claimed[#claimed+1] = table.concat({seg[1],seg[2],tostring(tonumber(seg[3])+1),
                                            ARGV[3]..':'..i,seg[5]}, '\x1f')
        gone[#gone+1] = m
    end
end

-- ③ 批量提交（每 500 个一组 unpack，规避 Lua 5.1 栈上限 8000）
for batch in chunks(gone, 500)    do redis.call('ZREM', KEYS[1], unpack(batch)) end
for batch in chunks(claimed, 500) do redis.call('ZADD', KEYS[3], leaseUntil, unpack_flat(batch)) end

return claimed                                   -- Go 端 split 解析后执行
-- 命令数：1 + N(SISMEMBER) + 2·⌈N/500⌉ ≈ 每任务 1 条 + 每批常数条
```

- 多实例并发安全：整脚本原子，两实例不会领到同一任务。
- 空分片检测：`#due == 0` 时返回标记，Go 端对该分钟 `ZREM registry`。
- Redis 5.0 无 `SMISMEMBER`，退化为 Lua 内逐任务 `SISMEMBER`（O(1)，无网络往返）。

### 5.3 ack_task.lua / fail_task.lua

```lua
-- ack: KEYS[1]=processing  ARGV[1]=member
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then
    redis.call('ZREM', KEYS[1], ARGV[1]); return 1
end; return 0                                   -- 已被 recover 改写 → no-op（防 ABA）

-- fail: KEYS[1]=processing  ARGV[1]=member [2]backoffMs
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then
    local t = redis.call('TIME'); local nowMs = ...
    redis.call('ZADD', KEYS[1], nowMs + ARGV[2], ARGV[1])  -- 租约延期，member 不变
    return 1
end; return 0
-- 各 2 条命令/任务
```

fail 不改 member、不写 lastError 进 Redis（仅进 hook 与日志）；`attempt ≥ maxAttempts` 的判断由 Go 端在调 fail 前完成，超限改调 dead 路径（见 §7.2）。

### 5.4 recover_leases.lua

```lua
-- KEYS: [1]processing [2]当前分钟pending分片 [3]dead
-- ARGV: [1]limit [2]baseMs [3]maxMs [4]maxAttempts
local nowMs = ... (TIME)
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', nowMs, 'LIMIT', 0, ARGV[1])

for _, m in ipairs(expired) do
    local seg = split(m, '\x1f')
    local attempt = tonumber(seg[3])
    local backoff = math.min(tonumber(ARGV[2]) * 2^(attempt-1), tonumber(ARGV[3]))  -- + Go 侧抖动已含
    if attempt >= tonumber(ARGV[4]) then
        redis.call('ZADD', KEYS[3], nowMs, m)                  -- 进 dead（保留信封供审计）
        redis.call('EXPIRE', KEYS[3], <retentionSeconds>)
    else
        local m2 = table.concat({seg[1],seg[2],seg[3],'',seg[5]}, '\x1f')  -- 清 claimID
        redis.call('ZADD', KEYS[2], nowMs + backoff, m2)       -- 放回当前分钟分片
    end
    redis.call('ZREM', KEYS[1], m)
end
-- 命令数：1 + 3N（N = 过期租约数，正常为 0；仅故障期产生量）
```

放回的目标是**当前分钟分片**（KEYS 由 Go 传入），score 为下次可执行时间；registry 中当前分钟必然存在（有任务在跑），无需额外注册。恢复后的任务再次被 claim 时 attempt+1，形成完整的执行次数上限。

### 5.5 remove_task.lua（沿用现有 delete_task.lua）

`SADD` 删除集合 + 首元素时 `EXPIRE`，不变。已领取（processing 中）的任务无法按 key 精确定位 member，接受降级语义（§9.3）。

### 5.6 Redis 5.0 兼容要点汇总

1. 扫描一律 `ZRANGEBYSCORE`（修复现有 `ZRANGE...BYSCORE` 的 6.2+ 语法问题）。
2. `SMISMEMBER` 不可用 → 逐任务 `SISMEMBER`。
3. `TIME` 依赖 5.0 默认效果复制；脚本不调用 `replicate_commands()`。
4. 批量 `unpack` 按 500/组，规避 Lua 5.1 栈上限。
5. 不使用 6.x 命令（`ZADD GT/LT`、`OBJECT` 新参数等）。

## 6. 执行引擎

### 6.1 tick 循环（单 goroutine，无叠加）

```go
for {
    select {
    case <-stopc: return
    case <-ticker.C: r.tick()
    }
}

tick():
    if stopping || inflight >= maxConcurrency { /* recover 仍执行 */ }
    recoverLeases(limit)                              // 每 tick，正常为空操作
    minutes := ZRANGEBYSCORE registry -inf nowMs LIMIT 0 maxSlicesPerTick(默认5)
    avail := maxConcurrency - inflight
    batchUUID := uuid()
    for _, m := range minutes {
        if avail <= 0 { break }
        claimed := claim(m, min(claimBatch, avail), leaseMs, batchUUID)
        for _, t := range claimed { wg.Add(1); go executeAndSettle(t) }
        if 分片已空 { ZREM registry m }
        avail -= len(claimed)
    }
    statsSnapshot()                                   // 低频（默认10s）
```

- 修复缺陷 #6：tick 同步执行，ticker 溢出自动丢弃，天然不叠加。
- 修复缺陷 #3/#4：registry 驱动，过期分片持续被扫直到清空；任意时长宕机积压自愈。
- claim 数量受 `min(claimBatch, 可用并发)` 约束，租约不会在排队中白白流逝。

### 6.2 executeAndSettle

```go
func (r *RTimeWheel) executeAndSettle(t ClaimedTask) {
    defer wg.Done()
    ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
    defer cancel()
    err := safeHandle(ctx, t.Task)          // recover panic 计为失败（修复缺陷 #9）
    switch {
    case err == nil:
        ack(t.Member)                        // 失败时重试若干次后放任租约回收
    case t.Attempt >= maxAttempts:
        dead(t.Member, err)
    default:
        fail(t.Member, backoff(t.Attempt))   // attempt+1 在 claim 时已发生
    }
    resultHook(TaskResult{...})              // success / retry / dead / ack_failed
}
```

- handler ctx 与 settle 的 Redis 调用分离：settle 用独立 background ctx，不受 handler 超时影响。
- ACK 失败（网络错误）重试 3 次后放弃 → 任务留 processing，由租约回收重投（at-least-once）。

### 6.3 生命周期

```go
Stop()        // 沿用：关闭 stopc、停 ticker，立即返回，不等待
Shutdown(ctx) // 新增：Stop() + 等待 run goroutine 退出 + 等待在途 executeAndSettle 完成
              // ctx 超时返回 ctx.Err()；未完成任务留在 processing，由租约回收
```

example 与测试改用 `Shutdown`。

### 6.4 时间语义

- score = `executeAt.UnixMilli()`；claim 条件 `score ≤ nowMs`（Redis TIME）。亚秒任务不再被整数秒截断提前执行（修复缺陷 #2）。
- 承诺：**不早于 `executeAt`**，最晚延迟一个 tick（1s）。README 明示。

## 7. 重试与租约

### 7.1 核心规则

| 事件 | 动作 | 在途 handler |
|------|------|--------------|
| handler 成功 | ACK 删除 processing | — |
| handler 失败（attempt < max） | fail：租约延期 `now + backoff(attempt)` | 已结束 |
| handler 失败（attempt ≥ max） | 移入 dead，记录 lastError（hook + 日志） | 已结束 |
| 进程崩溃 / ACK 未达 | 无动作，等租约到期 | 已死 |
| 租约到期 | recover：按 attempt 退避放回 pending，或超限进 dead | 若仍存活则并发重入（见 §7.3） |

### 7.2 退避

- `backoff(attempt) = min(baseMs · 2^(attempt-1), maxMs) + jitter`，base=1s、max=1m、jitter=±20%（Go 端计算，recover 场景由 Lua 按公式取中值）。
- 默认 `maxAttempts=10`：总重试窗口约 5 分钟。`WithMaxAttempts(1)` 关闭重试（退化为一执一投）。

### 7.3 已知边界（文档化，不假装解决）

- handler 不尊重 ctx 且执行超过 lease：租约到期后任务被重投，可能与仍在运行的旧 goroutine 并发执行。缓解：lease 默认 60s > handlerTimeout 30s，构造时校验 `lease > handlerTimeout`；根治需租约续期，非目标。
- 重试等待期间重复 `AddTask` 同 key：产生一条新的 attempt=0 任务，与旧重试任务并存，可能执行两次。

## 8. 可观测性

- `WithResultHook(func(ctx, TaskResult))`：`TaskResult{Task, Attempt, Outcome, Err, Duration}`，Outcome ∈ `success | retry_scheduled | dead_lettered | ack_failed`。
- 深度指标（低频，默认 10s）：processing/dead 深度（ZCARD）、当前分钟 pending 深度、claim lag（本批 `nowMs - executeAtMs` 分布）。经独立 `WithStatsHook` 暴露，不引入 metrics 依赖。
- 日志字段统一：task key、attempt、claimID、consumerID。

## 9. API 与兼容性

### 9.1 变更一览

| API | 变更 |
|-----|------|
| `NewRTimeWheel(store, handle)` | 签名不变，默认即可靠模式 |
| `NewRTimeWheelWithOptions(store, handle, opts...)` | 新增 |
| `AddTask(ctx, key, task, executeAt)` | 签名不变；内部拷贝 task 后填充，不再修改调用方对象 |
| `RemoveTask(ctx, key, executeAt)` | 签名不变；executeAt 仅用于定位删除集合（沿用） |
| `Shutdown(ctx) error` | 新增，优雅停止 |
| `Stop()` | 保留，立即停止（语义见 §6.3） |

默认参数：`tick=1s`、`claimBatch=2000`、`maxSlicesPerTick=5`、`maxConcurrency=500`、`handlerTimeout=30s`、`lease=60s`、`maxAttempts=10`、退避 base=1s/max=1m + 抖动、`deadRetention=7d`。均提供 `With*` 选项。

### 9.2 行为变化（现有调用方需知）

handler 返回 error / panic 不再是"执行完就丢"，而是按退避重试至多 maxAttempts 次。需要旧行为显式 `WithMaxAttempts(1)`。

### 9.3 降级语义（显式接受）

- 已领取（processing 中）的任务被 `RemoveTask` 后：仍会执行这一次；若执行失败，fail 路径检查删除标记（SISMEMBER）后不再重试。
- 高删除率场景（如订单支付后取消关单，删除率 >90%）：删除集合体量大、过滤成本固定，README 建议改用"不删除 + handler 幂等 no-op"模式。

### 9.4 升级说明

- pending 分片 key 与删除集合沿用；存量任务的 member 是裸 JSON（无信封前缀），claim 的 split 逻辑对无前缀 member 兼容处理（识别 `{` 开头，attempt 视为 0）→ **平滑升级，无需 drain**。
- score 精度从秒变毫秒：秒级 score（≤1e10）天然小于毫秒 nowMs，存量任务升级后立即被视为到期并走正常领取路径，不丢失。

## 10. 容量预算（订单超时关闭场景估算）

| 项 | 数值 |
|---|------|
| 稳态管理命令 | 每实例每秒 ~10 条（registry + claim + recover + 深度统计） |
| 吞吐成本 | ≈ 4 条命令/任务（SISMEMBER 1 + claim 摊薄 ~0.003 + ACK/fail 2 + 摊销） |
| 1 万任务/秒 | ≈ 4 万命令/秒 ≈ 40% 单 Redis 容量 → 规划上限 **~2 万任务/秒** |
| 内存 | payload 单份（信封前缀 +30B），分片小 key，RDB/AOF 友好 |
| 扩展位 | ACK/fail pipeline 微批（Store 预留）；registry 二级分片；瓶颈序：Redis CPU > 带宽（信封体积）> Lua 时长 |

## 11. 测试计划（`go test -race`，需本地 Redis，不可达则 skip）

| 场景 | 断言 |
|------|------|
| 基本触发 | 任务在 `[executeAt, executeAt+1s+ε]` 内执行一次；**不早于 executeAt**（回归缺陷 #2） |
| 失败重试 | handler 失败 2 次后成功 → attempt=3，最终 success，退避递增 |
| 超限死信 | 恒定失败 → maxAttempts 后进 dead，lastError 可从 hook 观察 |
| 崩溃恢复 | 直接调 claim 脚本领取后不 settle（模拟崩溃）→ 短租约到期后另一实例重新执行 |
| 租约防重 | 两实例、慢 handler（< lease）→ 恰好执行一次 |
| 优雅停止 | 慢 handler 进行中 `Shutdown(ctx)` → 等待完成后返回；ctx 不足 → 任务留 processing 被回收 |
| 删除语义 | pending 中删除 → 不执行；processing 中删除 → 执行一次且失败不重试 |
| 坏 payload | 手写损坏 member → 进 dead，不执行零值任务（回归缺陷 #5） |
| 多实例竞争 | 并发 claim 同一分片 → 每任务恰好被一个实例领取 |
| 积压恢复 | 构造 3 分钟前的分片任务 → registry 路径自动扫出执行（回归缺陷 #3/#4） |
| tick 不叠加 | 慢 handler + 短 tick → inflight 不超过 maxConcurrency（回归缺陷 #6） |
| 升级兼容 | 写入旧格式裸 JSON member + 秒级 score → 被正常领取执行 |

## 12. 待确认项

1. 降级语义 §9.3（已领取任务 Remove 后仍执行一次）是否接受。
2. 默认参数（§9.1）是否符合预期。
3. `Shutdown(ctx)` + 保留 `Stop()` 的 API 形态是否接受（issue 原文为 `Stop(ctx)`）。

---

## 版本记录

| 日期 | 版本 | 说明 |
|------|------|------|
| 2026-11-22 | v1.0 | 初稿：定案方案 D（分片 pending + registry + 全局 processing + 前缀信封 + 删除集合），Redis 5.0 基线 |
