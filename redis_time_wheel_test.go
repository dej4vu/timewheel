package timewheel

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/redis/goredis"
	"github.com/dej4vu/timewheel/pkg/redis/redigo"
	"github.com/dej4vu/timewheel/pkg/util"
)

func Test_LuaScript(t *testing.T) {
	if redis.AddTaskLuaScript == "" {
		t.Error("addTask shuld not be empty")
	}
	t.Logf("addTask :%v", redis.AddTaskLuaScript)

	if redis.DeleteTaskLuaScript == "" {
		t.Error("deleteTaskLuaScript shuld not be empty")
	}
	t.Logf("deleteTaskLuaScript :%v", redis.DeleteTaskLuaScript)

	if redis.RangeTasksLuaScript == "" {
		t.Error("rangeTaskSLuaScript shuld not be empty")
	}
	t.Logf("rangeTaskSLuaScript :%v", redis.RangeTasksLuaScript)
}

const (
	// redis 服务器信息
	network  = "tcp"
	address  = "localhost:6379"
	password = ""
)

// 任务处理函数
func handle(ctx context.Context, task *RTaskElement) error {
	slog.With("TimeWheel", "test").Info("task", slog.Any("RTaskElement", task))
	return nil
}

func Test_RedisTimeWheel_Redigo(t *testing.T) {
	client := redigo.NewClient(network, address, password)
	run(t, client)
}

func Test_RedisTimeWheel_Goredis(t *testing.T) {
	client := goredis.NewClient(network, address, password)
	run(t, client)
}

func run(t *testing.T, client redis.Store) {
	ctx := context.Background()

	rTimeWheel := NewRTimeWheel(client, handle)

	defer rTimeWheel.Stop()

	if err := rTimeWheel.AddTask(ctx, "test1", &RTaskElement{
		Msg:  util.GetTimeMinuteStr(time.Now()),
		Type: "test",
	}, time.Now().Add(time.Second)); err != nil {
		t.Error(err)
		return
	}

	if err := rTimeWheel.AddTask(ctx, "test2", &RTaskElement{
		Msg:  util.GetTimeMinuteStr(time.Now()),
		Type: "test",
	}, time.Now().Add(4*time.Second)); err != nil {
		t.Error(err)
		return
	}

	if err := rTimeWheel.RemoveTask(ctx, "test2", time.Now().Add(4*time.Second)); err != nil {
		t.Error(err)
		return
	}

	if err := rTimeWheel.AddTask(ctx, "test3", &RTaskElement{
		Msg:  util.GetTimeMinuteStr(time.Now()),
		Type: "test",
	}, time.Now().Add(5*time.Second)); err != nil {
		t.Error(err)
		return
	}

	<-time.After(6 * time.Second)
	t.Log("ok")
}
