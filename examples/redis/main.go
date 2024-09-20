package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/dej4vu/timewheel"
	"github.com/dej4vu/timewheel/pkg/redis/goredis"
)

const (
	// redis 服务器信息
	network  = "tcp"
	address  = "localhost:6379"
	password = ""
)

// 任务处理函数
func handle(ctx context.Context, task *timewheel.RTaskElement) error {
	slog.With("TimeWheel", "example").Info("task", slog.Any("RTaskElement", task))
	return nil
}

func main() {
	client := goredis.NewClient(network, address, password)

	rTimeWheel := timewheel.NewRTimeWheel(client, handle)

	ctx := context.Background()

	if err := rTimeWheel.AddTask(ctx, "test1", timewheel.NewRTaskElement("msg1", "test"),
		time.Now().Add(time.Second)); err != nil {
		slog.Error("add task err", slog.Any("error", err))
		return
	}

	if err := rTimeWheel.AddTask(ctx, "test1", timewheel.NewRTaskElement("msg1", "test"),
		time.Now().Add(time.Second)); err != nil {
		slog.Error("add task err", slog.Any("error", err))
		return
	}

	//新增任务
	t2 := time.Now().Add(140 * time.Second)

	if err := rTimeWheel.AddTask(ctx, "test2", timewheel.NewRTaskElement("msg2", "test"),
		t2); err != nil {
		slog.Error("add task err", slog.Any("error", err))
		return
	}

	// 删除任务
	rTimeWheel.RemoveTask(ctx, "test2", t2)

	// 新增任务
	if err := rTimeWheel.AddTask(ctx, "test3", timewheel.NewRTaskElement("msg3", "test"),
		time.Now().Add(120*time.Second)); err != nil {
		slog.Error("add task err", slog.Any("error", err))
		return
	}

	// 新增任务
	if err := rTimeWheel.AddTask(ctx, "test4", timewheel.NewRTaskElement("msg4", "test"),
		time.Now().Add(130*time.Second)); err != nil {
		slog.Error("add task err", slog.Any("error", err))
		return
	}

	select {}

}
