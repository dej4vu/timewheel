-- 删除任务
-- 获取标识删除任务的 set 集合的 key
local deleteSetKey = KEYS[1]
-- 获取定时任务的唯一键
local taskKey = ARGV[1]
-- 获取定时任务距离当前时间的秒数
local ttl = ARGV[2]
-- 将定时任务唯一键添加到 set 中
redis.call('sadd', deleteSetKey, taskKey)
local scnt = redis.call('scard', deleteSetKey)
-- 倘若是 set 中的首个元素，则对 set 设置过期时间，该时间为定时任务执行时间+3600s
if (tonumber(scnt) == 1) then
    redis.call('expire', deleteSetKey, ttl)
end
return scnt
