package kitexx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/kitex/pkg/klog"
	"github.com/cloudwego/kitex/pkg/registry"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/log"
)

type fakeRegistry struct{ deregisteredAt time.Time }

func (f *fakeRegistry) Register(*registry.Info) error { return nil }
func (f *fakeRegistry) Deregister(*registry.Info) error {
	f.deregisteredAt = time.Now()
	return nil
}

func TestDelayedRegistryWaitsAfterDeregistering(t *testing.T) {
	log.New(log.Options{Output: &bytes.Buffer{}})
	inner := &fakeRegistry{}
	d := &delayedRegistry{Registry: inner, wait: 150 * time.Millisecond}
	start := time.Now()
	if err := d.Deregister(nil); err != nil {
		t.Fatal(err)
	}
	returned := time.Now()
	if inner.deregisteredAt.Sub(start) > 50*time.Millisecond {
		t.Error("the real deregistration must happen first, not after the wait")
	}
	if returned.Sub(inner.deregisteredAt) < 140*time.Millisecond {
		t.Errorf("listener would close %v after deregistering; want >= 150ms", returned.Sub(inner.deregisteredAt))
	}
	zero := &delayedRegistry{Registry: &fakeRegistry{}, wait: 0}
	start = time.Now()
	zero.Deregister(nil)
	if time.Since(start) > 50*time.Millisecond {
		t.Error("wait=0 must not delay")
	}
}

func TestShutdownHooksRunInReverseAndSurviveFailures(t *testing.T) {
	var buf bytes.Buffer
	log.New(log.Options{Output: &buf})
	var order []string
	OnShutdown("db", func() error { order = append(order, "db"); return nil })
	OnShutdown("cache", func() error { order = append(order, "cache"); return errors.New("close failed") })
	OnShutdown("consumer", func() error { order = append(order, "consumer"); panic("boom") })
	runShutdownHooks()
	if strings.Join(order, ",") != "consumer,cache,db" {
		t.Fatalf("order = %v; want last registered first, and all of them despite panic/error", order)
	}
	out := buf.String()
	for _, want := range []string{"shutdown hook panicked", "hook=consumer", "shutdown hook failed", "hook=cache", "shutdown hook done", "hook=db"} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
	order = nil
	runShutdownHooks()
	if len(order) != 0 {
		t.Error("hooks must run only once")
	}
}

func TestKlogGoesThroughSlogAsSingleRecords(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(log.Options{Output: &buf, JSON: true})
	bridgeKlog(l, slog.LevelInfo)
	klog.Infof("KITEX: server listen at addr=%s", "[::]:8888")
	klog.Debugf("hidden at info level")
	klog.Errorf("KITEX: processing request error, error=panic: boom\ngoroutine 1 [running]:\nmain.go:10")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want exactly 2 JSON records (debug dropped, multi-line panic kept as one), got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"msg":"server listen at addr=[::]:8888"`) || !strings.Contains(lines[0], `"logger":"kitex"`) || !strings.Contains(lines[0], `"level":"INFO"`) {
		t.Errorf("info record wrong: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"level":"ERROR"`) || !strings.Contains(lines[1], `"stack":"goroutine 1 [running]:\nmain.go:10"`) {
		t.Errorf("panic record must carry the stack as one attribute: %s", lines[1])
	}
}

func TestShutdownDefaultsAndOverrides(t *testing.T) {
	var cfg Config
	if cfg.Shutdown.DrainTimeout.Or(defaultDrainTimeout) != 15*time.Second {
		t.Error("default drain timeout must be 15s")
	}
	zero := config.Duration(0)
	cfg.Shutdown.DeregisterWait = &zero
	if cfg.Shutdown.DeregisterWait.Std() != 0 {
		t.Error("an explicit 0s must be respected (pointer distinguishes unset from zero)")
	}
}
