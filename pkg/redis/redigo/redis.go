package redigo

import (
	"context"
	"time"

	store "github.com/dej4vu/timewheel/pkg/redis"
	"github.com/gomodule/redigo/redis"
)

var _ store.Store = (*Client)(nil)

// Client Redis 客户端.
type Client struct {
	opts *store.ClientOptions
	pool *redis.Pool
}

func NewClient(network, address, password string, opts ...store.ClientOption) *Client {
	c := Client{
		opts: &store.ClientOptions{
			Network:  network,
			Address:  address,
			Password: password,
		},
	}

	for _, opt := range opts {
		opt(c.opts)
	}

	store.RepairClient(c.opts)

	pool := c.getRedisPool()
	return &Client{
		pool: pool,
	}
}

func (c *Client) getRedisPool() *redis.Pool {
	return &redis.Pool{
		MaxIdle:     c.opts.MaxIdle,
		IdleTimeout: time.Duration(c.opts.IdleTimeoutSeconds) * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := c.getRedisConn()
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		MaxActive: c.opts.MaxActive,
		Wait:      c.opts.Wait,
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
}

func (c *Client) GetConn(ctx context.Context) (redis.Conn, error) {
	return c.pool.GetContext(ctx)
}

func (c *Client) getRedisConn() (redis.Conn, error) {
	if c.opts.Address == "" {
		panic("Cannot get redis address from config")
	}

	var dialOpts []redis.DialOption
	if len(c.opts.Password) > 0 {
		dialOpts = append(dialOpts, redis.DialPassword(c.opts.Password))
	}
	conn, err := redis.DialContext(context.Background(),
		c.opts.Network, c.opts.Address, dialOpts...)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *Client) SAdd(ctx context.Context, key, val string) (int, error) {
	conn, err := c.pool.GetContext(ctx)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	return redis.Int(conn.Do("SADD", key, val))
}

// Eval 支持使用 lua 脚本.
func (c *Client) Eval(ctx context.Context, src string, keys []string, args []interface{}) (interface{}, error) {
	rargs := make([]interface{}, 2, 2+len(keys)+len(args))
	rargs[0] = src
	rargs[1] = len(keys)
	for _, k := range keys {
		rargs = append(rargs, k)
	}
	rargs = append(rargs, args...)

	conn, err := c.pool.GetContext(ctx)
	if err != nil {
		return -1, err
	}
	defer conn.Close()

	return conn.Do("EVAL", rargs...)
}
