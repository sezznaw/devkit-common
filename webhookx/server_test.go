package webhookx

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// listen starts the callback listener of a runtime with one route of
// provider "pay" at path and returns its base URL and how often the handler
// ran. The listener is stopped when the test ends.
func listen(t *testing.T, cb kitexx.CallbacksConfig, path string) (base string, handled *atomic.Int32) {
	t.Helper()
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	rt.Config.Service.Name = "ser-vendor"
	port := freePort(t)
	cb.Enabled, cb.Addr = true, "127.0.0.1:"+port
	rt.Config.Callbacks = cb
	h := Server(rt)
	handled = &atomic.Int32{}
	Handle(rt, h, "pay", path, func(context.Context, *Event) error { handled.Add(1); return nil }, WithVerifier(func(*Event) error { return nil }))
	if err := startAll(rt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kitexx.RunShutdownHooks)
	base = "http://127.0.0.1:" + port
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err == nil {
			c.Close()
			return base, handled
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the callback listener did not come up")
	return "", nil
}

// oneShot closes its connection after every request, so that stopping the
// listener does not wait out its drain time for an idle connection.
var oneShot = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// allResults are the values of the result label of callbacks_received_total.
var allResults = []string{"ok", "duplicate", "bad_source", "bad_signature", "too_large", "archive_failed", "dedupe_unavailable", "handler_error"}

// received is callbacks_received_total of provider "pay" at path, summed
// over every result.
func received(path string) (n float64) {
	for _, res := range allResults {
		n += testutil.ToFloat64(metricsx.CallbacksReceived.WithLabelValues("pay", path, res))
	}
	return n
}

// TestServerBodyLimit: with callbacks.max_body_bytes the listener refuses a
// larger body before it reads it: 413, no route, no handler.
func TestServerBodyLimit(t *testing.T) {
	zlogtest.Capture(t)
	const path = "/callbacks/pay/limit"
	base, handled := listen(t, kitexx.CallbacksConfig{MaxBodyBytes: 4096, ReadTimeout: config.Duration(5 * time.Second)}, path)
	before := received(path)
	resp, err := oneShot.Post(base+path, "application/json", strings.NewReader(strings.Repeat("x", 8<<10)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("8 KiB against a 4 KiB listener limit: status %d, want 413", resp.StatusCode)
	}
	if handled.Load() != 0 || received(path) != before {
		t.Fatalf("the request reached the route: handler ran %d times, callbacks_received_total +%v", handled.Load(), received(path)-before)
	}
	resp, err = oneShot.Post(base+path, "application/json", strings.NewReader(strings.Repeat("x", 1<<10)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || handled.Load() != 1 {
		t.Fatalf("1 KiB: status %d, handler ran %d times; want 200 and 1", resp.StatusCode, handled.Load())
	}
}

// TestServerDefaultLimitUnchanged: without callbacks.max_body_bytes the
// listener behaves as before (Hertz's 4 MiB): a 2 MiB body is read and it is
// the route's own max_body that refuses it. A warning at start asks for the
// setting.
func TestServerDefaultLimitUnchanged(t *testing.T) {
	logs := zlogtest.Capture(t)
	const path = "/callbacks/pay/nolimit"
	base, handled := listen(t, kitexx.CallbacksConfig{}, path)
	if !logs.Has("WARN", "callbacks.max_body_bytes is not set: the listener reads bodies up to 4 MiB before any route's max_body applies") {
		t.Errorf("no warning about the unset limit:\n%s", logs)
	}
	before := testutil.ToFloat64(metricsx.CallbacksReceived.WithLabelValues("pay", path, "too_large"))
	resp, err := oneShot.Post(base+path, "application/json", strings.NewReader(strings.Repeat("x", 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	after := testutil.ToFloat64(metricsx.CallbacksReceived.WithLabelValues("pay", path, "too_large"))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || after != before+1 || handled.Load() != 0 {
		t.Fatalf("2 MiB without a listener limit: status %d, too_large +%v, handler ran %d times; want the route's 413", resp.StatusCode, after-before, handled.Load())
	}
}

// TestHandleRejectsMaxBodyAboveListener: a provider's max_body the listener
// would never let through is a configuration mistake found at start.
func TestHandleRejectsMaxBodyAboveListener(t *testing.T) {
	zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}, MaxBody: 2 << 20})
	rt.Config.Callbacks = kitexx.CallbacksConfig{Enabled: true, MaxBodyBytes: 1 << 20}
	verifier := WithVerifier(func(*Event) error { return nil })
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "max_body") || !strings.Contains(fmt.Sprint(r), "callbacks.max_body_bytes") {
				t.Errorf("max_body above the listener's limit must panic naming both settings, got %v", r)
			}
		}()
		Handle(rt, server.New(server.WithDisablePrintRoute(true)), "pay", "/cb", func(context.Context, *Event) error { return nil }, verifier)
	}()
	// at the limit, or no listener limit: fine
	rt.Config.Callbacks.MaxBodyBytes = 2 << 20
	Handle(rt, server.New(server.WithDisablePrintRoute(true)), "pay", "/cb", func(context.Context, *Event) error { return nil }, verifier)
	rt.Config.Callbacks.MaxBodyBytes = 0
	Handle(rt, server.New(server.WithDisablePrintRoute(true)), "pay", "/cb", func(context.Context, *Event) error { return nil }, verifier)
}

// TestHandlePresetsResultSeries: every result series of a route exists at 0
// from registration on, so that an alert on the first refusals is not blind
// (increase() needs a sample before the first increment).
func TestHandlePresetsResultSeries(t *testing.T) {
	zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	// the registry is the process's: a path of its own for every run (-count)
	path := "/callbacks/pay/preset-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seriesOf := func() map[string]float64 {
		got := map[string]float64{}
		mfs, err := metricsx.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() != "callbacks_received_total" {
				continue
			}
			for _, m := range mf.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if labels["provider"] == "pay" && labels["http_route"] == path {
					got[labels["result"]] = m.GetCounter().GetValue()
				}
			}
		}
		return got
	}
	if got := seriesOf(); len(got) != 0 {
		t.Fatalf("series before registration: %v", got)
	}
	Handle(rt, server.New(server.WithDisablePrintRoute(true)), "pay", path, func(context.Context, *Event) error { return nil }, WithVerifier(func(*Event) error { return nil }))
	got := seriesOf()
	for _, res := range allResults {
		if v, ok := got[res]; !ok || v != 0 {
			t.Errorf("result %q: present=%v value=%v, want a series at 0", res, ok, v)
		}
	}
}

func TestServerStartsWithTheRuntime(t *testing.T) {
	zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	rt.Config.Service.Name = "ser-vendor"
	rt.Config.Callbacks = kitexx.CallbacksConfig{Enabled: true, Addr: "127.0.0.1:0"}
	// Hertz needs a real port to report; pick one.
	port := freePort(t)
	rt.Config.Callbacks.Addr = "127.0.0.1:" + port
	h := Server(rt)
	if Server(rt) != h {
		t.Fatal("one engine per runtime")
	}
	Handle(rt, h, "pay", "/callbacks/pay/x", func(context.Context, *Event) error { return nil }, WithVerifier(func(*Event) error { return nil }))
	// rt.Run would call the starters; call them the way it does.
	if err := startAll(rt); err != nil {
		t.Fatal(err)
	}
	defer kitexx.RunShutdownHooks()
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = http.Post("http://127.0.0.1:"+port+"/callbacks/pay/x", "application/json", strings.NewReader("{}"))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "ok" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	rt2 := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	defer func() {
		if r := recover(); r == nil {
			t.Error("callbacks.enabled false panics")
		}
	}()
	Server(rt2)
}
