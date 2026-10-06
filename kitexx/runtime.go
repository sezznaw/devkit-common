package kitexx

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/mysqlx"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Runtime is what the framework has opened for a service by the time the
// service's own code runs: the configuration in effect and the connections
// its `mysql:` (later `redis:`, ...) sections asked for. main() gets it from
// NewRuntime and passes it to app.Setup, which hands the pieces to the repo
// and handler; a service never opens these itself, and never adds to this
// struct, which belongs to the framework.
type Runtime struct {
	Config Config
	// DB is the MySQL handle, nil when mysql.enabled is false. Queries take
	// the request's context (db.WithContext(ctx)) so they are spans of its
	// trace and records of its log.
	DB *gorm.DB
	// Redis is the Redis / Valkey client, nil when redis.enabled is false.
	// Commands take the request's context for the same reason.
	Redis *redis.Client
}

// TenantEnv names the environment variable a deployment sets to say which
// tenant it serves; mysql.source platform looks that tenant's database up.
const TenantEnv = "TENANT_CODE"

// infraDataID is the Nacos configuration that holds the addresses of the
// platform's infrastructure, among them the platform database.
const infraDataID = "infra.yaml"

// NewRuntime is what a service does first: it checks the configuration,
// installs the logger and tracing (Bootstrap), and opens what the
// configuration enables, MySQL first of all, before the service registers
// anywhere: a service whose database is not there does not take traffic, it
// fails to start and says why.
func NewRuntime(cfg Config) (*Runtime, error) {
	if err := Bootstrap(&cfg); err != nil {
		return nil, err
	}
	rt := &Runtime{Config: cfg}
	if err := rt.openMySQL(); err != nil {
		return nil, err
	}
	if err := rt.openRedis(); err != nil {
		return nil, err
	}
	return rt, nil
}

func (rt *Runtime) openRedis() error {
	cfg := rt.Config
	if !cfg.Redis.Enabled {
		zlog.Info("redis off", zlog.Str("hint", "set redis.enabled: true in conf/<env>.yaml for a service that uses Redis (idempotent requests need it)"))
		return nil
	}
	if err := cfg.Redis.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := redisTarget(ctx, cfg)
	if err != nil {
		return err
	}
	if config.IsLocal() && !isLoopback(target.Addr) {
		zlog.Warn("redis: this machine is using a server that is not on it; everybody who shares it sees what you write",
			zlog.Str("addr", target.Addr), zlog.Int("db", target.DB))
	}
	cli, err := redisx.Open(ctx, target, cfg.Redis)
	if err != nil {
		return err
	}
	rt.Redis = cli
	zlog.Info("redis connected", zlog.Str("addr", target.Addr), zlog.Int("db", target.DB),
		zlog.Str("source", cfg.Redis.SourceName()), zlog.Str("role", cfg.Redis.RoleName()))
	OnShutdown("redis close", cli.Close)
	return nil
}

// redisTarget is mysqlTarget for Redis: the configuration with source
// static, the tenant's valkey row of the datasource table with source platform.
func redisTarget(ctx context.Context, cfg Config) (redisx.Target, error) {
	if cfg.Redis.SourceName() == redisx.SourceStatic {
		return cfg.Redis.Static(), nil
	}
	platform, err := platformDatabase(cfg, "redis.source platform")
	if err != nil {
		return redisx.Target{}, err
	}
	row, err := mysqlx.ResolveKind(ctx, platform, strings.TrimSpace(os.Getenv(TenantEnv)), cfg.Redis.RoleName(), "valkey", os.Getenv)
	if err != nil {
		return redisx.Target{}, err
	}
	return redisx.TargetFromRow(row.Addr, row.DB, row.Password)
}

func (rt *Runtime) openMySQL() error {
	cfg := rt.Config
	if !cfg.MySQL.Enabled {
		zlog.Info("mysql off", zlog.Str("hint", "set mysql.enabled: true in conf/<env>.yaml for a service that uses MySQL"))
		return nil
	}
	if err := cfg.MySQL.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := mysqlTarget(ctx, cfg)
	if err != nil {
		return err
	}
	if config.IsLocal() && !isLoopback(target.Addr) {
		zlog.Warn("mysql: this machine is using a database that is not on it; everybody who shares that database sees what you write",
			zlog.Str("addr", target.Addr), zlog.Str("db", target.DB))
	}
	db, err := mysqlx.Open(ctx, target, cfg.MySQL)
	if err != nil {
		return err
	}
	rt.DB = db
	zlog.Info("mysql connected", zlog.Str("addr", target.Addr), zlog.Str("db", target.DB), zlog.Str("user", target.User),
		zlog.Str("source", cfg.MySQL.SourceName()), zlog.Str("role", cfg.MySQL.RoleName()))
	OnShutdown("mysql close", func() error { return mysqlx.Close(db) })
	return nil
}

// infraDoc is the part of infra.yaml the platform database is described in.
type infraDoc struct {
	MySQL struct {
		Platform struct {
			Addr        string `yaml:"addr"`
			DB          string `yaml:"db"`
			User        string `yaml:"user"`
			PasswordEnv string `yaml:"password_env"`
		} `yaml:"platform"`
	} `yaml:"mysql"`
}

// mysqlTarget resolves where the database is: the configuration itself with
// source static; with source platform, the platform database is asked for
// the database of the tenant in TENANT_CODE.
func mysqlTarget(ctx context.Context, cfg Config) (mysqlx.Target, error) {
	if cfg.MySQL.SourceName() == mysqlx.SourceStatic {
		return cfg.MySQL.Static(), nil
	}
	platform, err := platformDatabase(cfg, "mysql.source platform")
	if err != nil {
		return mysqlx.Target{}, err
	}
	return mysqlx.Resolve(ctx, platform, strings.TrimSpace(os.Getenv(TenantEnv)), cfg.MySQL.RoleName(), os.Getenv)
}

// platformDatabase is the platform database every source-platform lookup
// starts from: its address from infra.yaml in Nacos (mysql.platform), its
// password from the environment variable that document names. what says
// which setting needs it, for the error.
func platformDatabase(cfg Config, what string) (mysqlx.Target, error) {
	nc, err := Nacos(cfg)
	if err != nil {
		return mysqlx.Target{}, fmt.Errorf("%s needs Nacos for %s: %w", what, infraDataID, err)
	}
	content, err := nc.Get(infraDataID)
	if err != nil {
		return mysqlx.Target{}, fmt.Errorf("%s: read %s from Nacos: %w", what, infraDataID, err)
	}
	var doc infraDoc
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return mysqlx.Target{}, fmt.Errorf("%s: %s in Nacos: %w", what, infraDataID, err)
	}
	p := doc.MySQL.Platform
	if p.Addr == "" || p.DB == "" || p.User == "" || p.PasswordEnv == "" {
		return mysqlx.Target{}, fmt.Errorf("%s: %s in Nacos must have mysql.platform.{addr, db, user, password_env}; got addr=%q db=%q user=%q password_env=%q", what, infraDataID, p.Addr, p.DB, p.User, p.PasswordEnv)
	}
	password := os.Getenv(p.PasswordEnv)
	if password == "" {
		return mysqlx.Target{}, fmt.Errorf("%s: the platform database's password is to come from the environment variable %q (infra.yaml mysql.platform.password_env), which is not set", what, p.PasswordEnv)
	}
	return mysqlx.Target{Addr: p.Addr, DB: p.DB, User: p.User, Password: password}, nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
