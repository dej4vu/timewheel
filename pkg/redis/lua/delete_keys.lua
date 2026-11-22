-- 删除指定 keys（测试与运维用）
return redis.call('del', unpack(KEYS))
