# timewheel
<b>timewheel: 纯 golang 实现的时间轮框架</b><br/><br/>

## 核心能力
<b>基于 golang time ticker + 环形数组实现了单机版时间轮工具</b><br/><br/>
代码主要来源于<a href="https://github.com/xiaoxuxiansheng/timewheel">timewheel</a><br/><br/>
<b>基于 golang time ticker + redis zset 实现了分布式版时间轮工具</b><br/><br/>
参考<a href="https://github.com/xiaoxuxiansheng/timewheel">timewheel</a>，修改任务的执行方式，增加多 redis 客户端的实现

## 使用示例
使用单测示例代码如下. 参见 ./time_wheel_test.go 文件
- 单机版时间轮
```go
func Test_timeWheel(t *testing.T) {
	timeWheel := NewTimeWheel(10, 500*time.Millisecond)
	defer timeWheel.Stop()

	timeWheel.AddTask("test1", func() {
		t.Errorf("test1, %v", time.Now())
	}, time.Now().Add(time.Second))
	timeWheel.AddTask("test2", func() {
		t.Errorf("test2, %v", time.Now())
	}, time.Now().Add(5*time.Second))
	timeWheel.AddTask("test2", func() {
		t.Errorf("test2, %v", time.Now())
	}, time.Now().Add(3*time.Second))

	<-time.After(6 * time.Second)
}
```

- redis版时间轮
```go
const (
	// redis 服务器信息
	network  = "tcp"
	address  = "localhost:6379"
	password = ""
)

// 任务处理函数
func handle(ctx context.Context, task *RTaskElement) error {
	log.Printf("get task: %v\n", task)
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
		Key:  "key1",
		Msg:  util.GetTimeMinuteStr(time.Now()),
		Type: "test",
	}, time.Now().Add(time.Second)); err != nil {
		t.Error(err)
		return
	}

	if err := rTimeWheel.AddTask(ctx, "test2", &RTaskElement{
		Key:  "key2",
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
		Key:  "key3",
		Msg:  util.GetTimeMinuteStr(time.Now()),
		Type: "test",
	}, time.Now().Add(5*time.Second)); err != nil {
		t.Error(err)
		return
	}

	<-time.After(6 * time.Second)
	t.Log("ok")
}


```