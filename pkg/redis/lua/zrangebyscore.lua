-- 通用：按 score 升序返回 member 列表（registry 扫描用）
-- KEYS: [1]目标key
-- ARGV: [1]min [2]max [3]offset [4]count
return redis.call('zrangebyscore', KEYS[1], ARGV[1], ARGV[2], 'LIMIT', ARGV[3], ARGV[4])
