package hertzx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

const auditSpec = `
paths:
  /v1/admin/create:
    post:
      summary: 新建管理员
      x-permission: admin.manage
  /v1/admin/list:
    post:
      summary: 管理员列表
      x-permission: admin.view
  /v1/auth/login:
    post:
      summary: 登录
      security: []
      x-audit: true
  /v1/noise/write:
    post:
      summary: 噪音
      x-permission: noise.manage
      x-audit: false
`

func TestAuditedOperations(t *testing.T) {
	ops := auditedOperations([]byte(auditSpec))
	if len(ops) != 2 || ops["POST /v1/admin/create"].title != "新建管理员" || ops["POST /v1/auth/login"].title != "登录" {
		t.Fatalf("%+v", ops)
	}
}

func TestAuditMiddleware(t *testing.T) {
	zlogtest.Capture(t)
	var mu sync.Mutex
	var got []AuditEntry
	h := server.New(server.WithDisablePrintRoute(true))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		ctx = kitexx.WithIdentity(ctx, authx.Identity{Realm: authx.RealmAdmin, UID: 7})
		c.Next(ctx)
	})
	Audit(h, []byte(auditSpec), func(ctx context.Context, e AuditEntry) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})
	h.POST("/v1/admin/create", func(ctx context.Context, c *app.RequestContext) {
		c.JSON(200, map[string]any{"code": 10002, "msg": "username is taken"})
	})
	h.POST("/v1/admin/list", func(ctx context.Context, c *app.RequestContext) { c.JSON(200, map[string]any{"code": 0}) })
	h.POST("/v1/auth/login", func(ctx context.Context, c *app.RequestContext) { c.JSON(200, map[string]any{"code": 0}) })
	post := func(path, body string) {
		ut.PerformRequest(h.Engine, "POST", path, &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"}, ut.Header{Key: "User-Agent", Value: "test"})
	}
	post("/v1/admin/create", `{"request_id":"r","username":"bob","password":"hunter2","role_ids":[1]}`)
	post("/v1/admin/list", `{}`)
	post("/v1/auth/login", `{"username":"admin","password":"x"}`)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("list is not audited: %d entries", len(got))
	}
	e := got[0]
	if e.UID != 7 || e.Realm != "admin" || e.Action != "新建管理员" || e.Permission != "admin.manage" || e.Code != 10002 || e.Msg != "username is taken" || e.Status != 200 || e.UserAgent != "test" {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.Request, `"password":"***"`) || !strings.Contains(e.Request, `"username":"bob"`) {
		t.Fatalf("redaction: %s", e.Request)
	}
	if got[1].Action != "登录" || !strings.Contains(got[1].Request, `"password":"***"`) {
		t.Fatalf("%+v", got[1])
	}
}

func TestAsyncSink(t *testing.T) {
	zlogtest.Capture(t)
	var mu sync.Mutex
	var stored [][]AuditEntry
	calls := 0
	sink := NewAsyncSink(func(ctx context.Context, batch []AuditEntry) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("flaky")
		}
		stored = append(stored, batch)
		return nil
	})
	for i := 0; i < 150; i++ {
		sink.Sink(context.Background(), AuditEntry{Action: "a"})
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, b := range stored {
		total += len(b)
	}
	if total != 150 {
		t.Fatalf("stored %d of 150 (batches %d, calls %d)", total, len(stored), calls)
	}
	// a closed sink does not block
	done := make(chan struct{})
	go func() { sink.Sink(context.Background(), AuditEntry{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Sink after Close blocked")
	}
}

func TestRedact(t *testing.T) {
	got := Redact([]byte(`{"old_password":"a","nested":{"api_key":"k","name":"n"},"amount":"1.00"}`))
	if !strings.Contains(got, `"old_password":"***"`) || !strings.Contains(got, `"api_key":"***"`) || !strings.Contains(got, `"name":"n"`) || !strings.Contains(got, `"amount":"1.00"`) {
		t.Fatal(got)
	}
	if Redact(nil) != "" || Redact([]byte("not json")) != "not json" {
		t.Fatal("edge cases")
	}
}
