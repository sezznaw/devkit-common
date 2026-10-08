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
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

const spec = `openapi: 3.0.3
paths:
  /v1/auth/login:
    post:
      security: []
  /v1/member/getProfile:
    post:
      security:
        - bearerAuth: []
`

func TestRequireLogin(t *testing.T) {
	zlogtest.Capture(t)
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()
	cfg := authx.Config{Enabled: true, Secret: "0123456789abcdef0123456789abcdef", Realm: authx.RealmMember, AccessTTL: config.Duration(time.Hour)}
	iss, _ := authx.NewIssuer(cfg, rdb)
	ver, _ := authx.NewVerifier(cfg, rdb)
	h := server.New(server.WithDisablePrintRoute(true))
	RequireLogin(h, ver, []byte(spec))
	var seen int64
	h.POST("/v1/auth/login", func(ctx context.Context, c *app.RequestContext) { c.String(200, "public") })
	h.POST("/v1/member/getProfile", func(ctx context.Context, c *app.RequestContext) {
		seen = kitexx.MustUID(ctx)
		c.String(200, "ok")
	})
	h.GET("/ping", func(ctx context.Context, c *app.RequestContext) { c.String(200, "pong") })

	if w := ut.PerformRequest(h.Engine, "POST", "/v1/auth/login", nil); w.Code != 200 {
		t.Errorf("public: %d", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/ping", nil); w.Code != 200 {
		t.Errorf("framework path: %d", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", nil); w.Code != 401 || !strings.Contains(w.Body.String(), `"code":1004`) {
		t.Errorf("no token: %d %s", w.Code, w.Body.String())
	}
	tk, _ := iss.Login(context.Background(), authx.RealmMember, 42)
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", nil, ut.Header{Key: "Authorization", Value: "Bearer " + tk.AccessToken}); w.Code != 200 || seen != 42 {
		t.Errorf("with token: %d uid=%d", w.Code, seen)
	}
	iss.RevokeAll(context.Background(), authx.RealmMember, 42)
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", nil, ut.Header{Key: "Authorization", Value: "Bearer " + tk.AccessToken}); w.Code != 401 || !strings.Contains(w.Body.String(), `"code":1005`) {
		t.Errorf("revoked: %d %s", w.Code, w.Body.String())
	}
}
