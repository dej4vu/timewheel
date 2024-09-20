package redis

import "context"

// Store 表示 redis 存储接口
type Store interface {
	// redis sadd 命令格式：SADD key member [member ...]
	SAdd(ctx context.Context, key, val string) (int, error)

	// redis EVAL 命令格式： EVAL script numkeys [key [key ...]] [arg [arg ...]]
	// numkeys 参数通过len(keys) 自动计算
	Eval(ctx context.Context, src string, keys []string, args []interface{}) (interface{}, error)
}
