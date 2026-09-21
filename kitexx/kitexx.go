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
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/server"
	"github.com/cloudwego/kitex/transport"
	nacosregistry "github.com/kitex-contrib/registry-nacos/v2/registry"
	nacosresolver "github.com/kitex-contrib/registry-nacos/v2/resolver"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the part of a service config that kitexx understands. Embed it in
// the service's own config struct.
type Config struct {
	Service struct {
		// Name is the registered service name, e.g. "order".
		Name string `yaml:"name"`
		// Addr is the listen address, e.g. ":8888".
		Addr string `yaml:"addr"`
	} `yaml:"service"`
	Nacos nacosx.Config `yaml:"nacos"`
	Log   zlog.Options  `yaml:"log"`
	// LogLevelDataID names a Nacos configuration whose content is a log level
	// (debug, info, warn, error). The level of the running service follows
	// it, which is how debug logging is switched on in production without a
	// restart. Empty: the level stays what log.level says.
	LogLevelDataID string `yaml:"log_level_data_id"`
	// RegistryDisabled skips Nacos registration (handy for local runs).
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
	defaultDeregisterWait = 3 * time.Second
	defaultDrainTimeout   = 15 * time.Second
)

// Options builds the server options: basic info, listen address, Nacos
// registry (unless disabled) and logging middleware. It also installs the
// default logger.
func Options(cfg Config) ([]server.Option, error) {
	if cfg.Service.Name == "" {
		return nil, fmt.Errorf("kitexx: service.name is required")
	}
	// What is filled in here says so in the "logger configured" record, or the
	// value would look as if it had been written into the log section.
	if cfg.Log.Service == "" {
		cfg.Log.Service, cfg.Log.ServiceNote = cfg.Service.Name, "from service.name"
	}
	if cfg.Log.Env == "" {
		cfg.Log.Env, cfg.Log.EnvNote = config.Env(), "from "+config.EnvVar
		if os.Getenv(config.EnvVar) == "" {
			cfg.Log.EnvNote = config.EnvVar + " is not set"
		}
	}
	bridgeKlog(zlog.Init(cfg.Log))
	if cfg.LogLevelDataID != "" {
		// The level is a convenience: a service must start without it.
		if err := watchLogLevelFromNacos(cfg); err != nil {
			zlog.Warn("log level does not follow Nacos", zlog.Str("data_id", cfg.LogLevelDataID), zlog.Err(err))
		}
	}

	addr, err := net.ResolveTCPAddr("tcp", orDefault(cfg.Service.Addr, ":8888"))
	if err != nil {
		return nil, fmt.Errorf("kitexx: bad service.addr: %w", err)
	}
	opts := []server.Option{
		server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: cfg.Service.Name}),
		server.WithServiceAddr(addr),
		server.WithMiddleware(LoggingMiddleware()),
		server.WithMetaHandler(transmeta.ServerTTHeaderHandler),
		server.WithExitWaitTime(cfg.Shutdown.DrainTimeout.Or(defaultDrainTimeout)),
	}
	if !cfg.RegistryDisabled {
		cli, err := nacosx.SharedNamingClient(cfg.Nacos)
		if err != nil {
			return nil, fmt.Errorf("%w. To run without Nacos, for example on your own machine, set registry_disabled: true", err)
		}
		wait := defaultDeregisterWait
		if cfg.Shutdown.DeregisterWait != nil {
			wait = cfg.Shutdown.DeregisterWait.Std()
		}
		opts = append(opts, server.WithRegistry(&delayedRegistry{
			Registry: nacosregistry.NewNacosRegistry(cli, nacosregistry.WithGroup(cfg.Nacos.GroupName())),
			wait:     wait,
		}))
	}
	return opts, nil
}

// ClientOptions builds client options for calling other services: discovery
// through Nacos, and the TTHeader transport, which is what carries the
// trace_id of the request to the service that is called. With
// registry_disabled there is no discovery and the caller says where the
// service is, client.WithHostPorts.
func ClientOptions(cfg Config) ([]client.Option, error) {
	opts := []client.Option{
		client.WithTransportProtocol(transport.TTHeader),
		client.WithMetaHandler(transmeta.ClientTTHeaderHandler),
	}
	if cfg.Service.Name != "" {
		// Who is calling: the "from" of the request log of the service called.
		opts = append(opts, client.WithClientBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: cfg.Service.Name}))
	}
	if cfg.RegistryDisabled {
		return opts, nil
	}
	cli, err := nacosx.SharedNamingClient(cfg.Nacos)
	if err != nil {
		return nil, err
	}
	return append(opts, client.WithResolver(nacosresolver.NewNacosResolver(cli,
		nacosresolver.WithGroup(cfg.Nacos.GroupName())))), nil
}

// Run starts the server and blocks until it has stopped completely.
//
// Kitex itself reacts to SIGINT / SIGTERM / SIGHUP: it leaves Nacos, (with the
// delayed registry) keeps serving for deregister_wait, closes the listener and
// gives in-flight requests up to drain_timeout. Only after all of that does
// svr.Run return, which is when the OnShutdown hooks run.
func Run(svr server.Server, cfg Config) error {
	defer zlog.Sync()
	zlog.Info("server starting", zlog.Str("addr", orDefault(cfg.Service.Addr, ":8888")), zlog.Bool("registry", !cfg.RegistryDisabled))
	err := svr.Run()
	runShutdownHooks()
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
// The trace_id is the one the caller sent, or a new one for a request that
// starts here. It is put back into the context as a persistent metainfo value,
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
			traceID, ok := metainfo.GetPersistentValue(ctx, TraceIDKey)
			if !ok || traceID == "" {
				traceID = newTraceID()
				ctx = metainfo.WithPersistentValue(ctx, TraceIDKey, traceID)
			}
			ctx = zlog.CtxWith(ctx, zlog.Str(traceIDField, traceID), zlog.Str("method", method))

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

// TraceID returns the trace_id of the request ctx belongs to, or "".
func TraceID(ctx context.Context) string {
	id, _ := metainfo.GetPersistentValue(ctx, TraceIDKey)
	return id
}

func newTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
