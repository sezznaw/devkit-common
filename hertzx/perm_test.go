package hertzx

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

const permSpec = `
paths:
  /v1/admin/list:
    post:
      summary: 管理员列表
      tags: [管理员]
      x-permission: admin.view
  /v1/admin/create:
    post:
      summary: 新建管理员
      tags: [管理员]
      x-permission: admin.manage
  /v1/auth/me:
    post:
      summary: 当前登录
`

func TestPermissionPointsAndAllowed(t *testing.T) {
	pts := PermissionPoints([]byte(permSpec))
	if len(pts) != 2 || pts[0].Name != "admin.manage" || pts[1].Name != "admin.view" || pts[1].Path != "/v1/admin/list" || pts[1].Method != "POST" {
		t.Fatalf("%+v", pts)
	}
	if !Allowed([]string{"admin.view"}, "admin.view") || Allowed([]string{"admin.view"}, "admin.manage") {
		t.Fatal("exact")
	}
	if !Allowed([]string{"*"}, "x.y") || !Allowed([]string{"admin.*"}, "admin.manage") || Allowed([]string{"admin.*"}, "role.view") {
		t.Fatal("wildcards")
	}
}

func TestRequirePermission(t *testing.T) {
	zlogtest.Capture(t)
	loads := 0
	var fail bool
	load := func(ctx context.Context, id authx.Identity) ([]string, error) {
		loads++
		if fail {
			return nil, errors.New("down")
		}
		if id.UID == 1 {
			return []string{"admin.view"}, nil
		}
		return nil, nil
	}
	var uid int64
	h := server.New(server.WithDisablePrintRoute(true))
	// a stand-in for RequireLogin: puts the identity of the test in ctx
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		if uid != 0 {
			ctx = kitexx.WithIdentity(ctx, authx.Identity{Realm: authx.RealmAdmin, UID: uid})
		}
		c.Next(ctx)
	})
	if _, err := RequirePermission(h, load); err == nil {
		t.Fatal("no spec must be an error")
	}
	SetSpec(h, []byte(permSpec))
	cache, err := RequirePermission(h, load)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(ctx context.Context, c *app.RequestContext) { c.String(200, "ok") }
	h.POST("/v1/admin/list", ok)
	h.POST("/v1/admin/create", ok)
	h.POST("/v1/auth/me", ok)

	uid = 0
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/auth/me", nil); w.Code != 200 {
		t.Fatalf("no point, no login: %d", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/admin/list", nil); w.Code != 401 {
		t.Fatalf("point without login: %d", w.Code)
	}
	uid = 1
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/admin/list", nil); w.Code != 200 {
		t.Fatalf("allowed: %d %s", w.Code, w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/admin/create", nil); w.Code != 403 || !contains(w.Body.String(), `"code":1007`) {
		t.Fatalf("forbidden: %d %s", w.Code, w.Body.String())
	}
	if loads != 1 {
		t.Fatalf("cache: %d loads", loads)
	}
	cache.Invalidate(authx.RealmAdmin, 1)
	fail = true
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/admin/list", nil); w.Code != 503 {
		t.Fatalf("loader down: %d", w.Code)
	}
	if loads != 2 {
		t.Fatalf("after invalidate: %d loads", loads)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
