package kitexx

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"github.com/sezznaw/devkit-common/centrifugox"
	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/delayx"
	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/idx"
	"github.com/sezznaw/devkit-common/jobx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/mysqlx"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/s3x"
	"github.com/sezznaw/devkit-common/starrocksx"
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
	// Kafka is the event bus (Redpanda), nil when kafka.enabled is false:
	// Publish from a handler, Subscribe in app.Setup; the consumers run once
	// the server is up (Run) and stop before it.
	Kafka *kafkax.Client
	// Centrifugo is the push server, nil when centrifugo.enabled is false:
	// Publish to a channel, ConnectionToken for a client.
	Centrifugo *centrifugox.Client
	// S3 is the object storage, nil when s3.enabled is false: Put, Get,
	// Stat, List, Delete and presigned URLs, all under the service's prefix.
	S3 *s3x.Client
	// Report is the report database (StarRocks, a copy of the business
	// tables a few seconds behind), nil unless report.enabled: Query and
	// QueryRow, every query filtered by `tenant_id = :tenant`. Reports and
	// statistics only; nothing that decides anything reads it.
	Report *starrocksx.Client
	// ID makes the business's unique numbers (bet, order, statement
	// numbers): rt.ID.Next() is a 53-bit, time-ordered id unique across the
	// replicas (the instance number is leased in Redis), exact as a JSON
	// number and a JavaScript Number; in the database a BIGINT.
	ID *idx.Generator
	// Delay runs a function once at a chosen time, nil when mysql is off or
	// delay.enabled is false: Handle(kind, fn) in app.Setup, Schedule in the
	// transaction of the change that needs it, Cancel while still pending.
	Delay *delayx.Scheduler

	jobs      []jobx.Job
	providers map[string]*httpx.Client
	starters  []starter
}

// TenantEnv names the environment variable a deployment sets to say which
// tenant it serves; mysql.source platform looks that tenant's database up
// in the deployment's datasource table. One tenant is one complete
// deployment (own machines, own databases, own Nacos), so the table holds
// one tenant and "platform" means "this deployment's registry of data
// sources", not a layer shared across tenants.
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
	rt.serveMetrics()
	if err := rt.openMySQL(); err != nil {
		return nil, err
	}
	rt.openDelay()
	if err := rt.openRedis(); err != nil {
		return nil, err
	}
	if err := rt.openID(); err != nil {
		return nil, err
	}
	if err := rt.openKafka(); err != nil {
		return nil, err
	}
	if err := rt.openCentrifugo(); err != nil {
		return nil, err
	}
	if err := rt.openReport(); err != nil {
		return nil, err
	}
	if err := rt.openS3(); err != nil {
		return nil, err
	}
	if err := rt.openProviders(); err != nil {
		return nil, err
	}
	return rt, nil
}

func (rt *Runtime) openS3() error {
	cfg := rt.Config
	if !cfg.S3.Enabled {
		zlog.Info("s3 off", zlog.Str("hint", "set s3.enabled: true in conf/<env>.yaml for a service that stores files"))
		return nil
	}
	if err := cfg.S3.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := s3Target(ctx, cfg)
	if err != nil {
		return err
	}
	cli, err := s3x.Open(ctx, target, cfg.S3, cfg.Service.Name)
	if err != nil {
		return err
	}
	rt.S3 = cli
	zlog.Info("s3 connected", zlog.Str("endpoint", target.Endpoint), zlog.Str("bucket", target.Bucket), zlog.Str("prefix", cli.Prefix()), zlog.Str("source", cfg.S3.SourceName()))
	return nil
}

// s3Target: the static fields, or the datasource row of kind s3 (host:port,
// db_name = bucket, password_env = the access key's variable, params with
// path_style / insecure / secret_env).
func s3Target(ctx context.Context, cfg Config) (s3x.Target, error) {
	if cfg.S3.SourceName() == s3x.SourceStatic {
		return cfg.S3.Static(), nil
	}
	platform, err := platformDatabase(cfg, "s3.source platform")
	if err != nil {
		return s3x.Target{}, err
	}
	row, err := mysqlx.ResolveKind(ctx, platform, os.Getenv(TenantEnv), mysqlx.RoleTenant, "s3", os.Getenv)
	if err != nil {
		return s3x.Target{}, err
	}
	return s3x.TargetFromRow(row.Addr, row.DB, row.Password, row.Params, os.Getenv)
}

// PodNamespaceEnv is set by the deployment; a service that calls external
// APIs must run in a namespace that may leave the cluster.
const PodNamespaceEnv = "POD_NAMESPACE"

// ProviderNamespaceSuffix is the default egress.namespace_suffix: the
// namespaces with a way out of the cluster end in it ("<env>-provider").
const ProviderNamespaceSuffix = "-provider"

// openProviders builds one httpx client per `providers:` entry. The
// deployment rule is checked here: a service with providers in a namespace
// without egress would fail at the first call, at 3 am; it fails at start
// instead, with the fix in the message.
func (rt *Runtime) openProviders() error {
	cfg := rt.Config
	rt.providers = map[string]*httpx.Client{}
	if len(cfg.Providers) == 0 {
		return nil
	}
	if suffix := cfg.Egress.suffix(); cfg.Egress.check() {
		if ns := os.Getenv(PodNamespaceEnv); ns != "" && !strings.HasSuffix(ns, suffix) {
			return fmt.Errorf("kitexx: this service has providers (%s) but runs in namespace %q, which cannot reach the internet; deploy it to the %s namespace (services/<env>/<service>/app.yaml: namespace: %s%s), or set egress.check: false in a deployment without that split",
				providerNames(cfg), ns, suffix, strings.TrimSuffix(ns, suffix), suffix)
		}
	}
	for name, p := range cfg.Providers {
		c, err := httpx.New(name, p)
		if err != nil {
			return err
		}
		rt.providers[name] = c
		zlog.Info("provider configured", zlog.Str("name", name), zlog.Str("base_url", p.BaseURL), zlog.Dur("timeout", p.Timeout.Or(5*time.Second)))
	}
	return nil
}

func providerNames(cfg Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Provider is the client of an external API listed under `providers:`. A
// name that is not configured is a start-time mistake, not a run-time one:
// it panics with the names that exist, and app.Setup is where it is called.
func (rt *Runtime) Provider(name string) *httpx.Client {
	c, ok := rt.providers[name]
	if !ok {
		panic(fmt.Sprintf("kitexx: no provider %q in the configuration; providers: [%s]. Add providers.%s to conf/<env>.yaml", name, providerNames(rt.Config), name))
	}
	return c
}

func (rt *Runtime) openCentrifugo() error {
	cfg := rt.Config
	if !cfg.Centrifugo.Enabled {
		zlog.Info("centrifugo off", zlog.Str("hint", "set centrifugo.enabled: true in conf/<env>.yaml for a service that pushes to clients"))
		return nil
	}
	if err := cfg.Centrifugo.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := centrifugoTarget(cfg)
	if err != nil {
		return err
	}
	cli, err := centrifugox.Open(ctx, target, cfg.Centrifugo)
	if err != nil {
		return err
	}
	rt.Centrifugo = cli
	zlog.Info("centrifugo connected", zlog.Str("api", target.APIAddr), zlog.Bool("token_secret", target.TokenSecret != ""), zlog.Str("source", cfg.Centrifugo.SourceName()))
	return nil
}

// centrifugoTarget: the configuration with source static; with source
// platform the centrifugo section of infra.yaml in Nacos (api_addr, and the
// names of the environment variables that hold the API key and the token
// secret).
func centrifugoTarget(cfg Config) (centrifugox.Target, error) {
	if cfg.Centrifugo.SourceName() == centrifugox.SourceStatic {
		return cfg.Centrifugo.Static(), nil
	}
	doc, err := infraDocument(cfg, "centrifugo.source platform")
	if err != nil {
		return centrifugox.Target{}, err
	}
	c := doc.Centrifugo
	if c.APIAddr == "" || c.APIKeyEnv == "" {
		return centrifugox.Target{}, fmt.Errorf("centrifugo.source platform: %s in Nacos must have centrifugo.{api_addr, api_key_env}; got api_addr=%q api_key_env=%q", infraDataID, c.APIAddr, c.APIKeyEnv)
	}
	key := os.Getenv(c.APIKeyEnv)
	if key == "" {
		return centrifugox.Target{}, fmt.Errorf("centrifugo.source platform: the API key is to come from the environment variable %q (infra.yaml centrifugo.api_key_env), which is not set", c.APIKeyEnv)
	}
	secret := ""
	if c.TokenHMACEnv != "" {
		secret = os.Getenv(c.TokenHMACEnv)
	}
	return centrifugox.Target{APIAddr: c.APIAddr, APIKey: key, TokenSecret: secret}, nil
}

// openID makes the id generator: an instance number leased in Redis when
// the service has it, else INSTANCE_ID or a hostname hash (with a warning).
func (rt *Runtime) openID() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := idx.New(ctx, rt.Redis, rt.Config.Service.Name, rt.Config.ID, idx.Options{Local: rt.Config.Log.Env == "local" || rt.Config.Log.Env == ""})
	if err != nil {
		return err
	}
	rt.ID = g
	OnShutdown("id lease release", g.Close)
	return nil
}

// openReport opens the report database when report.enabled: source static
// from the section, source platform from the starrocks section of
// infra.yaml in Nacos (addr, db, user, the password's environment variable).
func (rt *Runtime) openReport() error {
	cfg := rt.Config
	if !cfg.Report.Enabled {
		return nil
	}
	if err := cfg.Report.Validate(); err != nil {
		return err
	}
	var target starrocksx.Target
	if cfg.Report.SourceName() == starrocksx.SourceStatic {
		target = cfg.Report.Static()
	} else {
		doc, err := infraDocument(cfg, "report.source platform")
		if err != nil {
			return err
		}
		sr := doc.StarRocks
		if sr.Addr == "" || sr.DB == "" || sr.User == "" {
			return fmt.Errorf("report.source platform: %s in Nacos must have starrocks.{addr, db, user, password_env}; got addr=%q db=%q user=%q", infraDataID, sr.Addr, sr.DB, sr.User)
		}
		pw := ""
		if sr.PasswordEnv != "" {
			pw = os.Getenv(sr.PasswordEnv)
			if pw == "" {
				return fmt.Errorf("report.source platform: the password is to come from the environment variable %q (infra.yaml starrocks.password_env), which is not set", sr.PasswordEnv)
			}
		}
		target = starrocksx.Target{Addr: sr.Addr, DB: sr.DB, User: sr.User, Password: pw}
	}
	tenant := strings.TrimSpace(os.Getenv(TenantEnv))
	if tenant == "" && cfg.Report.SourceName() == starrocksx.SourceStatic {
		tenant = "tenant_a" // a laptop: the local stack's tenant
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli, err := starrocksx.Open(ctx, target, cfg.Report, tenant)
	if err != nil {
		return err
	}
	rt.Report = cli
	OnShutdown("report close", cli.Close)
	zlog.Info("report database connected", zlog.Str("addr", target.Addr), zlog.Str("db", target.DB), zlog.Str("tenant", tenant), zlog.Str("source", cfg.Report.SourceName()))
	return nil
}

// openDelay makes the delayed-task scheduler on the service's MySQL. The
// poller starts with the server only when app.Setup registered a handler:
// a service that schedules nothing has no delayed_task table to poll.
func (rt *Runtime) openDelay() {
	cfg := rt.Config
	if rt.DB == nil || !cfg.Delay.EnabledOrDefault() {
		return
	}
	s := delayx.New(rt.DB, cfg.Delay, cfg.Service.Name)
	rt.Delay = s
	rt.OnStart("delayed tasks", func() error {
		if s.Handlers() == 0 {
			zlog.Debug("delayed tasks off", zlog.Str("hint", "rt.Delay.Handle(kind, fn) in app.Setup starts the scheduler"))
			return nil
		}
		s.Start(context.Background())
		return nil
	})
	OnShutdown("delayed tasks stop", func() error { s.Stop(); return nil })
}

func (rt *Runtime) openKafka() error {
	cfg := rt.Config
	if !cfg.Kafka.Enabled {
		zlog.Info("kafka off", zlog.Str("hint", "set kafka.enabled: true in conf/<env>.yaml for a service that publishes or consumes events"))
		return nil
	}
	if err := cfg.Kafka.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := kafkaTarget(ctx, cfg)
	if err != nil {
		return err
	}
	cli, err := kafkax.Open(ctx, target, cfg.Kafka, cfg.Service.Name, strings.TrimSpace(os.Getenv(TenantEnv)))
	if err != nil {
		return err
	}
	rt.Kafka = cli
	if rt.Redis != nil && cfg.Kafka.Dedupe.EnabledOrDefault() {
		cli.SetDedupe(rt.Redis)
	} else if rt.Redis == nil {
		zlog.Warn("kafka consumers run without dedupe (no redis): a redelivered event reaches the handler again; handlers must be idempotent")
	}
	zlog.Info("kafka connected", zlog.Any("brokers", target.Brokers), zlog.Bool("sasl", target.Username != ""),
		zlog.Str("source", cfg.Kafka.SourceName()), zlog.Str("role", cfg.Kafka.RoleName()))
	// The outbox relay: events queued in the database (PublishTx) go out
	// from here. Needs the service's MySQL; off by kafka.outbox.enabled.
	if rt.DB != nil && cfg.Kafka.Outbox.EnabledOrDefault() {
		db := rt.DB
		rt.OnStart("outbox relay", func() error {
			cli.StartOutbox(context.Background(), db, cfg.Kafka.Outbox)
			return nil
		})
		OnShutdown("outbox relay stop", func() error { cli.StopOutbox(); return nil })
	}
	OnShutdown("kafka close", cli.Close)
	return nil
}

func kafkaTarget(ctx context.Context, cfg Config) (kafkax.Target, error) {
	if cfg.Kafka.SourceName() == kafkax.SourceStatic {
		return cfg.Kafka.Static(), nil
	}
	platform, err := platformDatabase(cfg, "kafka.source platform")
	if err != nil {
		return kafkax.Target{}, err
	}
	row, err := mysqlx.ResolveKind(ctx, platform, strings.TrimSpace(os.Getenv(TenantEnv)), cfg.Kafka.RoleName(), "kafka", os.Getenv)
	if err != nil {
		return kafkax.Target{}, err
	}
	// Several brokers: host holds them comma-separated, port the common port.
	var brokers []string
	for _, h := range strings.Split(row.Addr, ",") {
		if h = strings.TrimSpace(h); h != "" {
			if !strings.Contains(h, ":") {
				h = h + ":" + row.Addr[strings.LastIndex(row.Addr, ":")+1:]
			}
			brokers = append(brokers, h)
		}
	}
	return kafkax.Target{Brokers: brokers, Username: row.User, Password: row.Password}, nil
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
	cli, err := redisx.Open(ctx, target, cfg.Redis, cfg.Service.Name)
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
	if sqlDB, err := db.DB(); err == nil {
		metricsx.RegisterDBStats(sqlDB, target.DB)
	}
	zlog.Info("mysql connected", zlog.Str("addr", target.Addr), zlog.Str("db", target.DB), zlog.Str("user", target.User),
		zlog.Str("source", cfg.MySQL.SourceName()), zlog.Str("role", cfg.MySQL.RoleName()))
	OnShutdown("mysql close", func() error { return mysqlx.Close(db) })
	return nil
}

// infraDoc is the part of infra.yaml (in Nacos) the framework reads: the
// platform database, and the push server.
type infraDoc struct {
	MySQL struct {
		Platform struct {
			Addr        string `yaml:"addr"`
			DB          string `yaml:"db"`
			User        string `yaml:"user"`
			PasswordEnv string `yaml:"password_env"`
		} `yaml:"platform"`
	} `yaml:"mysql"`
	Centrifugo struct {
		APIAddr      string `yaml:"api_addr"`
		APIKeyEnv    string `yaml:"api_key_env"`
		TokenHMACEnv string `yaml:"token_hmac_env"`
	} `yaml:"centrifugo"`
	StarRocks struct {
		Addr        string `yaml:"addr"`
		DB          string `yaml:"db"`
		User        string `yaml:"user"`
		PasswordEnv string `yaml:"password_env"`
	} `yaml:"starrocks"`
}

// infraDocument reads infra.yaml from Nacos. what says which setting needs
// it, for the error.
func infraDocument(cfg Config, what string) (infraDoc, error) {
	var doc infraDoc
	nc, err := Nacos(cfg)
	if err != nil {
		return doc, fmt.Errorf("%s needs Nacos for %s: %w", what, infraDataID, err)
	}
	content, err := nc.Get(infraDataID)
	if err != nil {
		return doc, fmt.Errorf("%s: read %s from Nacos: %w", what, infraDataID, err)
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return doc, fmt.Errorf("%s: %s in Nacos: %w", what, infraDataID, err)
	}
	return doc, nil
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
	doc, err := infraDocument(cfg, what)
	if err != nil {
		return mysqlx.Target{}, err
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

// serveMetrics starts the /metrics listener when metrics.enabled is set. A
// port that is taken (two services on one laptop) is a warning, not a start
// failure: metrics are how the service is watched, not what it does.
func (rt *Runtime) serveMetrics() {
	cfg := rt.Config
	if !cfg.Metrics.Enabled {
		zlog.Info("metrics off", zlog.Str("hint", "set metrics.enabled: true in conf/<env>.yaml; deployments scrape port "+strconv.Itoa(metricsx.DefaultPort)))
		return
	}
	addr, shutdown, err := metricsx.Serve(cfg.Metrics)
	if err != nil {
		zlog.Warn("metrics not served", zlog.Err(err))
		return
	}
	OnShutdown("metrics", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return shutdown(ctx)
	})
	zlog.Info("metrics on", zlog.Str("addr", addr), zlog.Str("path", "/metrics"))
}
