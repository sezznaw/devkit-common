package hertzx

import (
	"context"
	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/loadbalance"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/devroute"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

const guardSpec = `openapi: 3.0.3
paths:
  /v1/auth/login:
    post:
      operationId: S_AuthLogin
      x-rate-limit: 3/m
  /v1/member/getProfile:
    post:
      operationId: S_MemberGetProfile
  /v1/agency/addMembers:
    post:
      operationId: S_AgencyAddMembers
      x-max-body: 1k
`

func TestParseRate(t *testing.T) {
	for in, want := range map[string]string{"20/s": "20/s", "600 / m": "600/m", "5000/h": "5000/h", "off": "off", "": "off"} {
		r, err := kitexx.ParseRate(in)
		if err != nil || r.String() != want {
			t.Fatalf("%q -> %v %v", in, r, err)
		}
	}
	for _, bad := range []string{"x", "10", "0/m", "10/d"} {
		if _, err := kitexx.ParseRate(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func newGuarded(t *testing.T, cfg kitexx.GuardConfig, withRedis bool) *server.Hertz {
	return newGuardedWith(t, cfg, withRedis, 0)
}

// newGuardedWithDebug is newGuarded with tagged routing on: the X-Dev-Tag
// middleware installed and the guard given the debug timeout.
func newGuardedWithDebug(t *testing.T, cfg kitexx.GuardConfig, debug time.Duration) *server.Hertz {
	return newGuardedWith(t, cfg, false, debug)
}

func newGuardedWith(t *testing.T, cfg kitexx.GuardConfig, withRedis bool, debug time.Duration) *server.Hertz {
	t.Helper()
	zlogtest.Discard(t)
	h := server.New(server.WithDisablePrintRoute(true))
	SetSpec(h, []byte(guardSpec))
	if debug > 0 {
		on := true
		h.Use(DevTag(devroute.Config{On: &on}))
		h.Use(Guard(h, cfg, nil, "api", debug))
	} else if withRedis {
		srv := miniredis.RunT(t)
		cli, err := redisx.Open(context.Background(), redisx.Target{Addr: srv.Addr()}, redisx.Config{}, "api")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cli.Close() })
		h.Use(Guard(h, cfg, cli, "api"))
	} else {
		h.Use(Guard(h, cfg, nil, "api"))
	}
	ok := func(ctx context.Context, c *app.RequestContext) { c.JSON(200, map[string]any{"code": 0}) }
	h.POST("/v1/auth/login", ok)
	h.POST("/v1/member/getProfile", ok)
	h.POST("/v1/agency/addMembers", ok)
	h.POST("/slow", func(ctx context.Context, c *app.RequestContext) {
		select {
		case <-ctx.Done():
			return // the downstream call gave up with the deadline; nothing written
		case <-time.After(500 * time.Millisecond):
			c.JSON(200, map[string]any{"code": 0})
		}
	})
	h.GET("/ping", ok)
	return h
}

func TestGuardRateLimits(t *testing.T) {
	for _, withRedis := range []bool{true, false} {
		var cfg kitexx.GuardConfig
		r, _ := kitexx.ParseRate("6/m")
		cfg.Rate.PerIP = r
		h := newGuarded(t, cfg, withRedis)
		post := func(path, ip string) *ut.ResponseRecorder {
			return ut.PerformRequest(h.Engine, "POST", path, nil, ut.Header{Key: "X-Forwarded-For", Value: ip})
		}
		// route limit 3/m on login: the 4th from one IP is 429 with 1008
		for i := 0; i < 3; i++ {
			if w := post("/v1/auth/login", "10.0.0.1"); w.Code != 200 {
				t.Fatalf("redis=%v login %d: %d %s", withRedis, i, w.Code, w.Body.String())
			}
		}
		w := post("/v1/auth/login", "10.0.0.1")
		if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":1008`) || w.Header().Get("Retry-After") == "" {
			t.Fatalf("redis=%v 4th login: %d %s retry=%q", withRedis, w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
		// another IP is not affected
		if w := post("/v1/auth/login", "10.0.0.2"); w.Code != 200 {
			t.Fatalf("other ip: %d", w.Code)
		}
		// service-wide 6/m: 10.0.0.1 made 4 (3 ok + 1 refused, all counted) -> 2 more profile reads, then 429
		for i := 0; i < 2; i++ {
			if w := post("/v1/member/getProfile", "10.0.0.1"); w.Code != 200 {
				t.Fatalf("redis=%v profile %d: %d", withRedis, i, w.Code)
			}
		}
		if w := post("/v1/member/getProfile", "10.0.0.1"); w.Code != 429 {
			t.Fatalf("redis=%v service limit: %d %s", withRedis, w.Code, w.Body.String())
		}
		// /ping is never limited
		for i := 0; i < 10; i++ {
			if w := ut.PerformRequest(h.Engine, "GET", "/ping", nil, ut.Header{Key: "X-Forwarded-For", Value: "10.0.0.1"}); w.Code != 200 {
				t.Fatal("ping limited")
			}
		}
	}
}

func TestGuardTimeout(t *testing.T) {
	cfg := kitexx.GuardConfig{Timeout: config.Duration(30 * time.Millisecond)}
	h := newGuarded(t, cfg, false)
	w := ut.PerformRequest(h.Engine, "POST", "/slow", nil)
	if w.Code != 504 || !strings.Contains(w.Body.String(), `"code":1010`) {
		t.Fatalf("timeout: %d %s", w.Code, w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", nil); w.Code != 200 {
		t.Fatalf("fast request: %d", w.Code)
	}
}

// A request tagged with a tag that has a takeover instance gets the debug
// timeout: the same slow handler that is 504 for everyone else completes for
// the developer on a breakpoint. A tag nobody is debugging with relaxes
// nothing, so a header alone cannot hold a gateway slot. [takeover]
func TestGuardTimeoutRelaxedForTaggedRequest(t *testing.T) {
	cfg := kitexx.GuardConfig{Timeout: config.Duration(30 * time.Millisecond)}
	h := newGuardedWithDebug(t, cfg, time.Second)
	w := ut.PerformRequest(h.Engine, "POST", "/slow", nil)
	if w.Code != 504 {
		t.Fatalf("untagged slow request: %d", w.Code)
	}
	w = ut.PerformRequest(h.Engine, "POST", "/slow", nil, ut.Header{Key: devroute.HeaderKey, Value: "alice"})
	if w.Code != 504 {
		t.Fatalf("tagged request without a takeover behind it: %d", w.Code)
	}
	takeoverActive(t, "alice")
	w = ut.PerformRequest(h.Engine, "POST", "/slow", nil, ut.Header{Key: devroute.HeaderKey, Value: "alice"})
	if w.Code != 200 {
		t.Fatalf("tagged slow request: %d %s", w.Code, w.Body.String())
	}
}

// takeoverActive makes the process see a takeover instance with the tag,
// the way a backend client's balancer would when one registers.
func takeoverActive(t *testing.T, tag string) {
	t.Helper()
	lb := devroute.NewBalancer().(loadbalance.Rebalancer)
	key := "guard-test-" + tag
	in := discovery.NewInstance("tcp", "172.31.41.180:24001", 1, map[string]string{devroute.TagMetadata: tag})
	lb.Rebalance(discovery.Change{Result: discovery.Result{Cacheable: true, CacheKey: key, Instances: []discovery.Instance{in}}})
	t.Cleanup(func() { lb.Delete(discovery.Change{Result: discovery.Result{Cacheable: true, CacheKey: key}}) })
}

func TestRouteLimits(t *testing.T) {
	m := RouteLimits([]byte(guardSpec))
	if r, ok := m["POST /v1/auth/login"]; !ok || r.N != 3 || r.Window != time.Minute {
		t.Fatalf("%+v", m)
	}
	if _, ok := m["POST /v1/member/getProfile"]; ok {
		t.Fatal("no limit expected")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int{"16384": 16384, "64k": 64 << 10, "384K": 384 << 10, "1m": 1 << 20} {
		if n, err := kitexx.ParseSize(in); err != nil || n != want {
			t.Fatalf("%q -> %d %v", in, n, err)
		}
	}
	for _, bad := range []string{"", "x", "0", "-1", "512kb", "65m", "1g"} {
		if _, err := kitexx.ParseSize(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestGuardBodyLimitPerRoute(t *testing.T) {
	h := newGuarded(t, kitexx.GuardConfig{MaxBodyBytes: 100}, false)
	post := func(path string, n int) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, "POST", path, &ut.Body{Body: strings.NewReader(strings.Repeat("x", n)), Len: n})
	}
	// a route without @maxbody keeps guard.max_body_bytes; the one with @maxbody 1k takes up to 1024
	if w := post("/v1/member/getProfile", 100); w.Code != 200 {
		t.Fatalf("at the limit: %d", w.Code)
	}
	if w := post("/v1/member/getProfile", 101); w.Code != 413 || !strings.Contains(w.Body.String(), `"code":1001`) {
		t.Fatalf("over the default: %d %s", w.Code, w.Body.String())
	}
	if w := post("/v1/agency/addMembers", 1024); w.Code != 200 {
		t.Fatalf("within @maxbody: %d", w.Code)
	}
	if w := post("/v1/agency/addMembers", 1025); w.Code != 413 {
		t.Fatalf("over @maxbody: %d", w.Code)
	}
}

// The server's limit raised by SetSpec after server.New is the one a running
// server applies (Hertz reads the option when it starts).
func TestSetSpecRaisesTheRunningServersLimit(t *testing.T) {
	zlogtest.Discard(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	h := server.New(server.WithHostPorts(addr), server.WithMaxRequestBodySize(100), server.WithDisablePrintRoute(true))
	SetSpec(h, []byte(guardSpec))
	h.Use(Guard(h, kitexx.GuardConfig{MaxBodyBytes: 100}, nil, "api"))
	ok := func(ctx context.Context, c *app.RequestContext) { c.JSON(200, map[string]any{"code": 0}) }
	h.POST("/v1/agency/addMembers", ok)
	h.POST("/v1/member/getProfile", ok)
	// Spin logs through hlog after Shutdown returns (the Run error, its own
	// Shutdown call), so the cleanup waits for it: the next test may swap the
	// hlog logger (TestHertzLogsThroughZlog), a data race under -race.
	spun := make(chan struct{})
	go func() { h.Spin(); close(spun) }()
	t.Cleanup(func() {
		_ = h.Shutdown(context.Background())
		select {
		case <-spun:
		case <-time.After(3 * time.Second):
		}
	})
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	post := func(path string, n int) int {
		resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(strings.Repeat("x", n)))
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/v1/agency/addMembers", 900); code != 200 {
		t.Fatalf("900 bytes to the @maxbody 1k route: %d", code)
	}
	if code := post("/v1/member/getProfile", 900); code != 413 {
		t.Fatalf("900 bytes to a default route: %d", code)
	}
	if code := post("/v1/agency/addMembers", 2000); code != 413 {
		t.Fatalf("2000 bytes, above every limit: %d", code)
	}
}

// An oversized body is refused after it is counted: a client cannot send them without limit.
func TestGuardOversizedBodiesCountAgainstTheRate(t *testing.T) {
	r, _ := kitexx.ParseRate("3/m")
	cfg := kitexx.GuardConfig{MaxBodyBytes: 100}
	cfg.Rate.PerIP = r
	h := newGuarded(t, cfg, false)
	post := func(n int) int {
		return ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", &ut.Body{Body: strings.NewReader(strings.Repeat("x", n)), Len: n},
			ut.Header{Key: "X-Forwarded-For", Value: "10.0.0.9"}).Code
	}
	for i := 0; i < 3; i++ {
		if c := post(500); c != 413 {
			t.Fatalf("oversized %d: %d", i, c)
		}
	}
	if c := post(10); c != 429 {
		t.Fatalf("after three oversized bodies a small request should be rate limited: %d", c)
	}
}
