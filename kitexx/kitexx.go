// Package kitexx wires a Kitex server or client to the company infrastructure:
// Nacos registration / discovery, structured logging and request logging
// middleware. A service's main() should only need Options() and Run().
package kitexx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/server"
	nacosregistry "github.com/kitex-contrib/registry-nacos/v2/registry"
	nacosresolver "github.com/kitex-contrib/registry-nacos/v2/resolver"

	"github.com/sezznaw/devkit-common/log"
	"github.com/sezznaw/devkit-common/nacosx"
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
	Log   log.Options   `yaml:"log"`
	// RegistryDisabled skips Nacos registration (handy for local runs).
	RegistryDisabled bool `yaml:"registry_disabled"`
}

// Options builds the server options: basic info, listen address, Nacos
// registry (unless disabled) and logging middleware. It also installs the
// default logger.
func Options(cfg Config) ([]server.Option, error) {
	if cfg.Service.Name == "" {
		return nil, fmt.Errorf("kitexx: service.name is required")
	}
	if cfg.Log.Service == "" {
		cfg.Log.Service = cfg.Service.Name
	}
	l := log.New(cfg.Log)

	addr, err := net.ResolveTCPAddr("tcp", orDefault(cfg.Service.Addr, ":8888"))
	if err != nil {
		return nil, fmt.Errorf("kitexx: bad service.addr: %w", err)
	}
	opts := []server.Option{
		server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: cfg.Service.Name}),
		server.WithServiceAddr(addr),
		server.WithMiddleware(LoggingMiddleware(l)),
	}
	if !cfg.RegistryDisabled {
		cli, err := nacosx.NewNamingClient(cfg.Nacos)
		if err != nil {
			return nil, err
		}
		opts = append(opts, server.WithRegistry(nacosregistry.NewNacosRegistry(cli,
			nacosregistry.WithGroup(cfg.Nacos.GroupName()))))
	}
	return opts, nil
}

// ClientOptions builds client options for calling other services through Nacos.
func ClientOptions(cfg Config) ([]client.Option, error) {
	cli, err := nacosx.NewNamingClient(cfg.Nacos)
	if err != nil {
		return nil, err
	}
	return []client.Option{
		client.WithResolver(nacosresolver.NewNacosResolver(cli,
			nacosresolver.WithGroup(cfg.Nacos.GroupName()))),
	}, nil
}

// Run starts the server and blocks until it stops. Kitex already handles
// SIGINT/SIGTERM with a graceful stop and deregistration.
func Run(svr server.Server, cfg Config) error {
	slog.Info("server starting", "addr", orDefault(cfg.Service.Addr, ":8888"))
	if err := svr.Run(); err != nil {
		slog.Error("server stopped with error", "err", err)
		return err
	}
	slog.Info("server stopped")
	return nil
}

// LoggingMiddleware logs every RPC with method, latency and error, and puts a
// request-scoped logger into the context for handlers.
func LoggingMiddleware(l *slog.Logger) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			start := time.Now()
			method := "unknown"
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil && ri.Invocation() != nil {
				method = ri.Invocation().MethodName()
			}
			rl := l.With("method", method)
			ctx = log.WithContext(ctx, rl)
			err := next(ctx, req, resp)
			if err != nil {
				rl.Error("rpc", "latency", time.Since(start), "err", err)
			} else {
				rl.Info("rpc", "latency", time.Since(start))
			}
			return err
		}
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
