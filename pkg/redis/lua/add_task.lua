-- 添加任务：清除删除标记，写入分钟分片，登记分钟 registry
-- KEYS: [1]pending分片 [2]删除集合 [3]registry
-- ARGV: [1]executeAtMs [2]信封member [3]业务key [4]分钟起始ms [5]分钟member
local zsetKey = KEYS[1]
local deleteSetKey = KEYS[2]
local registryKey = KEYS[3]
local executeAtMs = ARGV[1]
local member = ARGV[2]
local taskKey = ARGV[3]
local minuteStartMs = ARGV[4]
local minuteMember = ARGV[5]

redis.call('srem', deleteSetKey, taskKey)
redis.call('zadd', zsetKey, executeAtMs, member)
redis.call('zadd', registryKey, minuteStartMs, minuteMember)
return 1
