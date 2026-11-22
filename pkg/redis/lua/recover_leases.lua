-- 回收过期租约：attempt 超限进死信，否则按退避放回当前分钟分片
-- 放回时顺带登记 registry（防止当前分钟分片不在 registry 导致任务滞留）
-- KEYS: [1]processing [2]当前分钟pending分片 [3]dead [4]registry
-- ARGV: [1]limit [2]baseMs [3]maxMs [4]maxAttempts [5]分钟起始ms [6]分钟member [7]retentionSeconds
-- 返回: 本次处理的过期条数
local processingKey = KEYS[1]
local pendingKey = KEYS[2]
local deadKey = KEYS[3]
local registryKey = KEYS[4]
local limit = tonumber(ARGV[1])
local baseMs = tonumber(ARGV[2])
local maxMs = tonumber(ARGV[3])
local maxAttempts = tonumber(ARGV[4])
local minuteStartMs = ARGV[5]
local minuteMember = ARGV[6]
local retentionSeconds = tonumber(ARGV[7])
local sep = "\31"

local t = redis.call('TIME')
local nowMs = t[1] * 1000 + math.floor(t[2] / 1000)

local expired = redis.call('zrangebyscore', processingKey, '-inf', nowMs, 'LIMIT', 0, limit)

local function split5(s)
    local res = {}
    local start = 1
    for i = 1, 4 do
        local pos = string.find(s, sep, start, true)
        if not pos then return nil end
        res[i] = string.sub(s, start, pos - 1)
        start = pos + 1
    end
    res[5] = string.sub(s, start)
    return res
end

for _, m in ipairs(expired) do
    redis.call('zrem', processingKey, m)
    local seg = split5(m)
    local attempt = nil
    if seg ~= nil and seg[1] == 'v1' then
        attempt = tonumber(seg[3])
    end
    if attempt == nil or attempt >= maxAttempts then
        -- 无法解析或超限：进死信
        redis.call('zadd', deadKey, nowMs, m)
        redis.call('expire', deadKey, retentionSeconds)
    else
        local backoff = math.min(baseMs * (2 ^ math.max(attempt - 1, 0)), maxMs)
        -- 清空 claimID 段后放回当前分钟分片，score 为下次可执行时间
        local m2 = table.concat({ seg[1], seg[2], seg[3], '', seg[5] }, sep)
        redis.call('zadd', pendingKey, nowMs + backoff, m2)
        redis.call('zadd', registryKey, minuteStartMs, minuteMember)
    end
end

return #expired
