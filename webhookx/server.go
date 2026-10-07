package webhookx

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/sezznaw/devkit-common/hertzx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

var servers sync.Map // *kitexx.Runtime -> *server.Hertz

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
	h := server.New(server.WithHostPorts(addr), server.WithDisablePrintRoute(true), server.WithExitWaitTime(kitexx.DrainTimeout(rt.Config)))
	h.Use(hertzx.Tracing(), hertzx.Metrics(), hertzx.RequestLog(), hertzx.Recovery())
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
