package main

import (
	"context"
	"log/slog"
	"math/rand"
	"time"

	"flag"
	"fmt"
	"github.com/dej4vu/timewheel"
	"github.com/dej4vu/timewheel/pkg/redis/goredis"
)

var (
	clientId = flag.String("clientId", "client1", "client id")
	count    = flag.Int64("count", 10000000, "执行次数")
)

const (
	// redis 服务器信息
	network  = "tcp"
	address  = "localhost:6379"
	password = ""
)

// 任务处理函数
func handle(ctx context.Context, task *timewheel.RTaskElement) error {
	at := time.Unix(task.ExecuteAtUnix, 0).Format(time.DateTime)
	slog.Info("get task", slog.Any("key", task.Key), slog.Any("executeAt", at))
	return nil
}

func main() {
	flag.Parse()
	client := goredis.NewClient(network, address, password)

	rTimeWheel := timewheel.NewRTimeWheel(client, handle)

	ctx := context.Background()

	ticker := time.Tick(1 * time.Millisecond)

	var i int64
	for {
		select {
		case <-ticker:
			i++
			if i <= *count {
				i := i
				go addTask(ctx, i, rTimeWheel)
			}
		}
	}

}

func addTask(ctx context.Context, i int64, tw *timewheel.RTimeWheel) {
	key := fmt.Sprintf("key_%s_%d", *clientId, i)
	msg := fmt.Sprintf("simple message from timewheel %d", i)
	rnd := rand.Intn(3600) + 1
	if err := tw.AddTask(ctx, key,
		timewheel.NewRTaskElement(msg, "test"),
		time.Now().Add(time.Duration(rnd)*time.Second)); err != nil {
		slog.Error("add task err", slog.Any("error", err))
		return
	}
	slog.Info("add task", slog.Any("key", key))
}
