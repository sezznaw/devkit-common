// Package mysqlx opens the MySQL connection of a service from its `mysql:`
// configuration: a GORM handle on a connection pool, with every query a span
// of the request's trace and a record in its log (slow queries and errors at
// warn / error, the rest at debug). Where the database is comes either from
// the configuration itself (source static: a developer's machine) or from the
// platform's datasource table (source platform: a deployment, which only says
// which tenant it serves). kitexx opens it at start-up and hands it to the
// service in Runtime.DB; a service never calls this package itself.
package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/plugin/opentelemetry/tracing"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the `mysql:` section of a service configuration. Every service
// has it; enabled: false (the default) means the service does not use MySQL
// and nothing here is read.
type Config struct {
	// Enabled switches MySQL on for this service.
	Enabled bool `yaml:"enabled"`
	// Source says where the database is found: "static", the addr / db /
	// user / password below (a developer's machine); or "platform", the
	// platform's datasource table, looked up for the tenant the deployment
	// names in TENANT_CODE (dev, uat, prod). Default static.
	Source string `yaml:"source"`
	// Role, with source platform: "tenant" (default), the database of the
	// tenant this deployment serves; or "platform", the platform database
	// itself (the tenant service).
	Role string `yaml:"role"`

	// Addr, DB, User, Password, Params: the database, with source static.
	// Addr "host:port", default 127.0.0.1:3306. Params are extra DSN
	// parameters ("a=1&b=2"); parseTime, UTC and utf8mb4 are always set.
	Addr     string `yaml:"addr"`
	DB       string `yaml:"db"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Params   string `yaml:"params"`

	// MaxOpen and MaxIdle size the pool (defaults 20 and 5);
	// ConnMaxLifetime recycles connections (default 30m, below MySQL's
	// wait_timeout and any load balancer's idle limit).
	MaxOpen         int             `yaml:"max_open"`
	MaxIdle         int             `yaml:"max_idle"`
	ConnMaxLifetime config.Duration `yaml:"conn_max_lifetime"`
	// SlowQuery is the latency from which a query is logged at warn
	// ("mysql slow query"). Default 200ms.
	SlowQuery config.Duration `yaml:"slow_query"`
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"
	RoleTenant     = "tenant"
	RolePlatform   = "platform"

	defaultAddr    = "127.0.0.1:3306"
	defaultMaxOpen = 20
	defaultMaxIdle = 5
	defaultMaxLife = 30 * time.Minute
	defaultSlow    = 200 * time.Millisecond
)

// SourceName is source with its default applied.
func (c Config) SourceName() string {
	if s := strings.TrimSpace(c.Source); s != "" {
		return s
	}
	return SourceStatic
}

// RoleName is role with its default applied.
func (c Config) RoleName() string {
	if r := strings.TrimSpace(c.Role); r != "" {
		return r
	}
	return RoleTenant
}

// Validate rejects a configuration that cannot work before anything is
// dialled, naming the setting.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.SourceName() {
	case SourceStatic:
		if c.DB == "" {
			return fmt.Errorf("mysqlx: mysql.db is required with source static")
		}
		if c.User == "" {
			return fmt.Errorf("mysqlx: mysql.user is required with source static")
		}
	case SourcePlatform:
		switch c.RoleName() {
		case RoleTenant, RolePlatform:
		default:
			return fmt.Errorf("mysqlx: mysql.role is %q; it must be %s or %s", c.Role, RoleTenant, RolePlatform)
		}
	default:
		return fmt.Errorf("mysqlx: mysql.source is %q; it must be %s or %s", c.Source, SourceStatic, SourcePlatform)
	}
	return nil
}

// Target is a database to connect to, once resolved: what Config holds with
// source static, or what the datasource table says with source platform.
type Target struct {
	Addr     string
	DB       string
	User     string
	Password string
	Params   string
}

// Static is the target of a static configuration.
func (c Config) Static() Target {
	addr := strings.TrimSpace(c.Addr)
	if addr == "" {
		addr = defaultAddr
	}
	return Target{Addr: addr, DB: c.DB, User: c.User, Password: c.Password, Params: c.Params}
}

// DSN is the go-sql-driver DSN of the target. Times are parsed and in UTC,
// the charset is utf8mb4, and the connection, read and write time out.
func (t Target) DSN() string {
	q := url.Values{}
	q.Set("parseTime", "true")
	q.Set("loc", "UTC")
	q.Set("time_zone", "'+00:00'")
	q.Set("charset", "utf8mb4")
	q.Set("timeout", "5s")
	q.Set("readTimeout", "30s")
	q.Set("writeTimeout", "30s")
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?%s", t.User, t.Password, t.Addr, t.DB, q.Encode())
	if p := strings.TrimSpace(t.Params); p != "" {
		dsn += "&" + strings.TrimPrefix(p, "&")
	}
	return dsn
}

// Open connects to the target, pings it and returns the GORM handle: pool
// sized by cfg, every query traced (a child span of the request, with the
// statement without its values) and logged through zlog with the request's
// trace_id. GORM's AutoMigrate is not to be used: the schema comes from the
// migrations.
func Open(ctx context.Context, t Target, cfg Config) (*gorm.DB, error) {
	if t.Addr == "" || t.DB == "" || t.User == "" {
		return nil, fmt.Errorf("mysqlx: target is incomplete: addr=%q db=%q user=%q", t.Addr, t.DB, t.User)
	}
	slow := cfg.SlowQuery.Or(defaultSlow)
	db, err := gorm.Open(mysql.Open(t.DSN()), &gorm.Config{
		Logger:                 &zlogger{slow: slow},
		SkipDefaultTransaction: true, // one statement is one statement; transactions are explicit
		PrepareStmt:            false,
		// Times go in and come out as UTC, see the DSN.
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("mysqlx: connect to %s/%s as %s: %w%s", t.Addr, t.DB, t.User, err, hint)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("mysqlx: %w", err)
	}
	sqlDB.SetMaxOpenConns(nonZero(cfg.MaxOpen, defaultMaxOpen))
	sqlDB.SetMaxIdleConns(nonZero(cfg.MaxIdle, defaultMaxIdle))
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime.Or(defaultMaxLife))

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("mysqlx: ping %s/%s as %s: %w%s", t.Addr, t.DB, t.User, err, hint)
	}
	if err := db.Use(tracing.NewPlugin(tracing.WithoutMetrics(), tracing.WithoutQueryVariables())); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("mysqlx: tracing: %w", err)
	}
	return db, nil
}

// Close closes the pool behind db; nil is fine.
func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Resolve looks the database of a tenant (or the platform's own, with role
// platform) up in the platform database's datasource table and returns it as
// a target, with the password read from the environment variable the row
// names (password_env): the table knows where, the deployment knows the
// secret. An unknown tenant or an unset variable is an error that names it.
func Resolve(ctx context.Context, platform Target, tenant, role string, getenv func(string) string) (Target, error) {
	code := tenant
	if role == RolePlatform {
		code = "platform"
	}
	if code == "" {
		return Target{}, fmt.Errorf("mysqlx: TENANT_CODE is not set; a deployment with mysql.source platform has to say which tenant it serves")
	}
	sqlDB, err := sql.Open("mysql", platform.DSN())
	if err != nil {
		return Target{}, fmt.Errorf("mysqlx: platform database: %w", err)
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var host, dbName, user, passwordEnv, params string
	var port int
	err = sqlDB.QueryRowContext(ctx,
		"SELECT host, port, db_name, username, password_env, params FROM datasource WHERE tenant_code = ? AND role = ? AND kind = 'mysql'",
		code, role).Scan(&host, &port, &dbName, &user, &passwordEnv, &params)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, fmt.Errorf("mysqlx: no mysql datasource for tenant %q with role %s in %s/%s.datasource", code, role, platform.Addr, platform.DB)
	}
	if err != nil {
		return Target{}, fmt.Errorf("mysqlx: read datasource of tenant %q from %s/%s: %w", code, platform.Addr, platform.DB, err)
	}
	password := getenv(passwordEnv)
	if passwordEnv == "" || password == "" {
		return Target{}, fmt.Errorf("mysqlx: the password of tenant %q's database is to come from the environment variable %q, which is not set", code, passwordEnv)
	}
	return Target{Addr: fmt.Sprintf("%s:%d", host, port), DB: dbName, User: user, Password: password, Params: params}, nil
}

// hint goes with every connection failure: the three things that are usually wrong.
const hint = " (is the database up, is mysql.addr right, and does the deployment set the password?)"

func nonZero(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// zlogger is GORM's logger on zlog: every statement a debug record of the
// request's logger (so it carries the trace_id), slow ones a warning, failed
// ones an error. A row that is not there (ErrRecordNotFound) is not an error
// of the query.
type zlogger struct{ slow time.Duration }

func (l *zlogger) LogMode(logger.LogLevel) logger.Interface { return l }
func (l *zlogger) Info(_ context.Context, msg string, args ...any) {
	zlog.Infof("gorm: "+msg, args...)
}
func (l *zlogger) Warn(_ context.Context, msg string, args ...any) {
	zlog.Warnf("gorm: "+msg, args...)
}
func (l *zlogger) Error(_ context.Context, msg string, args ...any) {
	zlog.Errorf("gorm: "+msg, args...)
}

func (l *zlogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	statement, rows := fc()
	fields := []zlog.Field{zlog.Str("sql", statement), zlog.Int("rows", rows), zlog.Dur("latency", elapsed)}
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		zlog.Ctx(ctx).Error("mysql", append(fields, zlog.Err(err))...)
	case elapsed >= l.slow:
		zlog.Ctx(ctx).Warn("mysql slow query", fields...)
	default:
		zlog.Ctx(ctx).Debug("mysql", fields...)
	}
}
