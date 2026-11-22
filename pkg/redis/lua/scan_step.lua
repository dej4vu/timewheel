-- 单步 SCAN：渐进扫描 timewheel_task_* 分片 key，返回 [新游标, 分钟1, 分钟2, ...]
-- 由 Go 侧循环驱动（避免长脚本触碰 lua-time-limit）
-- ARGV: [1]游标
local res = redis.call('scan', ARGV[1], 'MATCH', 'timewheel_task_*', 'COUNT', 500)
local out = { res[1] }
for _, k in ipairs(res[2]) do
    local minute = string.match(k, 'timewheel_task_%{(.*)%}')
    if minute then
        out[#out + 1] = minute
    end
end
return out
