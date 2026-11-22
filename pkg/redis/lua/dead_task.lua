-- 死信：校验存在后从 processing 移入 dead，刷新保留期
-- KEYS: [1]processing [2]dead
-- ARGV: [1]信封member [2]retentionSeconds
-- 返回: 1 入死信 / 0 不存在（no-op）
local processingKey = KEYS[1]
local deadKey = KEYS[2]
local member = ARGV[1]
local retentionSeconds = tonumber(ARGV[2])

if not redis.call('zscore', processingKey, member) then
    return 0
end

redis.call('zrem', processingKey, member)
local t = redis.call('TIME')
local nowMs = t[1] * 1000 + math.floor(t[2] / 1000)
redis.call('zadd', deadKey, nowMs, member)
redis.call('expire', deadKey, retentionSeconds)
return 1
