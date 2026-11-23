package redis

import (
	_ "embed"
)

var (
	// 注意 '//go:embed' 之间不能有空格

	// 添加任务 lua 脚本（含 registry 登记）
	//go:embed lua/add_task.lua
	AddTaskLuaScript string

	// 原子领取到期任务 lua 脚本（删除过滤 + processing 租约）
	//go:embed lua/claim_tasks.lua
	ClaimTasksLuaScript string

	// ACK lua 脚本（防 ABA 校验后删除 processing 记录）
	//go:embed lua/ack_task.lua
	AckTaskLuaScript string

	// 失败结算 lua 脚本（租约延期 / 已删除移除）
	//go:embed lua/fail_task.lua
	FailTaskLuaScript string

	// 死信 lua 脚本（processing 移入 dead）
	//go:embed lua/dead_task.lua
	DeadTaskLuaScript string

	// 过期租约回收 lua 脚本（放回 pending 或进 dead）
	//go:embed lua/recover_leases.lua
	RecoverLeasesLuaScript string

	// 通用 zrangebyscore lua 脚本（registry 扫描）
	//go:embed lua/zrangebyscore.lua
	ZRangeByScoreLuaScript string

	// 统计快照 lua 脚本
	//go:embed lua/stats.lua
	StatsLuaScript string

	// 删除指定 keys lua 脚本（测试与运维）
	//go:embed lua/delete_keys.lua
	DeleteKeysLuaScript string

	// 删除任务 lua 脚本（删除集合标记）
	//go:embed lua/delete_task.lua
	DeleteTaskLuaScript string
)
