package goredis

import (
	"context"
	"time"

	store "github.com/dej4vu/timewheel/pkg/redis"
	"github.com/redis/go-redis/v9"
)

var _ store.Store = (*Client)(nil)

// Client Redis 客户端.
type Client struct {
	opts *store.ClientOptions
	rdb  *redis.Client
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

	rdb := c.getCliet()
	return &Client{
		rdb: rdb,
	}
}

func (c *Client) getCliet() *redis.Client {
	opt := &redis.Options{
		Network:         c.opts.Network,
		Addr:            c.opts.Address,
		Password:        c.opts.Password,
		MaxIdleConns:    c.opts.MaxIdle,
		MaxActiveConns:  c.opts.MaxActive,
		ConnMaxIdleTime: time.Duration(c.opts.IdleTimeoutSeconds),
		OnConnect: func(ctx context.Context, cn *redis.Conn) error {
			err := cn.Ping(ctx).Err()
			return err
		},
	}

	return redis.NewClient(opt)
}

func (c *Client) SAdd(ctx context.Context, key, val string) (int, error) {
	resp, err := c.rdb.SAdd(ctx, key, val).Result()
	return int(resp), err
}

// Eval 支持使用 lua 脚本.
func (c *Client) Eval(ctx context.Context, src string, keys []string, args []interface{}) (interface{}, error) {
	resp, err := c.rdb.Eval(ctx, src, keys, args...).Result()
	return resp, err
}
