package hertzx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/config"
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
	t.Helper()
	zlogtest.Discard(t)
	h := server.New(server.WithDisablePrintRoute(true))
	SetSpec(h, []byte(guardSpec))
	if withRedis {
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

func TestRouteLimits(t *testing.T) {
	m := RouteLimits([]byte(guardSpec))
	if r, ok := m["POST /v1/auth/login"]; !ok || r.N != 3 || r.Window != time.Minute {
		t.Fatalf("%+v", m)
	}
	if _, ok := m["POST /v1/member/getProfile"]; ok {
		t.Fatal("no limit expected")
	}
}
