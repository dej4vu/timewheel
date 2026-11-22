-- 登记一个非空分钟分片到 registry（score=0，历史滞留优先被扫描）
-- KEYS: [1]pending分片 [2]registry
-- ARGV: [1]分钟member
if redis.call('zcard', KEYS[1]) > 0 then
    redis.call('zadd', KEYS[2], 0, ARGV[1])
    return 1
end
return 0
