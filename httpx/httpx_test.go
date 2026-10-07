package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestValidate(t *testing.T) {
	for _, bad := range []Provider{{}, {BaseURL: "api.vendor.com"}, {BaseURL: "ftp://x"}, {BaseURL: "https://x", Headers: map[string]string{"X-Api-Key": ""}}} {
		if err := bad.Validate("v"); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := (Provider{BaseURL: "https://api.vendor.com/v2", Headers: map[string]string{"X-Api-Key": "k"}}).Validate("v"); err != nil {
		t.Error(err)
	}
}

func TestCallsRetriesAndRecords(t *testing.T) {
	logs := zlogtest.Capture(t)
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{}) // what otelx.Init installs in a service
	defer otel.SetTracerProvider(noop.NewTracerProvider())

	var gets, posts atomic.Int32
	var gotKey, gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotTraceparent = r.Header.Get("X-Api-Key"), r.Header.Get("Traceparent")
		switch r.URL.Path {
		case "/flaky": // 500 twice, then ok
			if gets.Add(1) < 3 {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"odds": 1.5}`))
		case "/pay":
			posts.Add(1)
			w.WriteHeader(502)
		case "/missing":
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such match"}`))
		}
	}))
	defer srv.Close()

	c, err := New("vendor", Provider{BaseURL: srv.URL, Headers: map[string]string{"X-Api-Key": "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Odds float64 }
	if err := c.GetJSON(context.Background(), "/flaky?token=abc", &out); err != nil || out.Odds != 1.5 {
		t.Fatalf("GET: %v %+v", err, out)
	}
	if gets.Load() != 3 {
		t.Errorf("a GET that gets 500 is retried: %d attempts", gets.Load())
	}
	if gotKey != "secret" || !strings.HasPrefix(gotTraceparent, "00-") {
		t.Errorf("headers: key=%q traceparent=%q", gotKey, gotTraceparent)
	}
	if strings.Contains(logs.String(), "token=abc") || strings.Contains(logs.String(), "secret") {
		t.Error("the query string and the headers must not be logged")
	}
	if !logs.Has("INFO", "http call") {
		t.Error("a successful call is one INFO record")
	}
	// A POST that fails is not retried.
	err = c.PostJSON(context.Background(), "/pay", map[string]int{"amount": 100}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 502 || posts.Load() != 1 {
		t.Errorf("POST: err=%v attempts=%d", err, posts.Load())
	}
	if err := c.GetJSON(context.Background(), "/missing", &out); !errors.As(err, &se) || se.Status != 404 || !strings.Contains(se.Body, "no such match") {
		t.Errorf("404: %v", err)
	}
	// Metrics and spans.
	if got := testutil.ToFloat64(metricsx.HTTPClientRequests.WithLabelValues("vendor", "GET", "200")); got != 1 {
		t.Errorf("GET 200 counted %v", got)
	}
	if got := testutil.ToFloat64(metricsx.HTTPClientRequests.WithLabelValues("vendor", "POST", "502")); got != 1 {
		t.Errorf("POST 502 counted %v", got)
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	if !strings.Contains(strings.Join(names, ","), "GET vendor /flaky") {
		t.Errorf("spans: %v", names)
	}
}

func TestConnectionErrorIsReported(t *testing.T) {
	zlogtest.Capture(t)
	c, _ := New("down", Provider{BaseURL: "http://127.0.0.1:1", Retries: ptr(0)})
	err := c.GetJSON(context.Background(), "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "httpx: down GET /x") {
		t.Errorf("%v", err)
	}
	if got := testutil.ToFloat64(metricsx.HTTPClientRequests.WithLabelValues("down", "GET", "error")); got != 1 {
		t.Errorf("error counted %v", got)
	}
}

func ptr(i int) *int { return &i }

func TestBreakerOpensAndRecovers(t *testing.T) {
	zlogtest.Capture(t)
	var hits atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, _ := New("flaky", Provider{BaseURL: srv.URL, Retries: ptr(0), BreakerFailures: ptr(3), BreakerOpenFor: config.Duration(300 * time.Millisecond)})
	for i := 0; i < 3; i++ {
		if err := c.GetJSON(context.Background(), "/x", nil); err == nil {
			t.Fatal("503 is an error")
		}
	}
	err := c.GetJSON(context.Background(), "/x", nil)
	if !errors.Is(err, ErrCircuitOpen) || hits.Load() != 3 {
		t.Fatalf("4th call: err=%v hits=%d (the breaker must not have sent it)", err, hits.Load())
	}
	if got := testutil.ToFloat64(metricsx.HTTPClientBreakerState.WithLabelValues("flaky")); got != 2 {
		t.Errorf("state gauge %v", got)
	}
	failing.Store(false)
	time.Sleep(350 * time.Millisecond)
	if err := c.GetJSON(context.Background(), "/x", nil); err != nil {
		t.Fatalf("half-open probe: %v", err)
	}
	if err := c.GetJSON(context.Background(), "/x", nil); err != nil {
		t.Fatalf("closed again: %v", err)
	}
	if got := testutil.ToFloat64(metricsx.HTTPClientRejected.WithLabelValues("flaky", "circuit_open")); got < 1 {
		t.Errorf("rejected counted %v", got)
	}
}

func TestMaxConcurrentLimitsInFlight(t *testing.T) {
	zlogtest.Capture(t)
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, _ := New("slow", Provider{BaseURL: srv.URL, MaxConcurrent: ptr(2)})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.GetJSON(context.Background(), "/x", nil) }()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Errorf("peak in flight %d, max_concurrent 2", peak.Load())
	}
	// No slot within the deadline: rejected, not queued forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	for i := 0; i < 2; i++ {
		go c.GetJSON(context.Background(), "/x", nil)
	}
	time.Sleep(5 * time.Millisecond)
	if err := c.GetJSON(ctx, "/x", nil); err == nil {
		t.Error("a call that cannot get a slot before its deadline fails")
	}
}
