package kitexx

import (
	"sync"
	"time"

	"github.com/cloudwego/kitex/pkg/registry"

	"github.com/sezznaw/devkit-common/zlog"
)

// delayedRegistry keeps the listener open for a while after deregistering.
//
// Kitex stops in the order: deregister, then close the listener at once.
// Callers, however, keep using their cached instance list until Nacos has
// pushed the change, so for a second or two they still dial this instance and
// get connection errors, which shows up as a burst of failures on every
// rolling deploy. Waiting after the deregistration lets that traffic finish
// normally.
type delayedRegistry struct {
	registry.Registry
	wait time.Duration
}

func (d *delayedRegistry) Deregister(info *registry.Info) error {
	err := d.Registry.Deregister(info)
	if d.wait > 0 {
		zlog.Info("deregistered; still serving while callers refresh their instance lists", zlog.Dur("wait", d.wait))
		time.Sleep(d.wait)
	}
	return err
}

var (
	hooksMu sync.Mutex
	hooks   []namedHook
)

type namedHook struct {
	name string
	fn   func() error
}

// OnShutdown registers cleanup for resources the service owns (database pools,
// Redis, message consumers, ...). Hooks run in reverse registration order after
// the server has deregistered and every in-flight request has finished, so a
// handler never sees its database closed underneath it.
//
// Do not use Kitex's server.RegisterShutdownHook for this: those hooks run
// before deregistration, while requests are still being served.
func OnShutdown(name string, fn func() error) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	hooks = append(hooks, namedHook{name, fn})
}

// runShutdownHooks runs every hook even if an earlier one fails or panics.
func runShutdownHooks() {
	hooksMu.Lock()
	list := hooks
	hooks = nil
	hooksMu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		h := list[i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					zlog.Error("shutdown hook panicked", zlog.Str("hook", h.name), zlog.Any("panic", r))
				}
			}()
			start := time.Now()
			if err := h.fn(); err != nil {
				zlog.Error("shutdown hook failed", zlog.Str("hook", h.name), zlog.Err(err))
				return
			}
			zlog.Info("shutdown hook done", zlog.Str("hook", h.name), zlog.Dur("took", time.Since(start).Round(time.Millisecond)))
		}()
	}
}
