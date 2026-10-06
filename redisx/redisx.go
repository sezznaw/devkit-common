// Package redisx opens the Redis / Valkey connection of a service from its
// `redis:` configuration, the way mysqlx opens MySQL: the framework opens it
// at start-up (kitexx.NewRuntime) and hands it to the service as
// Runtime.Redis; every command is a span of the request's trace. Where the
// server is comes from the configuration (source static) or from the
// platform's datasource table (source platform, kind valkey).
package redisx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
)

// Config is the `redis:` section of a service configuration. Every service
// has it; enabled: false (the default) means the service does not use Redis.
type Config struct {
	// Enabled switches Redis on for this service.
	Enabled bool `yaml:"enabled"`
	// Source: "static", the addr / password / db below; or "platform", the
	// platform's datasource table (kind valkey) for the tenant in
	// TENANT_CODE. Default static.
	Source string `yaml:"source"`
	// Role, with source platform: "tenant" (default) or "platform".
	Role string `yaml:"role"`

	// Addr "host:port", default 127.0.0.1:6379; Password; DB, the database
	// index, default 0: the server, with source static.
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`

	// PoolSize 0 = go-redis default (10 per CPU). DialTimeout default 5s.
	PoolSize    int `yaml:"pool_size"`
	DialTimeout int `yaml:"dial_timeout"`
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"
	RoleTenant     = "tenant"
	RolePlatform   = "platform"
	defaultAddr    = "127.0.0.1:6379"
)

func (c Config) SourceName() string {
	if s := strings.TrimSpace(c.Source); s != "" {
		return s
	}
	return SourceStatic
}

func (c Config) RoleName() string {
	if r := strings.TrimSpace(c.Role); r != "" {
		return r
	}
	return RoleTenant
}

// Validate rejects a configuration that cannot work, naming the setting.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.SourceName() {
	case SourceStatic:
	case SourcePlatform:
		switch c.RoleName() {
		case RoleTenant, RolePlatform:
		default:
			return fmt.Errorf("redisx: redis.role is %q; it must be %s or %s", c.Role, RoleTenant, RolePlatform)
		}
	default:
		return fmt.Errorf("redisx: redis.source is %q; it must be %s or %s", c.Source, SourceStatic, SourcePlatform)
	}
	return nil
}

// Target is a server to connect to, once resolved.
type Target struct {
	Addr     string
	Password string
	DB       int
}

// Static is the target of a static configuration.
func (c Config) Static() Target {
	addr := strings.TrimSpace(c.Addr)
	if addr == "" {
		addr = defaultAddr
	}
	return Target{Addr: addr, Password: c.Password, DB: c.DB}
}

// TargetFromRow turns a datasource row (host:port, db_name as the index, the
// resolved password) into a target.
func TargetFromRow(addr, dbName, password string) (Target, error) {
	db := 0
	if s := strings.TrimSpace(dbName); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return Target{}, fmt.Errorf("redisx: the datasource row's db_name %q is not a database index", dbName)
		}
		db = n
	}
	return Target{Addr: addr, Password: password, DB: db}, nil
}

// Open connects to the target, pings it and returns the client, with every
// command a span of the request's trace.
func Open(ctx context.Context, t Target, cfg Config) (*redis.Client, error) {
	if t.Addr == "" {
		return nil, fmt.Errorf("redisx: addr is required")
	}
	timeout := time.Duration(cfg.DialTimeout) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	cli := redis.NewClient(&redis.Options{
		Addr:        t.Addr,
		Password:    t.Password,
		DB:          t.DB,
		PoolSize:    cfg.PoolSize,
		DialTimeout: timeout,
	})
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := cli.Ping(pingCtx).Err(); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("redisx: ping %s db %d: %w (is the server up, is redis.addr right, and does the deployment set the password?)", t.Addr, t.DB, err)
	}
	if err := redisotel.InstrumentTracing(cli, redisotel.WithDBStatement(false)); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("redisx: tracing: %w", err)
	}
	return cli, nil
}

// New connects a static configuration and pings; what a program that is not
// a service (a tool, a test) uses. Callers own Close().
func New(ctx context.Context, c Config) (*redis.Client, error) {
	return Open(ctx, c.Static(), c)
}
