// Package kitexx wires a Kitex server or client to the company infrastructure:
// Nacos registration / discovery, structured logging with a trace_id that
// follows a request from service to service, and request logging middleware. A service's main() should only need Options() and Run().
package kitexx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/server"
	"github.com/cloudwego/kitex/transport"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/centrifugox"
	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/delayx"
	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/idx"
	"github.com/sezznaw/devkit-common/starrocksx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/mysqlx"
	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/otelx"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/s3x"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the part of a service config that kitexx understands. Embed it in
// the service's own config struct.
type Config struct {
	Service struct {
		// Name is the registered service name, e.g. "order".
		Name string `yaml:"name"`
		// Addr is where the server listens: a port (8888), or "host:port"
		// to listen on one interface only ("127.0.0.1:8888"). ":8888" is
		// the same as 8888. Empty: 8888.
		Addr string `yaml:"addr"`
		// Advertise is the address callers reach this instance at, when it
		// is not the one the process sees: "host" or "host:port". A
		// container that is reached through the host's address and a
		// mapped port needs it; the deployment sets it. Empty: the address
		// this machine reaches Nacos from, and the listen port.
		Advertise string `yaml:"advertise"`
	} `yaml:"service"`
	Nacos nacosx.Config `yaml:"nacos"`
	Log   zlog.Options  `yaml:"log"`
	// Otel switches tracing on (otel.endpoint, the OTLP collector) and tunes
	// it. Off, the trace_id is still generated and passed on; on, it is the
	// id of the trace the collector shows.
	Otel otelx.Config `yaml:"otel"`
	// MySQL, when enabled, is opened by NewRuntime before the service
	// registers, and handed to the service as Runtime.DB. Every service has
	// the section; most leave it disabled.
	MySQL mysqlx.Config `yaml:"mysql"`
	// Redis, when enabled, is opened the same way and handed over as
	// Runtime.Redis. Idempotent requests (a request_id field) need it.
	Redis redisx.Config `yaml:"redis"`
	// Kafka (Redpanda), when enabled, is Runtime.Kafka: Publish and Subscribe.
	Kafka kafkax.Config `yaml:"kafka"`
	// Delay: run a function once at a chosen time (unpaid order cancel,
	// closing bets at kick-off): rt.Delay.Schedule in the business change's
	// transaction, rt.Delay.Handle(kind, fn) in app.Setup. On by default
	// when the service has MySQL (table delayed_task from the migrations).
	Delay delayx.Config `yaml:"delay"`
	// Centrifugo, when enabled, is Runtime.Centrifugo: Publish to channels,
	// ConnectionToken for clients.
	Centrifugo centrifugox.Config `yaml:"centrifugo"`
	// S3: s3.enabled opens the object storage (SeaweedFS on dev) as rt.S3,
	// one bucket under a prefix of the service's own; source platform reads
	// the datasource row of kind s3.
	S3 s3x.Config `yaml:"s3"`
	// Auth: the login state (authx). A gateway sets realm and verifies
	// tokens (hertzx.RequireLogin); the authentication service issues them.
	Auth authx.Config `yaml:"auth"`
	// Metrics: metrics.enabled serves Prometheus metrics on metrics.addr
	// (default port 9091): requests by method and code, latency histograms,
	// Go runtime, MySQL pool, Redis commands, Kafka client. Deployments scrape
	// it; off on a laptop.
	Metrics metricsx.Config `yaml:"metrics"`
	// Providers: the external HTTP APIs this service calls, by name
	// (`providers.odds-feed.base_url` ...); rt.Provider(name) is the client.
	// A service with providers runs in the provider namespace (egress).
	Providers map[string]httpx.Provider `yaml:"providers"`
	// Docs: docs.enabled makes an API service serve /docs and /openapi.yaml
	// (hertzx.ServeDocs; the document is generated from the IDL by make gen).
	Docs struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"docs"`
	// CORS: cors.enabled lets browsers on other origins (a web front end
	// served from its own domain, a developer's Vite on localhost) call an
	// API service; hertzx.New installs the middleware. allowed_origins lists
	// the exact origins (scheme, host, port); nothing is allowed by default.
	CORS CORSConfig `yaml:"cors"`
	// Guard: what an API service refuses before any handler runs: request
	// body size (413), a request deadline (504, code 1010) and request-rate
	// limits per client IP (429, code 1008), service-wide and per route
	// (`// @limit` in the IDL). Defaults apply without the section.
	Guard GuardConfig `yaml:"guard"`
	// Limits: what one instance of an RPC service refuses with code 5003
	// (max_in_flight, default 1000; max_qps, off; max_connections, 10000).
	Limits LimitsConfig `yaml:"limits"`
	// Report: report.enabled opens the StarRocks report database (a CDC copy
	// of the business tables, seconds behind) as rt.Report, query only, for
	// the report service alone (devkit lint, rule report-only).
	Report starrocksx.Config `yaml:"report"`
	// ID: the unique-number generator (rt.ID). id.instance_bits splits the
	// low 12 bits between replicas and ids per millisecond (default 5 + 7).
	ID idx.Config `yaml:"id"`
	// RPCClient: timeout, retry and circuit breaker of the calls this
	// service makes to other services (ClientOptions). Defaults suit the
	// cluster; the section is only for changing them.
	RPCClient RPCClientConfig `yaml:"rpc_client"`
	// Callbacks: an HTTP listener for the callbacks providers send us, on a
	// port of its own (the only one the deployment exposes to them, under
	// /callbacks/). webhookx.Server(rt) builds it; it starts and stops with
	// the RPC server. For the one service that integrates vendors.
	Callbacks CallbacksConfig `yaml:"callbacks"`
	// LogLevelDataID names a Nacos configuration whose content is a log level
	// (debug, info, warn, error). The level of the running service follows
	// it, which is how debug logging is switched on in production without a
	// restart. Empty: the level stays what log.level says.
	LogLevelDataID string `yaml:"log_level_data_id"`
	// ConfigDataID names the Nacos configuration that WatchConfig reads.
	// Default: "<service.name>.yaml".
	ConfigDataID string `yaml:"config_data_id"`
	// RegistryDisabled runs the service without Nacos (handy for local runs):
	// it is not registered, the services it calls are not looked up, and
	// configurations are files, see Nacos.
	RegistryDisabled bool `yaml:"registry_disabled"`
	// Shutdown tunes the graceful stop. Both values accept "3s"-style durations.
	Shutdown struct {
		// DeregisterWait is how long to keep serving after leaving Nacos, so
		// callers can notice before the listener closes. Default 3s; only
		// applies when the registry is enabled. Set 0s to turn it off.
		DeregisterWait *config.Duration `yaml:"deregister_wait"`
		// DrainTimeout is how long in-flight requests get to finish once the
		// listener is closed. Default 15s (Kitex's own default is 5s). Keep
		// deregister_wait + drain_timeout below the platform's kill timeout
		// (Kubernetes terminationGracePeriodSeconds defaults to 30s).
		DrainTimeout config.Duration `yaml:"drain_timeout"`
	} `yaml:"shutdown"`
}

const (
	// serviceListInterval is how often Nacos is asked for service names that
	// were not there before; instances of known services are pushed.
	serviceListInterval   = 3 * time.Second
	defaultDeregisterWait = 3 * time.Second
	defaultDrainTimeout   = 15 * time.Second
)

// Options is NewRuntime followed by Runtime.Options, for a service that does
// not need the Runtime (nothing enabled besides Nacos and logging).
func Options(cfg Config) ([]server.Option, error) {
	rt, err := NewRuntime(cfg)
	if err != nil {
		return nil, err
	}
	return rt.Options()
}

// Options builds the server options: basic info, listen address, tracing and
// logging middleware, the TTHeader meta handler, the drain timeout and the
// Nacos registry (unless disabled).
func (rt *Runtime) Options() ([]server.Option, error) {
	cfg := rt.Config
	addr, err := ListenAddr(cfg.Service.Addr)
	if err != nil {
		return nil, err
	}
	opts := []server.Option{
		server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: cfg.Service.Name}),
		server.WithServiceAddr(addr),
		// the limits first: a refused request costs no tracing, no log line
		// beyond the once-a-minute warning
		server.WithMiddleware(Limits(cfg.Limits)),
		server.WithMiddleware(ServerTracing()),
		server.WithMiddleware(ServerMetrics()),
		server.WithMiddleware(LoggingMiddleware()),
		server.WithMiddleware(ValidationMiddleware()),
		server.WithMiddleware(rt.Idempotency()),
		server.WithMetaHandler(transmeta.ServerTTHeaderHandler),
		server.WithExitWaitTime(DrainTimeout(cfg)),
	}
	opts = append(opts, connectionLimit(cfg.Limits)...)
	zlog.Info("limits on", zlog.Int("max_in_flight", int64(cfg.Limits.inFlight())), zlog.Int("max_qps", int64(cfg.Limits.MaxQPS)), zlog.Int("max_connections", int64(cfg.Limits.connections())))
	if !cfg.RegistryDisabled {
		cli, advertise, err := Registration(cfg, addr.Port)
		if err != nil {
			return nil, err
		}
		opts = append(opts, server.WithRegistry(&delayedRegistry{
			Registry: newNacosRegistry(cli, advertise),
			wait:     DeregisterWait(cfg),
		}))
	}
	return opts, nil
}

// Bootstrap is what a service of any kind does first, an RPC service in
// Options and an API service in hertzx.New: it checks service.name, installs
// the logger (Kitex's own records go through it too), lets the level follow
// Nacos when log_level_data_id says so, and sets up tracing (otel). What it
// fills into cfg.Log is for the "logger configured" record.
func Bootstrap(cfg *Config) error {
	if cfg.Service.Name == "" {
		return fmt.Errorf("kitexx: service.name is required")
	}
	// What is filled in here says so in the "logger configured" record, or the
	// value would look as if it had been written into the log section.
	if cfg.Log.Service == "" {
		cfg.Log.Service, cfg.Log.ServiceNote = cfg.Service.Name, "from service.name"
	}
	// The version every record and span carries: log.version if set, else
	// APP_VERSION, which the deployment sets to the image tag (the chart does;
	// CI's `go mod tidy` would otherwise mark the VCS revision "-dirty"), else
	// the VCS revision zlog reads from the binary.
	if cfg.Log.Version == "" {
		if v := strings.TrimSpace(os.Getenv(VersionEnv)); v != "" {
			cfg.Log.Version = v
		}
	}
	if cfg.Log.Env == "" {
		cfg.Log.Env, cfg.Log.EnvNote = config.Env(), "from "+config.EnvVar
		if os.Getenv(config.EnvVar) == "" {
			cfg.Log.EnvNote = config.EnvVar + " is not set"
		}
	}
	bridgeKlog(zlog.Init(cfg.Log))
	if err := initTracing(*cfg); err != nil {
		return err
	}
	if cfg.LogLevelDataID != "" {
		// The level is a convenience: a service must start without it.
		nc, err := Nacos(*cfg)
		if err == nil {
			err = watchLogLevel(nc, cfg.LogLevelDataID)
		}
		if err != nil {
			zlog.Warn("log level does not follow Nacos", zlog.Str("data_id", cfg.LogLevelDataID), zlog.Err(err))
		}
	}
	return nil
}

// initTracing installs the OpenTelemetry provider for the process and makes
// sure the spans still in the batch are exported when the server has stopped.
// A collector that cannot be reached is not a start failure (the exporter
// retries in the background); an endpoint that cannot be parsed is.
func initTracing(cfg Config) error {
	shutdown, err := otelx.Init(cfg.Otel, otelx.Identity{
		Service: cfg.Log.Service,
		Version: cfg.Log.Version,
		Env:     cfg.Log.Env,
	})
	if err != nil {
		return err
	}
	if !cfg.Otel.Enabled() {
		zlog.Info("tracing off", zlog.Str("hint", "set otel.endpoint to the OTLP collector to switch it on"))
		return nil
	}
	ratio := 1.0
	if cfg.Otel.SampleRatio != nil {
		ratio = *cfg.Otel.SampleRatio
	}
	zlog.Info("tracing on", zlog.Str("endpoint", cfg.Otel.Endpoint), zlog.Float("sample_ratio", ratio))
	OnShutdown("tracing flush", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return shutdown(ctx)
	})
	return nil
}

// Registration prepares what registering in Nacos needs, before anything is
// started: it refuses a developer's machine that would announce itself in a
// Nacos other people use, resolves service.advertise ("" when the listener's
// own address is to be registered), connects, and starts the record of which
// services are alive.
func Registration(cfg Config, listenPort int) (cli *nacosx.Client, advertise string, err error) {
	if err := cfg.Nacos.CheckRegistration(); err != nil {
		return nil, "", err
	}
	if advertise, err = advertiseAddr(cfg.Service.Advertise, listenPort); err != nil {
		return nil, "", err
	}
	if cli, err = Nacos(cfg); err != nil {
		return nil, "", err
	}
	if cfg.Nacos.WatchesServices() {
		cli.WatchServices(serviceListInterval)
	}
	return cli, advertise, nil
}

// DeregisterWait is how long a service keeps serving after it has left Nacos
// (shutdown.deregister_wait, default 3s), and DrainTimeout how long requests in
// progress get after that (shutdown.drain_timeout, default 15s).
func DeregisterWait(cfg Config) time.Duration {
	if cfg.Shutdown.DeregisterWait != nil {
		return cfg.Shutdown.DeregisterWait.Std()
	}
	return defaultDeregisterWait
}

func DrainTimeout(cfg Config) time.Duration { return cfg.Shutdown.DrainTimeout.Or(defaultDrainTimeout) }

// ClientOptions builds client options for calling other services: discovery
// through Nacos, and the TTHeader transport, which is what carries the
// trace_id of the request to the service that is called. With
// registry_disabled there is no discovery and the caller says where the
// service is, client.WithHostPorts.
func ClientOptions(cfg Config) ([]client.Option, error) {
	opts := append([]client.Option{
		client.WithTransportProtocol(transport.TTHeader),
		client.WithMetaHandler(transmeta.ClientTTHeaderHandler),
		client.WithMiddleware(ClientTracing()),
		client.WithMiddleware(ClientMetrics()),
	}, rpcClientOptions(cfg)...)
	if cfg.Service.Name != "" {
		// Who is calling: the "from" of the request log of the service called.
		opts = append(opts, client.WithClientBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: cfg.Service.Name}))
	}
	if cfg.RegistryDisabled {
		return opts, nil
	}
	cli, err := Nacos(cfg)
	if err != nil {
		return nil, err
	}
	return append(opts, client.WithResolver(newNacosResolver(cli, cfg.Nacos))), nil
}

// Run starts the server and blocks until it has stopped completely. With
// event subscriptions (rt.Kafka.Subscribe in app.Setup) it starts the
// consumers once the server is up and stops them with the shutdown hooks.
//
// Kitex itself reacts to SIGINT / SIGTERM / SIGHUP: it leaves Nacos, (with the
// delayed registry) keeps serving for deregister_wait, closes the listener and
// gives in-flight requests up to drain_timeout. Only after all of that does
// svr.Run return, which is when the OnShutdown hooks run.
func (rt *Runtime) Run(svr server.Server) error {
	if err := RunStarters(rt); err != nil {
		zlog.Error("cannot start", zlog.Err(err))
		return err
	}
	if rt.Kafka != nil {
		if err := rt.Kafka.Start(context.Background()); err != nil {
			zlog.Error("cannot start consumers", zlog.Err(err))
			return err
		}
	}
	return Run(svr, rt.Config)
}

// Run is Runtime.Run for a service without event subscriptions.
func Run(svr server.Server, cfg Config) error {
	defer zlog.Sync()
	zlog.Info("server starting", zlog.Str("addr", normalizeAddr(cfg.Service.Addr)), zlog.Bool("registry", !cfg.RegistryDisabled))
	err := svr.Run()
	RunShutdownHooks()
	nacosCloseShared()
	if err != nil {
		zlog.Error("server stopped with error", zlog.Err(err))
		return err
	}
	zlog.Info("server stopped")
	return nil
}

// TraceIDKey is the metainfo key under which the trace_id of a request travels
// from service to service, and traceIDField the field it is logged under.
const (
	TraceIDKey   = "TRACE_ID"
	traceIDField = "trace_id"
)

// LoggingMiddleware logs every RPC with method, caller, latency and error, and
// gives the context the logger of the request: what a handler logs with
// zlog.Ctx(ctx) carries trace_id and method.
//
// The trace_id is the id of the trace ServerTracing put into the context,
// which is the caller's or a new one; with tracing off it is the one the
// caller sent as TRACE_ID, or a new one for a request that starts here.
// Either way it is put back into the context as a persistent metainfo value,
// so the clients this service calls with that context pass it on (see
// ClientOptions), and a collector shows the whole chain under one id.
func LoggingMiddleware() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			start := time.Now()
			method, from := "unknown", ""
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if ri.Invocation() != nil {
					method = ri.Invocation().MethodName()
				}
				if ri.From() != nil {
					from = ri.From().ServiceName()
				}
			}
			ctx, traceID := ensureTraceID(ctx)
			ctx = zlog.CtxWith(ctx, zlog.Str(traceIDField, traceID), zlog.Str("method", method))
			if uid, ok := UID(ctx); ok {
				ctx = zlog.CtxWith(ctx, zlog.Int("uid", uid))
			}

			err := next(ctx, req, resp)

			fields := []zlog.Field{zlog.Dur("latency", time.Since(start))}
			if from != "" {
				fields = append(fields, zlog.Str("from", from))
			}
			if err != nil {
				zlog.Ctx(ctx).Error("rpc", append(fields, zlog.Err(err))...)
			} else {
				zlog.Ctx(ctx).Info("rpc", fields...)
			}
			return err
		}
	}
}

// ensureTraceID returns the trace_id of the request: the span's when there is
// one, else the TRACE_ID metainfo value, else a new one. It is stored as the
// TRACE_ID metainfo value (persistent, so it crosses every hop) when it is not
// already.
func ensureTraceID(ctx context.Context) (context.Context, string) {
	traceID := otelx.TraceID(ctx)
	if traceID == "" {
		traceID, _ = metainfo.GetPersistentValue(ctx, TraceIDKey)
	}
	if traceID == "" {
		traceID = NewTraceID()
	}
	if cur, _ := metainfo.GetPersistentValue(ctx, TraceIDKey); cur != traceID {
		ctx = metainfo.WithPersistentValue(ctx, TraceIDKey, traceID)
	}
	return ctx, traceID
}

// TraceID returns the trace_id of the request ctx belongs to, or "".
func TraceID(ctx context.Context) string {
	id, _ := metainfo.GetPersistentValue(ctx, TraceIDKey)
	return id
}

// NewTraceID returns a new trace_id: 32 hex characters.
func NewTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// defaultPort is where a service listens when service.addr is empty.
const defaultPort = "8888"

// normalizeAddr turns what service.addr may hold into "host:port": a bare
// port ("8888", which is what `addr: 8888` in YAML arrives as) listens on
// every interface.
func normalizeAddr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		v = defaultPort
	}
	if !strings.Contains(v, ":") {
		return ":" + v
	}
	return v
}

// advertiseAddr turns service.advertise into the "host:port" to register, ""
// when it is not set. Without a port it is the port the service listens on.
func advertiseAddr(v string, listenPort int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	const accepts = `a host such as "10.0.0.5", or "host:port" such as "10.0.0.5:30888"`
	host, port := v, strconv.Itoa(listenPort)
	if h, p, err := net.SplitHostPort(v); err == nil {
		host, port = h, p
	} else if strings.Contains(v, ":") && net.ParseIP(v) == nil {
		return "", fmt.Errorf("kitexx: service.advertise is %q; it must be %s", v, accepts)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("kitexx: service.advertise is %q: %q is not a port between 1 and 65535; it must be %s", v, port, accepts)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf("kitexx: service.advertise is %q: it has to name the host callers connect to; it must be %s", v, accepts)
	}
	return net.JoinHostPort(host, port), nil
}

// ListenAddr resolves service.addr. The errors name the setting and what it
// accepts, because the net package's own ("missing port in address") do not.
func ListenAddr(v string) (*net.TCPAddr, error) {
	const accepts = `a port such as 8888, or "host:port" such as "127.0.0.1:8888"`
	norm := normalizeAddr(v)
	_, port, err := net.SplitHostPort(norm)
	if err != nil {
		return nil, fmt.Errorf("kitexx: service.addr is %q; it must be %s", v, accepts)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("kitexx: service.addr is %q: %q is not a port between 1 and 65535; it must be %s", v, port, accepts)
	}
	addr, err := net.ResolveTCPAddr("tcp", norm)
	if err != nil {
		return nil, fmt.Errorf("kitexx: service.addr is %q: %v; it must be %s", v, err, accepts)
	}
	return addr, nil
}

func nacosCloseShared() { nacosx.CloseShared() }

// CallbacksConfig is the `callbacks:` section.
type CallbacksConfig struct {
	// Enabled starts the callback listener in rt.Run.
	Enabled bool `yaml:"enabled"`
	// Addr: a port ("8081", the default) or host:port. Never service.addr.
	Addr string `yaml:"addr"`
}

// DefaultCallbacksPort is the callback port when callbacks.addr is empty;
// the service chart exposes it.
const DefaultCallbacksPort = 8081

// ListenAddr resolves callbacks.addr.
func (c CallbacksConfig) ListenAddr() (string, error) {
	a := strings.TrimSpace(c.Addr)
	if a == "" {
		a = strconv.Itoa(DefaultCallbacksPort)
	}
	addr, err := ListenAddr(a)
	if err != nil {
		return "", fmt.Errorf("callbacks.addr: %w", err)
	}
	return addr.String(), nil
}

// OnStart registers something to start right before the RPC server does,
// in rt.Run: the callback listener, a poller. Its stop goes through
// OnShutdown as usual.
func (rt *Runtime) OnStart(name string, fn func() error) {
	rt.starters = append(rt.starters, starter{name, fn})
}

type starter struct {
	name string
	fn   func() error
}

// RunStarters runs what OnStart registered; rt.Run does it, tests may too.
func RunStarters(rt *Runtime) error {
	for _, st := range rt.starters {
		if err := st.fn(); err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return nil
}

// VersionEnv is the environment variable the deployment sets to the image
// tag; it becomes log.version (the `version` of every record and span) when
// the configuration does not set one.
const VersionEnv = "APP_VERSION"
