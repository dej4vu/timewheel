-- 扫描 redis 时间轮. 获取分钟范围内,已删除任务集合 以及在时间上达到执行条件的定时任务进行返回
-- 当聚合类型为空时，会自动被 redis 删除
local zsetKey = KEYS[1]
local deleteSetKey = KEYS[2]
local score1 = ARGV[1]
local score2 = ARGV[2]
local deleteSet = redis.call('smembers', deleteSetKey)
local targets = redis.call('zrange', zsetKey, score1, score2, 'byscore')
redis.call('zremrangebyscore', zsetKey, score1, score2)
local reply = {}
reply[1] = deleteSet
for i, v in ipairs(targets) do
    reply[#reply + 1] = v
end
return reply
