// Package hertzx starts an API service (HTTP, CloudWeGo Hertz) the way kitexx
// starts an RPC service: the same configuration, the same logger, the same
// rules for Nacos, the same stop.
//
//	var cfg app.Config                       // embeds hertzx.Config
//	config.LoadDefault(&cfg)
//	h, err := hertzx.New(cfg.Config)         // logger, listener, request log, recovery
//	app.Setup(&cfg, h)                       // the service: RPC clients, middleware
//	router.GeneratedRegister(h)              // the routes hz generated from the IDL
//	err = hertzx.Run(h, cfg.Config)          // serve until SIGINT/SIGTERM, stop gracefully
package hertzx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	hconfig "github.com/cloudwego/hertz/pkg/common/config"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the configuration of an RPC service, key for key: service, nacos,
// log, shutdown and the rest mean the same, conf/*.yaml look the same, and
// kitexx.ClientOptions(cfg) is how an API service calls the RPC services
// behind it.
type Config = kitexx.Config

// TraceHeader carries the trace_id of a request: taken from the request when
// the caller sends one that looks like an id, always set on the response.
const TraceHeader = "X-Trace-Id"

// OnShutdown registers cleanup that runs once the server has stopped
// completely; it is kitexx.OnShutdown.
func OnShutdown(name string, fn func() error) { kitexx.OnShutdown(name, fn) }

// New installs the logger and returns a Hertz server that listens on
// service.addr, logs every request with a trace_id and turns a panic in a
// handler into a record and a 500. It refuses, before anything is started, what
// kitexx.Options refuses: a service without a name, a Nacos that cannot be
// reached or does not accept the account, a laptop that would register in a
// Nacos other people use.
func New(cfg Config, opts ...hconfig.Option) (*server.Hertz, error) {
	if err := kitexx.Bootstrap(&cfg); err != nil {
		return nil, err
	}
	bridgeHlog(zlog.Default())
	addr, err := kitexx.ListenAddr(cfg.Service.Addr)
	if err != nil {
		return nil, err
	}
	if !cfg.RegistryDisabled {
		if _, _, err := kitexx.Registration(cfg, addr.Port); err != nil {
			return nil, err
		}
	}
	all := append([]hconfig.Option{
		server.WithHostPorts(addr.String()),
		server.WithExitWaitTime(kitexx.DrainTimeout(cfg)),
		// Hertz prints the routes as debug records of its own; one per route
		// at every start is noise next to the request log.
		server.WithDisablePrintRoute(true),
	}, opts...)
	h := server.New(all...)
	h.Use(RequestLog(), Recovery())
	return h, nil
}

// Run serves until SIGINT, SIGTERM or SIGHUP and then stops in the order that
// loses no request: leave Nacos, keep serving for shutdown.deregister_wait
// while callers and gateways notice, close the listener, give requests in
// progress up to shutdown.drain_timeout, run the OnShutdown hooks.
//
// Hertz's own Spin does these at the same time, and its registry registers a
// second after the start whether or not the listener is up; so the service is
// registered here, once it really listens.
func Run(h *server.Hertz, cfg Config) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)
	return run(h, cfg, stop)
}

func run(h *server.Hertz, cfg Config, stop <-chan os.Signal) error {
	defer zlog.Sync()
	addr, err := kitexx.ListenAddr(cfg.Service.Addr)
	if err != nil {
		return err
	}
	zlog.Info("server starting", zlog.Str("addr", addr.String()), zlog.Str("kind", "http"), zlog.Bool("registry", !cfg.RegistryDisabled))

	// Hertz panics, with a goroutine dump, when the port is taken. Two services
	// of a project on the same port is the most likely mistake on a developer's
	// machine, so it gets a sentence instead.
	if l, err := net.Listen("tcp", addr.String()); err != nil {
		err = fmt.Errorf("hertzx: cannot listen on %s: %w. Is another service using the port? Give this one its own service.addr", addr, err)
		kitexx.RunShutdownHooks()
		zlog.Error("server stopped with error", zlog.Err(err))
		return err
	} else {
		l.Close()
	}

	failed := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				failed <- fmt.Errorf("hertzx: the server failed: %v", r)
			}
		}()
		failed <- h.Run()
	}()
	if err := waitListening(addr, failed, 10*time.Second); err != nil {
		kitexx.RunShutdownHooks()
		zlog.Error("server stopped with error", zlog.Err(err))
		return err
	}

	deregister := func() error { return nil }
	if !cfg.RegistryDisabled {
		cli, advertise, err := kitexx.Registration(cfg, addr.Port)
		if err == nil {
			reg := nacosx.Registration{Service: cfg.Service.Name, Addr: addr.String(), Metadata: map[string]string{"protocol": "http"}}
			if advertise != "" {
				reg.Addr = advertise
			}
			deregister, err = cli.Register(reg)
		}
		if err != nil {
			_ = h.Shutdown(context.Background())
			kitexx.RunShutdownHooks()
			zlog.Error("server stopped with error", zlog.Err(err))
			return err
		}
	}

	select {
	case err = <-failed: // the server gave up by itself
	case sig := <-stop:
		zlog.Info("stopping", zlog.Str("signal", sig.String()))
	}
	if derr := deregister(); derr != nil {
		zlog.Warn("could not deregister from Nacos", zlog.Err(derr))
	} else if wait := kitexx.DeregisterWait(cfg); !cfg.RegistryDisabled && cfg.Nacos.Registers() && wait > 0 && err == nil {
		zlog.Info("deregistered; still serving while callers refresh their instance lists", zlog.Dur("wait", wait))
		time.Sleep(wait)
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), kitexx.DrainTimeout(cfg))
		if serr := h.Shutdown(ctx); serr != nil && !errors.Is(serr, context.Canceled) {
			zlog.Warn("requests were still in progress when the drain timeout ended", zlog.Err(serr))
		}
		cancel()
	}
	kitexx.RunShutdownHooks()
	if err != nil {
		zlog.Error("server stopped with error", zlog.Err(err))
		return err
	}
	zlog.Info("server stopped")
	return nil
}

// waitListening returns once something accepts connections on addr, or with the
// error the server failed with (the port is taken, for example).
func waitListening(addr *net.TCPAddr, failed <-chan error, limit time.Duration) error {
	host := addr.IP.String()
	if addr.IP == nil || addr.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	target := net.JoinHostPort(host, fmt.Sprint(addr.Port))
	deadline := time.Now().Add(limit)
	for {
		select {
		case err := <-failed:
			if err == nil {
				err = errors.New("hertzx: the server stopped before it was listening")
			}
			return err
		default:
		}
		if conn, err := net.DialTimeout("tcp", target, 200*time.Millisecond); err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hertzx: nothing is listening on %s after %s", target, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// RequestLog logs every request (method, path, status, latency, client) and
// gives the context the logger of the request: what a handler logs with
// zlog.Ctx(ctx) carries trace_id, method and path.
//
// The trace_id is the one of the X-Trace-Id request header when that looks like
// an id, otherwise a new one. It is put into the context the way kitexx does,
// so the RPC clients a handler calls with that context pass it on, and it is
// set on the response, so whoever reports a problem can say which request.
// 5xx is an error record, 4xx a warning.
func RequestLog() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		traceID := string(c.GetHeader(TraceHeader))
		if !looksLikeID(traceID) {
			traceID = kitexx.NewTraceID()
		}
		c.Response.Header.Set(TraceHeader, traceID)
		ctx = metainfo.WithPersistentValue(ctx, kitexx.TraceIDKey, traceID)
		ctx = zlog.CtxWith(ctx, zlog.Str("trace_id", traceID),
			zlog.Str("method", string(c.Method())), zlog.Str("path", string(c.Path())))

		c.Next(ctx)

		status := c.Response.StatusCode()
		fields := []zlog.Field{zlog.Int("status", status), zlog.Dur("latency", time.Since(start)), zlog.Str("client", c.ClientIP())}
		switch {
		case status >= 500:
			zlog.Ctx(ctx).Error("http", fields...)
		case status >= 400:
			zlog.Ctx(ctx).Warn("http", fields...)
		default:
			zlog.Ctx(ctx).Info("http", fields...)
		}
	}
}

// looksLikeID keeps what comes from outside out of the log unless it is what a
// trace id is made of: 8 to 64 letters, digits, '-' or '_'.
func looksLikeID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Recovery turns a panic in a handler into one error record, with the stack in
// "panic_stack", and a 500 without a body. It comes after RequestLog, so the
// record has the trace_id and the request is still logged, as a 500.
func Recovery() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		defer func() {
			if r := recover(); r != nil {
				zlog.Ctx(ctx).Error("panic in handler", zlog.Any("panic", r), zlog.Str("panic_stack", string(debug.Stack())))
				c.AbortWithStatus(500)
			}
		}()
		c.Next(ctx)
	}
}
