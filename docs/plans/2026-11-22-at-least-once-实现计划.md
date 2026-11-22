# Redis 时间轮 At-Least-Once 可靠性改造实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按方案 D 重写 Redis 时间轮：分片 pending + 分钟 registry + 全局 processing + 前缀信封 + 删除集合，实现 at-least-once（ACK、失败重试、租约回收、优雅停止），修复 9 项既有缺陷。

**Architecture:** Lua 脚本承载全部原子状态迁移（add/claim/ack/fail/dead/recover），时间基准统一取脚本内 `redis.call('TIME')` 毫秒；Go 侧为单 goroutine tick 引擎 + 受限并发的 executeAndSettle；信封 `v1\x1f<key>\x1f<attempt>\x1f<claimID>\x1f<JSON>` 让 Lua 只做字符串操作（不走 cjson，中文不膨胀）。

**Tech Stack:** Go 1.23（模块 `github.com/dej4vu/timewheel`），仅既有依赖（go-redis v9 / redigo / gocast / slog），**不新增第三方库**（batchUUID 自造）。

**Spec:** `docs/design/2026-11-22-redis时间轮-at-least-once-可靠性设计.md`（执行者必读，本计划不重复其论证）

## Global Constraints

- Redis 5.0 命令白名单：`ZRANGEBYSCORE`/`ZADD`/`ZREM`/`ZSCORE`/`SADD`/`SREM`/`SISMEMBER`/`SMEMBERS`/`EXPIRE`/`DEL`/`ZCARD`/`TIME`；禁用 `ZRANGE...BYSCORE`(6.2)、`SMISMEMBER`(6.2)。
- Lua 是 5.1：**分隔符在 Lua 内写作 `"\31"`（十进制）**，`\x1f` 十六进制转义是 5.2+ 语法；`unpack` 栈上限 8000，批量命令每 500 元素一组分批。
- 脚本内取时间一律 `redis.call('TIME')`（毫秒 = sec*1000+floor(usec/1000)）；score 均为毫秒 int64。
- Key 常量（redis_scripts.go 定义，全部小写直连，无 hash tag）：`timewheel_registry`、`timewheel_processing`、`timewheel_dead`、`timewheel_task_{YYYY-MM-DD-HH:MM}`、`timewheel_delset_{YYYY-MM-DD-HH:MM}`。
- Store 接口（pkg/redis/store.go）不改，一切经 `Eval`。
- 不变性：`AddTask` 拷贝入参 task 后填充，不修改调用方对象；Go 侧无共享可变状态（inflight 用 atomic）。
- 每文件 <800 行；错误一律 `fmt.Errorf("xxx: %w", err)` 包装；handler panic recover 计为失败。
- 集成测试需本地 Redis，连接失败 `t.Skipf` 而非 Fail；全部测试 `go test -race ./...` 必须通过；gofmt/go vet 干净。

## Review Focus

1. **Lua 5.1 无 `\x` 转义**：脚本里写成 `"\x1f"` 会语法错或分割失败 → Task 3 的脚本真实执行测试（Task 4 脚本级测试首轮即触发）。
2. **`unpack` 栈上限**：claimBatch=2000 时批量 ZADD 4000 个参数爆栈 → Task 4 用 600+ 任务（跨 500 分批边界）claim 验证。
3. **legacy 秒级 score vs 毫秒 now**：升级后存量任务必须立即到期被领取，不得滞留 → Task 6 升级兼容测试。
4. **recover 放回当前分钟分片但 registry 尚无该分钟条目 → 任务永久滞留**：recover 脚本放回时必须顺带 `ZADD registry` → Task 4 recover 测试在空 registry 状态下验证可再领取。
5. **redigo/goredis EVAL 返回结构差异**（嵌套 []interface{} 的数字/字符串形态）：脚本调用层统一 gocast 解析 → Task 4 双驱动跑同一套脚本级断言。

---

### Task 1: 信封与结果类型（纯函数，无 Redis）

**Files:**
- Create: `redis_task.go`
- Test: `redis_task_test.go`

**Interfaces（Produces，后续全部任务依赖）:**
```go
const envelopeSep = "\x1f"        // Go 侧
type envelope struct { Key string; Attempt int; ClaimID string; Payload []byte; Legacy bool }
func encodeEnvelope(key string, attempt int, claimID string, payload []byte) string
func decodeEnvelope(member string) (envelope, error)   // "{"开头→Legacy；split 段数≠5 或首段≠"v1" → error
func decodeTaskPayload(payload []byte) (*RTaskElement, error)
type Outcome string  // "success" | "retry_scheduled" | "dead_lettered" | "ack_failed"
type TaskResult struct { Task *RTaskElement; Attempt int; Outcome Outcome; Err error; Duration time.Duration }
type Stats struct { PendingDepth, ProcessingDepth, DeadDepth, ClaimLagMs int64 }
```
- `RTaskElement` 从 redis_time_wheel.go **原样移入**本文件（字段与 json tag 不变，JSON 格式兼容存量数据）。

- [ ] **Step 1: 写失败测试** `redis_task_test.go`：
  - `TestEnvelopeRoundTrip`：encode→decode 还原 key/attempt/claimID/payload；attempt=0、claimID=""（空段）也还原。
  - `TestEnvelopeKeyWithPipeAndChinese`：key 含 `|`、`/`，payload 含中文 `{"msg":"中文消息"}`——round-trip 无损（证明 \x1f 分隔不受业务字符影响）。
  - `TestDecodeLegacyJSONMember`：`decodeEnvelope(`{"key":"k1","msg":"m","type":"t","executeAtUnix":100}`)` → Legacy=true、Payload 为原文。
  - `TestDecodeMalformed`：段数 4、首段 "v2"、空串 → 返回 error。
  - `TestDecodeTaskPayload`：合法 JSON 还原字段；损坏 JSON 返回 error。
- [ ] **Step 2: 运行确认失败** `go test -race ./... -run TestEnvelope` → 编译错误（函数未定义）。
- [ ] **Step 3: 实现 redis_task.go**（decode 用 `strings.SplitN(member, envelopeSep, 5)`，首段校验 `"v1"`）。
- [ ] **Step 4: 运行通过** `go test -race -run 'TestEnvelope|TestDecode' ./...` → PASS。
- [ ] **Step 5: Commit** `git add redis_task.go redis_task_test.go && git commit -m "feat: add envelope codec and result types for redis timewheel"`

### Task 2: 配置项与校验（纯函数，无 Redis）

**Files:**
- Create: `redis_options.go`
- Test: `redis_options_test.go`

**Interfaces（Produces）:**
```go
type RTimeWheelOptions struct { /* 字段私有，经 With* 设置 */ }
type RTimeWheelOption func(*RTimeWheelOptions)
func newRTimeWheelOptions(opts ...RTimeWheelOption) (*RTimeWheelOptions, error)
// WithTickInterval(1s) WithClaimBatch(2000) WithMaxSlicesPerTick(5) WithMaxConcurrency(500)
// WithHandlerTimeout(30s) WithLease(60s) WithMaxAttempts(10) WithBackoff(base 1s, max 1m)
// WithDeadRetention(7*24h) WithStatsInterval(10s)
// WithResultHook(func(context.Context, TaskResult)) WithStatsHook(func(context.Context, Stats))
```
- 校验规则（违反返回 error）：所有 Duration>0、ClaimBatch/MaxSlicesPerTick/MaxConcurrency/MaxAttempts ≥1、BackoffBase≤BackoffMax、**Lease>HandlerTimeout**。

- [ ] **Step 1: 写失败测试**：默认值逐项断言（spec §9.1 的 11 个默认值）；`WithLease(10*time.Second)` + 默认 handlerTimeout=30s → error；`WithMaxAttempts(0)` → error；合法组合返回 nil。
- [ ] **Step 2: 确认失败** `go test -race -run TestOptions ./...`。
- [ ] **Step 3: 实现**（functional options 模式，参照 pkg/redis/option.go 风格）。
- [ ] **Step 4: 通过**。
- [ ] **Step 5: Commit** `feat: add timewheel options with validation`。

### Task 3: Lua 脚本与 embed

**Files:**
- Rewrite: `pkg/redis/lua/add_task.lua`（+registry ZADD）
- Create: `pkg/redis/lua/claim_tasks.lua`、`ack_task.lua`、`fail_task.lua`、`dead_task.lua`、`recover_leases.lua`、`zrangebyscore.lua`、`stats.lua`、`delete_keys.lua`
- Delete: `pkg/redis/lua/range_tasks.lua`
- Modify: `pkg/redis/script.go`（embed 变量：AddTaskLuaScript、ClaimTasksLuaScript、AckTaskLuaScript、FailTaskLuaScript、DeadTaskLuaScript、RecoverLeasesLuaScript、ZRangeByScoreLuaScript、StatsLuaScript、DeleteKeysLuaScript、DeleteTaskLuaScript；**移除 RangeTasksLuaScript**）
- Test: `redis_time_wheel_test.go`（重写 Test_LuaScript）

**脚本契约（Produces，Task 4/5 依赖；KEYS/ARGV/返回值精确约定）：**

| 脚本 | KEYS | ARGV | 返回 |
|---|---|---|---|
| add_task | pending分片, delset, registry | executeAtMs, member, key, 分钟起始ms | 1 |
| claim_tasks | pending分片, delset, processing, registry | limit, leaseMs, batchUUID | table：新信封数组 |
| ack_task | processing | member | 1 成功 / 0 ABA-noop |
| fail_task | processing, delset | member, backoffMs | 1 延期 / 2 已删除移除 / 0 noop |
| dead_task | processing, dead | member, retentionSeconds | 1 / 0 noop |
| recover_leases | processing, 当前分钟pending, dead, registry | limit, baseMs, maxMs, maxAttempts | 处理条数 |
| zrangebyscore | 目标key | min, max, offset, count | member 数组 |
| stats | processing, dead, 当前分钟pending | — | [ZCARD×3] |
| delete_keys | 任意 keys | — | DEL 返回值 |

**claim_tasks.lua 算法（签名不能决定的算法，完整给出）：**
```lua
local sep = "\31"
-- split(s) -> 5 段（string.find(s, sep, start, true) 循环）
-- nowMs/leaseUntil 由 TIME 计算
local due = ZRANGEBYSCORE pending -inf nowMs LIMIT 0 limit
for i, m in ipairs(due):
    local key, attempt, json
    if string.sub(m, 1, 1) == "{" then               -- legacy 裸 JSON
        local ok, j = pcall(cjson.decode, m)
        if not ok then ZADD dead nowMs m; ZREM-source; continue(malformed→dead)
        key, attempt, json = j.key, 0, m
    else
        local seg = split(m); if #seg ~= 5 or seg[1] ~= "v1" then → malformed→dead
        key, attempt, json = seg[2], tonumber(seg[3]), seg[5]
    end
    if SISMEMBER delset key == 1 then 丢弃（仅移出 pending）
    else 新信封 = "v1"..sep..key..sep..(attempt+1)..sep..batchUUID..":"..i..sep..json
claimed/dropped 每 500 一组：ZREM pending / ZADD processing leaseUntil
if #due == 0 and ZCARD pending == 0 then ZREM registry <分钟member(ARGV[4])>
```
（registry member 即分钟字符串，作为 ARGV[4] 传入）

**recover_leases.lua 关键点：** 过期信封 split 后 attempt≥maxAttempts → `ZADD dead` + `EXPIRE dead retention(ARGV[5])`；否则信封清空 claimID 段后 `ZADD 当前分钟pending nowMs+backoff` 且 **`ZADD registry <当前分钟起始ms(ARGV[6])> <分钟member(ARGV[7])>`**（防 Review Focus #4）；backoff=min(baseMs·2^(attempt-1), maxMs)；最后 ZREM processing。

- [ ] **Step 1: 重写 Test_LuaScript**：全部 embed 变量非空 + `strings.Contains(ClaimTasksLuaScript, "\"31\"")`（锁死十进制转义，防 Review Focus #1）+ 确认无 `"\x"` 字样。
- [ ] **Step 2: 确认失败**（新变量未定义）。
- [ ] **Step 3: 实现 9 个 lua 文件与 script.go**。fail_task 内同样 split 信封取 key 做 SISMEMBER；dead_task 与 ack_task 先 `ZSCORE` 校验存在再操作（防 ABA）。
- [ ] **Step 4: 通过** `go test -race -run TestLuaScript ./...`。
- [ ] **Step 5: Commit** `feat: rewrite lua scripts for at-least-once state machine (redis 5.0 compatible)`。

### Task 4: 脚本调用层 + 脚本级 Redis 测试

**Files:**
- Create: `redis_scripts.go`、`redis_test.go`（测试基建）
- Create: `redis_scripts_test.go`
- Modify: `pkg/util/time.go`（加 `MinuteStartMs(t time.Time) int64`）

**Interfaces:**
```go
// Consumes: Task 1 envelope；Task 3 脚本变量
type redisScripts struct{ store redis.Store }
func newRedisScripts(store redis.Store) *redisScripts
func (s *redisScripts) addTask(ctx, executeAtMs int64, member, key, minuteStr string, minuteStartMs int64) error
func (s *redisScripts) claim(ctx, minuteStr string, limit int, leaseMs int64, batchUUID string, minuteStartMs int64) ([]string, error)
func (s *redisScripts) ack(ctx, member string) (bool, error)
func (s *redisScripts) fail(ctx, member string, backoffMs int64, minuteStr string) (applied, deleted bool, err error)
func (s *redisScripts) dead(ctx, member string, retention time.Duration) (bool, error)
func (s *redisScripts) recover(ctx, curMinuteStr string, curMinuteStartMs int64, limit, maxAttempts int, baseMs, maxMs int64) error
func (s *redisScripts) listMinutes(ctx, nowMs int64, limit int) ([]string, error)
func (s *redisScripts) stats(ctx, curMinuteStr string) (Stats, error)
func (s *redisScripts) deleteKeys(ctx, keys ...string) error   // 测试与运维用
```
- 所有返回解析经 `gocast`（双驱动一致性，Review Focus #5）。

**测试基建（redis_test.go，Produces 给 Task 5/6）：**
```go
func testStores(t *testing.T) []redis.Store          // goredis+redigo；dial 失败 t.Skipf
func flushTimewheelKeys(t, store)                    // deleteKeys(registry/processing/dead + SCAN？否：测试用固定分钟 key 直删)
func eventually(t, timeout, interval time.Duration, cond func() bool) bool
```

- [ ] **Step 1: 写失败测试** `redis_scripts_test.go`（双驱动循环跑同一断言）：
  - `TestScriptAddAndClaim`：addTask 3 笔（1 笔已 SADD 删除）→ claim 返回 2 个新信封；attempt=1、claimID 前缀=batchUUID；被删任务不返回且已移出 pending。
  - `TestClaimBatchOverUnpackLimit`：add 600 笔 → claim limit=600 全部领出（覆盖 500 分批，Review Focus #2）。
  - `TestClaimAtomicAcrossDrivers`：两个 scripts 实例并发各 claim limit=100，总领取恰好 100 不重复。
  - `TestAckAbaNoop`：ack 已存在 member → true；先手动 ZREM 后 ack → false 不报错。
  - `TestFailDelayAndDeleted`：fail 延期后 ZSCORE 为新值、member 不变；SADD 删除标记后 fail → deleted=true 且 processing 中消失。
  - `TestRecoverRequeuesToCurrentMinute`：手写 processing 过期条目，registry 为空 → recover 后 `listMinutes` 能看到当前分钟且再 claim 领出、attempt+1（Review Focus #4）。
  - `TestRecoverDeadOnMaxAttempts`：attempt≥max → 进 dead，processing 清空。
  - `TestClaimLegacyMember`：手写旧格式裸 JSON member（score 用秒）→ claim 领出且信封 attempt=1（Review Focus #3 前半）。
  - `TestClaimMalformedToDead`：信封段数错 / 坏 JSON 裸 member → 进 dead 不执行。
  - `TestClaimEmptySliceRegistryCleanup`：分片任务领完后 registry 对应分钟被 ZREM。
- [ ] **Step 2: 确认失败**（编译错误）。
- [ ] **Step 3: 实现 redis_scripts.go 与 redis_test.go**。分钟字符串用现有 `util.GetTimeMinuteStr`。
- [ ] **Step 4: 通过** `go test -race -run TestScript ./...`（无 Redis 时 SKIP 输出确认）。
- [ ] **Step 5: Commit** `feat: typed lua script layer for timewheel state machine`。

### Task 5: 执行引擎重写 + 基础集成测试

**Files:**
- Rewrite: `redis_time_wheel.go`（保留 `NewRTimeWheel`/`AddTask`/`RemoveTask`/`Stop` 签名；删 getExecutableTasks/executeTasks/executeTask/getMinuteSlice/getDeleteSetKey）
- Test: `redis_time_wheel_test.go`（重写既有用例）

**Interfaces:**
```go
// Consumes: Task 1/2/4 全部
func NewRTimeWheel(store redis.Store, handle func(context.Context, *RTaskElement) error) *RTimeWheel
func NewRTimeWheelWithOptions(store redis.Store, handle ..., opts ...RTimeWheelOption) (*RTimeWheel, error)
func (r *RTimeWheel) Stop()                     // 关 stopc+停 ticker，立即返回
func (r *RTimeWheel) Shutdown(ctx) error        // Stop + 等 runDone + 等 wg；ctx 超时返回 ctx.Err()
func (r *RTimeWheel) AddTask(ctx, key, task, executeAt) error   // 拷贝填充；precheck msg/type/key 非空
func (r *RTimeWheel) RemoveTask(ctx, key, executeAt) error      // delset（沿用 delete_task.lua）
```
- 内部：`run()`（单 goroutine select stopc/ticker.C，tick 同步执行不叠加）；`tick()`（recover → listMinutes → 逐分片 claim min(claimBatch, avail) → go executeAndSettle；batchID=`consumerID-unixnano-递增seq`）；`executeAndSettle(member)`（decode→handlerTimeout ctx→safeHandle→ack/fail/dead 分支→resultHook；ack 网络错误重试 3 次→ack_failed）；`backoffOf(attempt)`=base·2^(attempt-1) cap max ±20% 抖动；每 StatsInterval 调 stats+statsHook。
- 慢路径：decode 信封/JSON 失败 → dead + resultHook(dead_lettered)。

- [ ] **Step 1: 重写集成测试（保留旧测试名以维持示例兼容）**：
  - `Test_RedisTimeWheel_Goredis`（原用例改写）：加 test1/test2(删)/test3 → test1/test3 恰好各执行 1 次、test2 不执行。
  - `Test_NotEarlierThanExecuteAt`：executeAt=now+1.5s（亚秒）→ 执行时刻 ≥ executeAt（毫秒断言，回归缺陷 #2）。
  - `TestRetryWithBackoff`：handler 前 2 次错误后成功 → resultHook 顺序 retry_scheduled×2→success、attempt=3。
  - `TestDeadLetterAfterMaxAttempts`：`WithMaxAttempts(3)` 恒失败 → dead_lettered；dead zset 含该 member。
  - `TestHandlerPanicCountsAsFailure`：panic 一次后成功 → 有 retry_scheduled。
  - `TestRemoveTaskPending`：删除后不执行。
- [ ] **Step 2: 确认失败**。
- [ ] **Step 3: 实现**。测试时间参数用 `WithTickInterval(50ms)/WithLease(300ms)/WithBackoff(50ms,200ms)` 等注入；轮询用 `eventually`。
- [ ] **Step 4: 通过** `go test -race ./...`。
- [ ] **Step 5: Commit** `feat: rewrite redis timewheel engine with at-least-once semantics`。

### Task 6: 可靠性集成测试（生命周期、崩溃、并发、升级）

**Files:**
- Test: `redis_reliability_test.go`

**Consumes:** Task 5 的构造器与 options；Task 4 的 redisScripts（模拟崩溃直调脚本）。

- [ ] **Step 1: 写测试**（goredis 驱动；断言含容差窗口）：
  - `TestCrashRecoveryByLease`：redisScripts.claim 直调（模拟崩溃不 settle）→ 300ms 租约到期后另一 RTimeWheel 重新执行该任务（attempt 递增）。
  - `TestNoDuplicateWithinLease`：两实例同库、handler 慢（200ms<lease）→ 恰好执行 1 次。
  - `TestGracefulShutdownWaits`：慢 handler 进行中 `Shutdown(ctx 2s)` → 等待完成返回 nil、success hook 已触发。
  - `TestShutdownTimeoutLeavesProcessing`：handler 阻塞 800ms、`Shutdown(ctx 100ms)` → 返回 ctx.Err()；租约到期后任务被重新执行（不丢，回归缺陷 #7）。
  - `TestBacklogAcrossMinutes`：任务写入 3 分钟前的旧分片（手写 key）→ 引擎自动扫出执行（回归缺陷 #3/#4）。
  - `TestTickNoStacking`：`WithMaxConcurrency(2)` + 慢 handler + tick 50ms → inflight 峰值 ≤2（回归缺陷 #6）。
  - `TestMultiInstanceExclusiveClaim`：两实例 + 50 笔任务 → 每笔恰好执行 1 次（总执行数=50）。
  - `TestRemoveWhileProcessing`：任务执行中 RemoveTask → 执行 1 次且失败后不重试（spec §9.3 降级语义）。
  - `TestLegacyMemberUpgrade`：手写裸 JSON member（秒级 score，3 秒前）→ 引擎领取执行 attempt=1（回归缺陷 #3 升级路径，Review Focus #3）。
  - `TestMalformedMemberNotExecuted`：坏 member → 不进 handler、进 dead（回归缺陷 #5）。
- [ ] **Step 2: 确认失败**（引擎缺口会在 Backlog/Shutdown 用例暴露）。
- [ ] **Step 3: 修补引擎至全绿**（预计需处理：分钟起始 ms 的时区/取整、eventually 窗口）。
- [ ] **Step 4: 通过** `go test -race ./...`（重复跑 3 次确认无 flake）。
- [ ] **Step 5: Commit** `test: reliability suite for lease recovery, shutdown, concurrency and upgrade`。

### Task 7: 统计、README 与收尾

**Files:**
- Test: `redis_stats_test.go`
- Modify: `README.md`、`examples/redis/main.go`
- Verify: 全仓库

- [ ] **Step 1: 写统计测试**：`WithStatsInterval(100ms)` + 注入任务 → statsHook 收到 Stats{ProcessingDepth≥0, DeadDepth…, ClaimLagMs≥0}；ack 失败路径（对 store 包一层错误注入 fake `brokenStore{redis.Store}` Eval 返回 error）→ resultHook 收到 ack_failed（重试 3 次后）。
- [ ] **Step 2: 确认失败 → 实现缺口（stats 调度已在 Task 5 预埋则此处仅测试）→ 通过**。
- [ ] **Step 3: README 重写**：at-least-once 声明与消费端幂等要求、时间语义（不早于 executeAt、最晚 +1 tick）、Redis ≥5.0 / 单机与主从、key 一览表、重试参数表、删除模式选择建议（高删除率推荐幂等 no-op）、新 API 示例（Shutdown/WithOptions）、升级说明（存量任务自动兼容）。example 改用 `Shutdown`，删除 `sleep 30s`。
- [ ] **Step 4: 全量验证**：`gofmt -l .` 空、`go vet ./...` 干净、`go test -race -count=1 ./...` 全绿；`grep -r "RangeTasks\|zremrangebyscore\|ZRANGE.*byscore" --include='*.lua' --include='*.go' .` 无残留。
- [ ] **Step 5: Commit** `docs: at-least-once semantics, redis 5.0 baseline and upgrade guide`。

---

## 自查记录

- Spec 覆盖：§4 数据模型→T3/T4；§5 六脚本→T3（dead 拆为独立脚本，共 9 lua）；§6 引擎→T5；§7 重试→T4/T5/T6；§8 观测→T7；§9 API→T5/T7；§11 测试 12 场景→T5(4)+T6(10)+T4 脚本级；§9.4 升级→T6 Legacy 用例。
- 类型一致性：envelope/decodeEnvelope（T1）在 T4/T5 消费；redisScripts 方法签名（T4）与 T5/T6 调用一致；Stats/TaskResult 定义仅在 T1。
- 比例：计划 7 任务 ≈ spec §5-§11 的骨架 + 测试断言，未誊写实现体。
