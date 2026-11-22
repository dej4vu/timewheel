-- 原子领取分钟分片中到期任务：过滤删除标记，写入 processing 租约
-- KEYS: [1]pending分片 [2]删除集合 [3]processing [4]registry [5]dead
-- ARGV: [1]limit [2]leaseMs [3]batchUUID [4]分钟member
-- 返回: 领取到的新信封数组（Go 端解析后执行）
-- 说明: 分隔符为 0x1f，Redis 内置 Lua 5.1 不支持十六进制字符转义，写作十进制 "\31"
local pendingKey = KEYS[1]
local deleteSetKey = KEYS[2]
local processingKey = KEYS[3]
local registryKey = KEYS[4]
local deadKey = KEYS[5]
local limit = tonumber(ARGV[1])
local leaseMs = tonumber(ARGV[2])
local batchUUID = ARGV[3]
local minuteMember = ARGV[4]
local sep = "\31"

local t = redis.call('TIME')
local nowMs = t[1] * 1000 + math.floor(t[2] / 1000)
local leaseUntil = nowMs + leaseMs

local due = redis.call('zrangebyscore', pendingKey, '-inf', nowMs, 'LIMIT', 0, limit)

-- 将信封按分隔符切成 5 段；段数不足返回 nil
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

local claimed = {}
local removed = {}
local malformed = {}

for i, m in ipairs(due) do
    local key = nil
    local attempt = nil
    local payload = nil
    if string.sub(m, 1, 1) == '{' then
        -- 旧格式裸 JSON（平滑升级路径）：解码提取业务 key
        local ok, j = pcall(cjson.decode, m)
        if ok and type(j) == 'table' and j.key ~= nil then
            key = tostring(j.key)
            attempt = 0
            payload = m
        end
    else
        local seg = split5(m)
        if seg ~= nil and seg[1] == 'v1' then
            local a = tonumber(seg[3])
            if a ~= nil then
                key = seg[2]
                attempt = a
                payload = seg[5]
            end
        end
    end

    if key == nil then
        -- 无法解析的负载：不执行，进死信
        malformed[#malformed + 1] = m
        removed[#removed + 1] = m
    elseif redis.call('sismember', deleteSetKey, key) == 1 then
        -- 已删除：直接出队
        removed[#removed + 1] = m
    else
        claimed[#claimed + 1] = table.concat(
            { 'v1', key, tostring(attempt + 1), batchUUID .. ':' .. i, payload }, sep)
        removed[#removed + 1] = m
    end
end

-- 分批提交：Lua 5.1 unpack 栈上限 8000，每批最多 500 元素
local function zremChunks(key, list)
    for i = 1, #list, 500 do
        local batch = {}
        for j = i, math.min(i + 499, #list) do
            batch[#batch + 1] = list[j]
        end
        redis.call('zrem', key, unpack(batch))
    end
end

zremChunks(pendingKey, removed)

-- zadd 参数为 score/member 严格配对，同 score 批量写入需交错展开
for i = 1, #claimed, 500 do
    local batch = {}
    for j = i, math.min(i + 499, #claimed) do
        batch[#batch + 1] = leaseUntil
        batch[#batch + 1] = claimed[j]
    end
    redis.call('zadd', processingKey, unpack(batch))
end

for i = 1, #malformed, 500 do
    local batch = {}
    for j = i, math.min(i + 499, #malformed) do
        batch[#batch + 1] = nowMs
        batch[#batch + 1] = malformed[j]
    end
    redis.call('zadd', deadKey, unpack(batch))
end

-- 分片已无到期任务且已清空时，从 registry 移除该分钟
if #due == 0 and redis.call('zcard', pendingKey) == 0 then
    redis.call('zrem', registryKey, minuteMember)
end

return claimed
