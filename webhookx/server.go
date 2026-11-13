package webhookx

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	hconfig "github.com/cloudwego/hertz/pkg/common/config"

	"github.com/sezznaw/devkit-common/hertzx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

var servers sync.Map // *kitexx.Runtime -> *server.Hertz

// UseClientRule installs on h the rule the callback listener judges a
// request's source by (the `client:` section: trusted_proxies and edges),
// which is what Event.ClientIP, allow_cidrs and the archive's "client" then
// hold: the engine's ClientIP function; the same function on every request,
// because a request served in process (ut.PerformRequest, a test) never
// gets the engine's and would fall back to Hertz's rule, the leftmost
// X-Forwarded-For entry, which the caller chooses; and, with client.edges,
// the Edge middleware, which refuses what no edge vouched for when
// edges.required is set and removes the key header so that it never reaches
// a handler or the archive. Server calls it for the listener. A test that
// mounts callback routes on an engine of its own calls it before Handle,
// with the `client:` section of the configuration it wants to prove, so
// that it tests the rule production runs and not Hertz's default. An error
// is a `client:` section that cannot work (a trusted proxy that is no CIDR,
// a short or shared edge key).
//
// UseClientRule 把回调监听判断来源地址的那条规则（`client:` 段的 trusted_proxies 和 edges）
// 装到 h 上：引擎的 ClientIP 函数、每个请求上的同一个函数（进程内请求拿不到引擎的函数，会退回
// Hertz 默认规则——取 X-Forwarded-For 最左一项，那是调用方自己能写的）、以及配了 client.edges
// 时的 Edge 中间件。Server 自己调它；在自己的引擎上挂回调路由的测试也调它（在 Handle 之前，
// 传要验证的那份配置的 `client:` 段），测的才是生产跑的规则。
func UseClientRule(h *server.Hertz, c kitexx.ClientConfig) error {
	trusted, err := c.Trusted()
	if err != nil {
		return err
	}
	edges, err := c.EdgeSet()
	if err != nil {
		return err
	}
	resolve := hertzx.ClientIP(trusted, edges)
	h.SetClientIPFunc(resolve)
	h.Use(func(_ context.Context, rc *app.RequestContext) { rc.SetClientIPFunc(resolve) })
	if edges.Enabled() {
		h.Use(hertzx.Edge(trusted, edges)) // the key header never reaches the archive
	}
	return nil
}

// Server is the HTTP listener of a Kitex service for the callbacks its
// providers send (callbacks.enabled, port 8081 by default): a Hertz engine
// with the framework's tracing, metrics, request log and recovery, started
// by rt.Run right before the RPC server and stopped with it. Register
// routes on it with Handle. The same engine is returned on every call.
// With callbacks.enabled false it panics: a route registered on a listener
// that never starts is a mistake to find at start.
func Server(rt *kitexx.Runtime) *server.Hertz {
	if h, ok := servers.Load(rt); ok {
		return h.(*server.Hertz)
	}
	cfg := rt.Config.Callbacks
	if !cfg.Enabled {
		panic("webhookx: callbacks.enabled is false in conf/<env>.yaml; the callback listener would never start")
	}
	addr, err := cfg.ListenAddr()
	if err != nil {
		panic(err)
	}
	opts := []hconfig.Option{server.WithHostPorts(addr), server.WithDisablePrintRoute(true), server.WithExitWaitTime(kitexx.DrainTimeout(rt.Config))}
	// callbacks.max_body_bytes: Hertz allocates a request's whole body by its
	// Content-Length and reads it before any route runs, so a route's own
	// max_body cannot keep a large body out of memory; only this limit does
	// (413 before the body is read). Unset keeps Hertz's 4 MiB, because the
	// listener also carries routes mounted on the engine directly, whose
	// largest legitimate body the framework cannot know.
	if n := cfg.MaxBodyBytes; n > 0 {
		opts = append(opts, server.WithMaxRequestBodySize(n), server.WithMaxKeepBodySize(min(n, 64<<10)))
	} else {
		zlog.Warn("callbacks.max_body_bytes is not set: the listener reads bodies up to 4 MiB before any route's max_body applies")
	}
	if d := cfg.ReadTimeout.Or(0); d > 0 {
		opts = append(opts, server.WithReadTimeout(d))
	}
	h := server.New(opts...)
	h.Use(hertzx.Tracing(), hertzx.Metrics(), hertzx.RequestLog(), hertzx.Recovery())
	// allow_cidrs and the logs need the vendor's real address: only the
	// trusted proxies' X-Forwarded-For entries count (client.trusted_proxies),
	// or what an edge wrote (client.edges). After the request log, so that a
	// request the edges refuse is a record.
	if err := UseClientRule(h, rt.Config.Client); err != nil {
		panic(err)
	}
	if prev, loaded := servers.LoadOrStore(rt, h); loaded {
		return prev.(*server.Hertz)
	}
	rt.OnStart("callback listener", func() error {
		// Probe the port first: Hertz panics when it is taken.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("webhookx: listen on %s: %w", addr, err)
		}
		ln.Close()
		go func() {
			if err := h.Run(); err != nil {
				zlog.Error("callback listener stopped with error", zlog.Err(err))
			}
		}()
		zlog.Info("callback listener starting", zlog.Str("addr", addr), zlog.Str("path", "/callbacks/..."))
		return nil
	})
	kitexx.OnShutdown("callback listener", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), kitexx.DrainTimeout(rt.Config)+time.Second)
		defer cancel()
		return h.Shutdown(ctx)
	})
	return h
}
