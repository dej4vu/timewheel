-- ACK：校验 member 仍在 processing 后删除（防止旧消费者误删新租约）
-- KEYS: [1]processing
-- ARGV: [1]信封member
-- 返回: 1 删除成功 / 0 不存在（已被回收改写，no-op）
local processingKey = KEYS[1]
local member = ARGV[1]
if redis.call('zscore', processingKey, member) then
    redis.call('zrem', processingKey, member)
    return 1
end
return 0
