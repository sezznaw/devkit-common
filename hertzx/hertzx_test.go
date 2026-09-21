package hertzx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func engine(routes func(h *server.Hertz)) *server.Hertz {
	h := server.New(server.WithDisablePrintRoute(true))
	h.Use(RequestLog(), Recovery())
	routes(h)
	return h
}

// Every request is one record, and what the handler logs belongs to it: the
// same trace_id, which also goes on to the RPC services and back to the client.
func TestRequestLog(t *testing.T) {
	logs := zlogtest.Capture(t)
	var inHandler, passedOn string
	h := engine(func(h *server.Hertz) {
		h.GET("/user/:id", func(ctx context.Context, c *app.RequestContext) {
			inHandler = kitexx.TraceID(ctx)
			passedOn, _ = metainfo.GetPersistentValue(ctx, kitexx.TraceIDKey)
			zlog.Ctx(ctx).Info("in handler", zlog.Str("id", c.Param("id")))
			c.String(200, "ok")
		})
		h.GET("/missing", func(ctx context.Context, c *app.RequestContext) { c.String(404, "no") })
		h.GET("/broken", func(ctx context.Context, c *app.RequestContext) { c.String(502, "bad") })
	})

	w := ut.PerformRequest(h.Engine, "GET", "/user/7?x=1", nil)
	id := w.Header().Get(TraceHeader)
	if len(id) != 32 || inHandler != id || passedOn != id {
		t.Fatalf("trace_id: response %q, handler %q, passed on %q", id, inHandler, passedOn)
	}
	recs := logs.Records()
	if len(recs) != 2 {
		t.Fatalf("want the handler's record and the request's: %s", logs)
	}
	for _, r := range recs {
		if r.Fields["trace_id"] != id || r.Fields["method"] != "GET" || r.Fields["path"] != "/user/7" {
			t.Errorf("record without the request: %+v", r)
		}
	}
	if req := recs[1]; req.Msg != "http" || req.Level != "INFO" || req.Fields["status"] != float64(200) || req.Fields["latency"] == nil {
		t.Errorf("request record: %+v", req)
	}

	// The caller's id is kept when it looks like one, and only then.
	w = ut.PerformRequest(h.Engine, "GET", "/user/7", nil, ut.Header{Key: TraceHeader, Value: "abc-DEF_12345678"})
	if got := w.Header().Get(TraceHeader); got != "abc-DEF_12345678" || inHandler != got {
		t.Errorf("the caller's trace id was not kept: %q / %q", got, inHandler)
	}
	for _, bad := range []string{"short", "has space 12345678", "quote\"12345678", strings.Repeat("a", 65)} {
		w = ut.PerformRequest(h.Engine, "GET", "/user/7", nil, ut.Header{Key: TraceHeader, Value: bad})
		if got := w.Header().Get(TraceHeader); got == bad || len(got) != 32 {
			t.Errorf("%q must be replaced by a new id, got %q", bad, got)
		}
	}

	ut.PerformRequest(h.Engine, "GET", "/missing", nil)
	ut.PerformRequest(h.Engine, "GET", "/broken", nil)
	if !logs.Has("WARN", "http") || !logs.Has("ERROR", "http") {
		t.Errorf("4xx is a warning and 5xx an error: %s", logs)
	}
}

func TestRecovery(t *testing.T) {
	logs := zlogtest.Capture(t)
	h := engine(func(h *server.Hertz) {
		h.GET("/boom", func(ctx context.Context, c *app.RequestContext) { panic("nil map") })
	})
	w := ut.PerformRequest(h.Engine, "GET", "/boom", nil)
	if w.Code != 500 {
		t.Fatalf("status %d", w.Code)
	}
	id := w.Header().Get(TraceHeader)
	var sawPanic, sawRequest bool
	for _, r := range logs.Records() {
		switch r.Msg {
		case "panic in handler":
			sawPanic = r.Level == "ERROR" && r.Fields["trace_id"] == id && r.Fields["panic"] == "nil map" &&
				strings.Contains(fmt.Sprint(r.Fields["panic_stack"]), "hertzx_test.go")
		case "http":
			sawRequest = r.Level == "ERROR" && r.Fields["status"] == float64(500)
		}
	}
	if !sawPanic || !sawRequest {
		t.Errorf("want the panic and the request as error records of the same trace: %s", logs)
	}
}

// Hertz's own records go through zlog: one format, the caller is the line of
// Hertz, and the "HERTZ: " prefix is the logger field instead.
func TestHertzLogsThroughZlog(t *testing.T) {
	var buf bytes.Buffer
	bridgeHlog(zlog.New(zlog.Options{Format: "json", Level: "debug", Output: &buf}))
	t.Cleanup(func() { bridgeHlog(zlog.Default()) })
	hlog.SystemLogger().Infof("HTTP server listening on address=%s", "[::]:8080")
	hlog.Warnf("from the %s", "application")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("records: %q", buf.String())
	}
	if !strings.Contains(lines[0], `"msg":"HTTP server listening on address=[::]:8080"`) || !strings.Contains(lines[0], `"logger":"hertz"`) || !strings.Contains(lines[0], `"level":"INFO"`) {
		t.Errorf("system record: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"level":"WARN"`) || !strings.Contains(lines[1], `"caller":"hertzx/hertzx_test.go:`) {
		t.Errorf("the caller must be the line that logged: %s", lines[1])
	}
}

// The stop: requests in progress finish, then the cleanup runs, then Run
// returns. Nacos is left out here; the order with Nacos is tested against a
// real one.
func TestRunStopsGracefully(t *testing.T) {
	logs := zlogtest.Capture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	var cfg Config
	cfg.Service.Name, cfg.Service.Addr, cfg.RegistryDisabled = "gateway", fmt.Sprintf("127.0.0.1:%d", port), true
	cfg.Log.Output = io.Discard
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	zlogtest.Capture(t) // New installed the service's logger; the test wants the records
	logs = zlogtest.Capture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h.GET("/slow", func(ctx context.Context, c *app.RequestContext) {
		close(entered)
		<-release
		c.String(200, "done")
	})
	var order []string
	OnShutdown("db", func() error { order = append(order, "cleanup"); return nil })

	stop, done := make(chan os.Signal, 1), make(chan error, 1)
	go func() { done <- run(h, cfg, stop) }()
	reply := make(chan string, 1)
	go func() {
		for i := 0; i < 100; i++ {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/slow", port))
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				reply <- string(b)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		reply <- "never connected"
	}()
	<-entered
	stop <- syscall.SIGTERM
	select {
	case <-done:
		t.Fatal("Run returned while a request was in progress")
	case <-time.After(300 * time.Millisecond):
	}
	order = append(order, "request finishes")
	close(release)
	if got := <-reply; got != "done" {
		t.Errorf("the request in progress got %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ", ") != "request finishes, cleanup" {
		t.Errorf("order: %v", order)
	}
	if !logs.Has("INFO", "server stopped") {
		t.Errorf("records: %s", logs)
	}
}

// A port that is taken is an error from Run, not a process that hangs.
func TestRunReportsAPortThatIsTaken(t *testing.T) {
	zlogtest.Discard(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var cfg Config
	cfg.Service.Name, cfg.Service.Addr, cfg.RegistryDisabled = "gateway", l.Addr().String(), true
	cfg.Log.Output = io.Discard
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	zlogtest.Discard(t)
	done := make(chan error, 1)
	go func() { done <- run(h, cfg, make(chan os.Signal)) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run hangs on a port that is taken")
	}
}
