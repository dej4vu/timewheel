package redis

const (
	// 默认连接池超过 10 s 释放连接
	DefaultIdleTimeoutSeconds = 10
	// 默认最大激活连接数
	DefaultMaxActive = 100
	// 默认最大空闲连接数
	DefaultMaxIdle = 20
)

type ClientOptions struct {
	MaxIdle            int
	IdleTimeoutSeconds int
	MaxActive          int
	Wait               bool
	DB                 int
	// 必填参数
	Network  string
	Address  string
	Password string
}

type ClientOption func(c *ClientOptions)

func WithMaxIdle(maxIdle int) ClientOption {
	return func(c *ClientOptions) {
		c.MaxIdle = maxIdle
	}
}

func WithIdleTimeoutSeconds(idleTimeoutSeconds int) ClientOption {
	return func(c *ClientOptions) {
		c.IdleTimeoutSeconds = idleTimeoutSeconds
	}
}

func WithMaxActive(maxActive int) ClientOption {
	return func(c *ClientOptions) {
		c.MaxActive = maxActive
	}
}

func WithWaitMode() ClientOption {
	return func(c *ClientOptions) {
		c.Wait = true
	}
}

func WithDB(db int) ClientOption {
	return func(c *ClientOptions) {
		c.DB = db
	}
}

func RepairClient(c *ClientOptions) {
	if c.MaxIdle < 0 {
		c.MaxIdle = DefaultMaxIdle
	}

	if c.IdleTimeoutSeconds < 0 {
		c.IdleTimeoutSeconds = DefaultIdleTimeoutSeconds
	}

	if c.MaxActive < 0 {
		c.MaxActive = DefaultMaxActive
	}
}
