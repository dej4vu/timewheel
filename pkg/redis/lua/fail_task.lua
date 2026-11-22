-- 失败结算：校验存在后租约延期（member 不变，重试统一走租约到期路径）
-- 若任务已被 RemoveTask 标记删除，则直接移除不再重试
-- KEYS: [1]processing [2]删除集合
-- ARGV: [1]信封member [2]backoffMs
-- 返回: 1 已延期 / 2 已删除移除 / 0 不存在（no-op）
local processingKey = KEYS[1]
local deleteSetKey = KEYS[2]
local member = ARGV[1]
local backoffMs = tonumber(ARGV[2])
local sep = "\31"

if not redis.call('zscore', processingKey, member) then
    return 0
end

-- 从信封第二段提取业务 key（信封格式: v1|key|attempt|claimID|json）
local key = nil
if string.sub(member, 1, 1) ~= '{' then
    local pos1 = string.find(member, sep, 1, true)
    if pos1 then
        local pos2 = string.find(member, sep, pos1 + 1, true)
        if pos2 then
            key = string.sub(member, pos1 + 1, pos2 - 1)
        end
    end
end

if key ~= nil and redis.call('sismember', deleteSetKey, key) == 1 then
    redis.call('zrem', processingKey, member)
    return 2
end

local t = redis.call('TIME')
local nowMs = t[1] * 1000 + math.floor(t[2] / 1000)
redis.call('zadd', processingKey, nowMs + backoffMs, member)
return 1
