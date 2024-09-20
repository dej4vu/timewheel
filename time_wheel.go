package timewheel

import (
	"container/list"
	"log/slog"
	"sync"
	"time"
)

// taskElement 封装了一笔定时任务的明细信息
type taskElement struct {
	// 任务执行函数
	task func()

	// 定时任务挂载在环状数组中的索引位置
	pos int

	// 定时任务的延迟轮次. 指的是 curSlot 指针还要扫描过环状数组多少轮，才满足执行该任务的条件
	cycle int

	// 定时任务的唯一标识键
	key string
}

// TimeWheel 时间轮
type TimeWheel struct {
	// 单例工具，保证时间轮停止操作只能执行一次
	sync.Once

	// 时间轮运行时间间隔
	interval time.Duration

	// 时间轮定时器
	ticker *time.Ticker

	// 停止时间轮的 channel
	stopc chan struct{}

	// 新增定时任务的入口 channel
	addTaskCh chan *taskElement

	// 删除定时任务的入口 channel
	removeTaskCh chan string

	// 通过 list 组成的环状数组. 通过遍历环状数组的方式实现时间轮
	// 定时任务数量较大，每个 slot 槽内可能存在多个定时任务，因此通过 list 进行组装
	slots []*list.List

	// 当前遍历到的环状数组的索引
	curSlot int

	// 定时任务 key 到任务节点的映射，便于在 list 中删除任务节点
	keyToETask map[string]*list.Element
}

// NewTimeWheel 新建时间轮
// slotNum 环状数组长度
// interval 轮询时间间隔
func NewTimeWheel(slotNum int, interval time.Duration) *TimeWheel {
	// 环状数组长度默认为 10
	if slotNum <= 0 {
		slotNum = 10
	}

	// 扫描时间间隔默认为 1 秒
	if interval <= 0 {
		interval = time.Second
	}

	t := TimeWheel{
		interval:     interval,
		ticker:       time.NewTicker(interval),
		stopc:        make(chan struct{}),
		keyToETask:   make(map[string]*list.Element),
		slots:        make([]*list.List, 0, slotNum),
		addTaskCh:    make(chan *taskElement),
		removeTaskCh: make(chan string),
	}

	// 初始化数据槽
	for i := 0; i < slotNum; i++ {
		t.slots = append(t.slots, list.New())
	}

	// 异步启动时间轮常驻 goroutine
	go t.run()
	return &t
}

// Stop 停止时间轮
func (t *TimeWheel) Stop() {
	t.Do(func() {
		t.ticker.Stop()
		close(t.stopc)
	})
}

// AddTask 添加任务到时间轮
func (t *TimeWheel) AddTask(key string, task func(), executeAt time.Time) {
	pos, cycle := t.getPosAndCircle(executeAt)
	t.addTaskCh <- &taskElement{
		pos:   pos,
		cycle: cycle,
		task:  task,
		key:   key,
	}
}

// RemoveTask 从时间轮移除任务
func (t *TimeWheel) RemoveTask(key string) {
	t.removeTaskCh <- key
}

// 运行时间轮
func (t *TimeWheel) run() {
	defer func() {
		if err := recover(); err != nil {
			// ...
			slog.Error("[TimeWheel] 执行错误: %v", err)
		}
	}()

	// 通过 for + select 的代码结构运行一个常驻 goroutine 是常规操作
	for {
		select {
		// 停止时间轮
		case <-t.stopc:
			return
		// 接收到定时信号
		case <-t.ticker.C:
			// 批量执行定时任务
			t.tick()
		// 接收创建定时任务的信号
		case task := <-t.addTaskCh:
			t.addTask(task)
		// 接收到删除定时任务的信号
		case removeKey := <-t.removeTaskCh:
			t.removeTask(removeKey)
		}
	}
}

func (t *TimeWheel) tick() {
	list := t.slots[t.curSlot]
	defer t.circularIncr()
	t.execute(list)
}

func (t *TimeWheel) execute(l *list.List) {
	// 遍历每个 list
	for e := l.Front(); e != nil; {
		taskElement, _ := e.Value.(*taskElement)
		if taskElement.cycle > 0 {
			taskElement.cycle--
			e = e.Next()
			continue
		}

		// 执行任务
		go func() {
			defer func() {
				if err := recover(); err != nil {
					// ...
				}
			}()
			taskElement.task()
		}()

		// 执行任务后，从时间轮中删除
		next := e.Next()
		l.Remove(e)
		delete(t.keyToETask, taskElement.key)
		e = next
	}
}

func (t *TimeWheel) getPosAndCircle(executeAt time.Time) (int, int) {
	delay := int(time.Until(executeAt))
	cycle := delay / (len(t.slots) * int(t.interval))
	pos := (t.curSlot + delay/int(t.interval)) % len(t.slots)
	return pos, cycle
}

func (t *TimeWheel) addTask(task *taskElement) {
	list := t.slots[task.pos]
	if _, ok := t.keyToETask[task.key]; ok {
		t.removeTask(task.key)
	}
	eTask := list.PushBack(task)
	t.keyToETask[task.key] = eTask
}

func (t *TimeWheel) removeTask(key string) {
	eTask, ok := t.keyToETask[key]
	if !ok {
		return
	}
	delete(t.keyToETask, key)
	task, _ := eTask.Value.(*taskElement)
	_ = t.slots[task.pos].Remove(eTask)
}

// circularIncr 向前移动指针
func (t *TimeWheel) circularIncr() {
	t.curSlot = (t.curSlot + 1) % len(t.slots)
}
