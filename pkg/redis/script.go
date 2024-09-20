package redis

import (
	_ "embed"
)

var (
	// 注意 '//go:embed' 之间不能有空格

	// 添加任务 lua 脚本
	//go:embed lua/add_task.lua
	AddTaskLuaScript string

	// 删除任务 lua 脚本
	//go:embed lua/delete_task.lua
	DeleteTaskLuaScript string

	// 取任务 lua 脚本
	//go:embed lua/range_tasks.lua
	RangeTasksLuaScript string
)
