-- 添加任务时，如果存在删除 key 的标识，则将其删除
-- 添加任务时，根据时间（所属的 min）决定数据从属于哪个分片{}
local zsetKey = KEYS[1]
local deleteSetKey = KEYS[2]
local score = ARGV[1]
local task = ARGV[2]
local taskKey = ARGV[3]
redis.call('srem', deleteSetKey, taskKey)
return redis.call('zadd', zsetKey, score, task)
