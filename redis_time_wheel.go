package timewheel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dej4vu/timewheel/pkg/redis"
	"github.com/dej4vu/timewheel/pkg/util"
	"github.com/demdxx/gocast"
)

var log = slog.Default().With("TimeWheel", "core")

// RTaskElement 任务明细
type RTaskElement struct {
	// 任务 key
	Key string `json:"key"`
	// 任务内容
	Msg string `json:"msg"`
	// 任务类型
	Type string `json:"type"`
	// 执行时间
	ExecuteAtUnix int64 `json:"executeAtUnix"`
}

// NewRTaskElement 创建新任务
func NewRTaskElement(msg string, _type string) *RTaskElement {
	return &RTaskElement{
		Msg:  msg,
		Type: _type,
	}
}

// RTimeWheel redis实现的分布式时间轮
type RTimeWheel struct {
	// 内置的单例工具，用于保证 stopc 只被关闭一次
	sync.Once
	// 任务处理函数
	handle func(context.Context, *RTaskElement) error
	// 用于停止时间轮的控制器 channel
	stopc chan struct{}
	// 触发定时扫描任务的定时器
	ticker *time.Ticker
	// redis存储接口
	store redis.Store
}

// NewRTimeWheel 构造 redis 实现的分布式时间轮
func NewRTimeWheel(store redis.Store, handle func(context.Context, *RTaskElement) error) *RTimeWheel {
	r := &RTimeWheel{
		ticker: time.NewTicker(time.Second),
		stopc:  make(chan struct{}),
		handle: handle,
		store:  store,
	}

	go r.run()
	return r
}

// Stop 停止时间轮
func (r *RTimeWheel) Stop() {
	r.Do(func() {
		close(r.stopc)
		r.ticker.Stop()
	})
}

// AddTask 添加定时任务
func (r *RTimeWheel) AddTask(ctx context.Context, key string, task *RTaskElement, executeAt time.Time) error {
	if err := r.addTaskPrecheck(task); err != nil {
		return err
	}

	task.Key = key
	task.ExecuteAtUnix = executeAt.Unix()
	taskBody, _ := json.Marshal(task)
	_, err := r.store.Eval(ctx, redis.AddTaskLuaScript,
		[]string{
			// 分钟级 zset 时间片
			r.getMinuteSlice(executeAt),
			// 标识任务删除的集合
			r.getDeleteSetKey(executeAt),
		},
		[]interface{}{
			// 以执行时刻的秒级时间戳作为 zset 中的 score
			executeAt.Unix(),
			// 任务明细
			string(taskBody),
			// 任务 key，用于存放在删除集合中
			key,
		})
	return err
}

// RemoveTask 从 redis 时间轮中删除一个定时任务
func (r *RTimeWheel) RemoveTask(ctx context.Context, key string, executeAt time.Time) error {
	//定时任务距离当前时间的秒数+3600s
	ttl := int(time.Until(executeAt).Seconds()) + 3600

	// 标识任务已被删除
	_, err := r.store.Eval(ctx, redis.DeleteTaskLuaScript,
		[]string{r.getDeleteSetKey(executeAt)},
		[]interface{}{key, ttl},
	)
	return err
}

func (r *RTimeWheel) run() {
	for {
		select {
		case <-r.stopc:
			return
		case <-r.ticker.C:
			// 每次 tick 获取任务
			go r.executeTasks()
		}
	}
}

func (r *RTimeWheel) executeTasks() {
	defer func() {
		if err := recover(); err != nil {
			log.Error("recover from err", err)
		}
	}()

	// 并发控制，保证 30 s 之内完成该批次全量任务的执行，及时回收 goroutine，避免发生 goroutine 泄漏
	tctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	tasks, err := r.getExecutableTasks(tctx)
	if err != nil {
		log.Error("get executable tasks", slog.Any("error", err))
		return
	}

	// 并发执行任务
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		// shadow
		task := task
		go func() {
			defer func() {
				if err := recover(); err != nil {
					log.Error("recover from err", err)
				}
				wg.Done()
			}()
			if err := r.executeTask(tctx, task); err != nil {
				log.Error("executeTask err", err.Error(), slog.Any("task key", task.Key))
			}
		}()
	}
	wg.Wait()
}

func (r *RTimeWheel) executeTask(ctx context.Context, task *RTaskElement) error {
	return r.handle(ctx, task)
}

func (r *RTimeWheel) addTaskPrecheck(task *RTaskElement) error {
	if task.Msg == "" || task.Type == "" {
		return fmt.Errorf("msg:%s, type:%s should not by empty", task.Msg, task.Type)
	}
	return nil
}

func (r *RTimeWheel) getExecutableTasks(ctx context.Context) ([]*RTaskElement, error) {
	now := time.Now()
	// 根据当前时间，推算出其从属的分钟级时间片
	minuteSlice := r.getMinuteSlice(now)
	// 推算出其对应的分钟级已删除任务集合
	deleteSetKey := r.getDeleteSetKey(now)
	// 以秒级时间戳作为 score 进行 zset 检索
	nowSecond := util.GetTimeSecond(now)
	score1 := nowSecond.Unix()
	score2 := nowSecond.Add(time.Second).Unix()
	// 执行 lua 脚本，本质上是通过 zrange 指令结合秒级时间戳对应的 score 进行定时任务检索
	rawReply, err := r.store.Eval(ctx, redis.RangeTasksLuaScript,
		[]string{minuteSlice, deleteSetKey},
		[]interface{}{score1, score2},
	)
	if err != nil {
		return nil, err
	}

	// 结果中，首个元素对应为已删除任务的 key 集合，后续元素对应为各笔定时任务
	replies := gocast.ToInterfaceSlice(rawReply)
	if len(replies) == 0 {
		return nil, fmt.Errorf("invalid replies: %v", replies)
	}

	deleteds := gocast.ToStringSlice(replies[0])
	deletedSet := make(map[string]struct{}, len(deleteds))
	for _, deleted := range deleteds {
		deletedSet[deleted] = struct{}{}
	}

	// 遍历各笔定时任务，倘若其存在于删除集合中，则跳过，否则追加到 list 中返回，用于后续执行
	tasks := make([]*RTaskElement, 0, len(replies)-1)
	for i := 1; i < len(replies); i++ {
		var task RTaskElement
		if err := json.Unmarshal([]byte(gocast.ToString(replies[i])), &task); err != nil {
			// log
			log.Error("unmarshal task err", err.Error(), slog.Any("raw task", replies[i]))
		}

		if _, ok := deletedSet[task.Key]; ok {
			continue
		}
		tasks = append(tasks, &task)
	}

	return tasks, nil
}

// 获取定时任务有序表 key 的方法
func (r *RTimeWheel) getMinuteSlice(executeAt time.Time) string {
	return fmt.Sprintf("timewheel_task_{%s}", util.GetTimeMinuteStr(executeAt))
}

// 获取删除任务集合 key 的方法
func (r *RTimeWheel) getDeleteSetKey(executeAt time.Time) string {
	return fmt.Sprintf("timewheel_delset_{%s}", util.GetTimeMinuteStr(executeAt))
}
