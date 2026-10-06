package kitexx

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/mysqlx"
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
	return rt, nil
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
// source static; with source platform, the platform database (its address
// from infra.yaml in Nacos, its password from the environment variable that
// document names) is asked for the database of the tenant in TENANT_CODE.
func mysqlTarget(ctx context.Context, cfg Config) (mysqlx.Target, error) {
	if cfg.MySQL.SourceName() == mysqlx.SourceStatic {
		return cfg.MySQL.Static(), nil
	}
	nc, err := Nacos(cfg)
	if err != nil {
		return mysqlx.Target{}, fmt.Errorf("mysql.source platform needs Nacos for %s: %w", infraDataID, err)
	}
	content, err := nc.Get(infraDataID)
	if err != nil {
		return mysqlx.Target{}, fmt.Errorf("mysql.source platform: read %s from Nacos: %w", infraDataID, err)
	}
	var doc infraDoc
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return mysqlx.Target{}, fmt.Errorf("mysql.source platform: %s in Nacos: %w", infraDataID, err)
	}
	p := doc.MySQL.Platform
	if p.Addr == "" || p.DB == "" || p.User == "" || p.PasswordEnv == "" {
		return mysqlx.Target{}, fmt.Errorf("mysql.source platform: %s in Nacos must have mysql.platform.{addr, db, user, password_env}; got addr=%q db=%q user=%q password_env=%q", infraDataID, p.Addr, p.DB, p.User, p.PasswordEnv)
	}
	password := os.Getenv(p.PasswordEnv)
	if password == "" {
		return mysqlx.Target{}, fmt.Errorf("mysql.source platform: the platform database's password is to come from the environment variable %q (infra.yaml mysql.platform.password_env), which is not set", p.PasswordEnv)
	}
	platform := mysqlx.Target{Addr: p.Addr, DB: p.DB, User: p.User, Password: password}
	return mysqlx.Resolve(ctx, platform, strings.TrimSpace(os.Getenv(TenantEnv)), cfg.MySQL.RoleName(), os.Getenv)
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
