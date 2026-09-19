// Package redisx builds a go-redis client from config and verifies connectivity.
package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	// PoolSize 0 = go-redis default (10 per CPU).
	PoolSize int `yaml:"pool_size"`
	// DialTimeout in seconds, default 5.
	DialTimeout int `yaml:"dial_timeout"`
}

// New connects and pings; callers own Close().
func New(ctx context.Context, c Config) (*redis.Client, error) {
	if c.Addr == "" {
		return nil, fmt.Errorf("redisx: addr is required")
	}
	timeout := time.Duration(c.DialTimeout) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	cli := redis.NewClient(&redis.Options{
		Addr:        c.Addr,
		Password:    c.Password,
		DB:          c.DB,
		PoolSize:    c.PoolSize,
		DialTimeout: timeout,
	})
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := cli.Ping(pingCtx).Err(); err != nil {
		cli.Close()
		return nil, fmt.Errorf("redisx: ping %s: %w", c.Addr, err)
	}
	return cli, nil
}
