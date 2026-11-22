-- 统计快照
-- KEYS: [1]processing [2]dead [3]当前分钟pending分片
-- 返回: [processing深度, dead深度, pending深度]
return {
    redis.call('zcard', KEYS[1]),
    redis.call('zcard', KEYS[2]),
    redis.call('zcard', KEYS[3])
}
