package hertzx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
		ctx = kitexx.WithClient(ctx, kitexx.Client{DeviceID: "dev-1", IP: "203.0.113.9"})
		c.Next(ctx)
	})
	Audit(h, []byte(auditSpec), func(ctx context.Context, e AuditEntry) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})
	h.POST("/v1/admin/create", func(ctx context.Context, c *app.RequestContext) {
		c.JSON(200, map[string]any{"code": 10002, "msg": "username is taken", "data": map[string]any{"token": "t", "uid": 9}})
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
	// the answer is kept too, redacted the same way, and the client's device id
	if !strings.Contains(e.Response, `"code":10002`) || !strings.Contains(e.Response, `"uid":9`) || !strings.Contains(e.Response, `"token":"***"`) {
		t.Fatalf("response: %s", e.Response)
	}
	if e.DeviceID != "dev-1" {
		t.Fatalf("device id: %q", e.DeviceID)
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
	if Redact(nil) != "" || Redact([]byte("not json")) != "<non-JSON body, 8 bytes, not recorded>" {
		t.Fatal("edge cases")
	}
	// arrays, at the top and inside, are redacted too
	got = Redact([]byte(`[{"password":"a","n":1},{"items":[{"bank_account":"6222","id_no":"x","phone":"138"}]}]`))
	if strings.Contains(got, `"a"`) || strings.Contains(got, "6222") || strings.Contains(got, `"x"`) || !strings.Contains(got, `"phone":"138"`) || !strings.Contains(got, `"n":1`) {
		t.Fatal(got)
	}
	// a form body is redacted like JSON (it used to be stored as is, password and all)
	got = Redact([]byte("username=sean&password=plain-text&tag=a&tag=b"))
	if strings.Contains(got, "plain-text") || !strings.Contains(got, `"username":"sean"`) || !strings.Contains(got, `"tag":["a","b"]`) {
		t.Fatal(got)
	}
	// big numbers keep their digits
	if got = Redact([]byte(`{"id":9007199254740993}`)); got != `{"id":9007199254740993}` {
		t.Fatal(got)
	}
}

// A body over the limit is cut at a character boundary and marked: MySQL in
// strict mode refuses half a character, and with it the whole batch.
func TestRedactCutsAtCharacterBoundary(t *testing.T) {
	for pad := 0; pad < 3; pad++ {
		body := `{"remark":"` + strings.Repeat("x", pad) + strings.Repeat("备注", 2000) + `"}`
		got := Redact([]byte(body))
		if len(got) > auditBodyLimit || !utf8.ValidString(got) || !strings.HasSuffix(got, AuditTruncated) {
			t.Fatalf("pad %d: len %d valid %v suffix %v", pad, len(got), utf8.ValidString(got), strings.HasSuffix(got, AuditTruncated))
		}
	}
	if got := cutUTF8(strings.Repeat("浏", 200), auditUALimit, ""); len(got) > auditUALimit || !utf8.ValidString(got) {
		t.Fatal(len(got))
	}
	if got := cutUTF8("a\xffb", 10, ""); !utf8.ValidString(got) {
		t.Fatal(got)
	}
}

// One entry the store refuses (a column too long, say) does not take the rest of its batch with it.
func TestAsyncSinkStoresAroundABadEntry(t *testing.T) {
	zlogtest.Capture(t)
	var mu sync.Mutex
	var stored []string
	sink := NewAsyncSink(func(ctx context.Context, batch []AuditEntry) error {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range batch {
			if e.Action == "bad" {
				return errors.New("Data too long")
			}
		}
		for _, e := range batch {
			stored = append(stored, e.Action)
		}
		return nil
	})
	sink.Sink(context.Background(), AuditEntry{Action: "a"})
	sink.Sink(context.Background(), AuditEntry{Action: "bad"})
	sink.Sink(context.Background(), AuditEntry{Action: "b"})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(stored, ",") != "a,b" {
		t.Fatalf("stored %v", stored)
	}
}

func TestAsyncSinkStopsRetryingWhenTheStoreIsDown(t *testing.T) {
	zlogtest.Capture(t)
	var mu sync.Mutex
	calls := 0
	gate := make(chan struct{})
	sink := NewAsyncSink(func(ctx context.Context, batch []AuditEntry) error {
		<-gate // the first call holds the worker while the rest queue up
		mu.Lock()
		defer mu.Unlock()
		calls++
		return errors.New("connection refused")
	})
	sink.Sink(context.Background(), AuditEntry{Action: "first"})
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 99; i++ {
		sink.Sink(context.Background(), AuditEntry{Action: "a"})
	}
	close(gate)
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// the batch of one: twice; the batch of 99: twice whole, then three single entries and the rest dropped
	if calls != 2+2+auditStoreDownAfter {
		t.Fatalf("store called %d times", calls)
	}
}

// Payment and crypto accounts are masked like bank accounts; phone and email stay readable.
func TestRedactPaymentAccounts(t *testing.T) {
	got := Redact([]byte(`{"payer_account":"6222001","payee_account":"6222002","account_no":"6222003","crypto_address":"TXyz","bank_branch":"BJ","phone":"138","email":"a@b.c","account_holder":"Zhang"}`))
	for _, leak := range []string{"6222001", "6222002", "6222003", "TXyz", "BJ"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%s leaks %s", got, leak)
		}
	}
	if !strings.Contains(got, `"phone":"138"`) || !strings.Contains(got, `"email":"a@b.c"`) {
		t.Fatalf("phone and email should stay: %s", got)
	}
}

// A @sensitive operation is recorded without its response.
func TestAuditSensitiveKeepsNoResponse(t *testing.T) {
	zlogtest.Discard(t)
	spec := []byte("openapi: 3.0.3\npaths:\n  /v1/member/reveal:\n    post:\n      summary: 查看完整信息\n      x-permission: member.manage\n      x-sensitive: true\n  /v1/member/update:\n    post:\n      summary: 改资料\n      x-permission: member.manage\n")
	h := server.New(server.WithDisablePrintRoute(true))
	var got []AuditEntry
	Audit(h, spec, func(ctx context.Context, e AuditEntry) { got = append(got, e) })
	reply := func(ctx context.Context, c *app.RequestContext) {
		c.JSON(200, map[string]any{"code": 0, "data": map[string]any{"phone": "13800138000"}})
	}
	h.POST("/v1/member/reveal", reply)
	h.POST("/v1/member/update", reply)
	ut.PerformRequest(h.Engine, "POST", "/v1/member/reveal", &ut.Body{Body: strings.NewReader(`{"uid":7}`), Len: 9})
	ut.PerformRequest(h.Engine, "POST", "/v1/member/update", &ut.Body{Body: strings.NewReader(`{"uid":7}`), Len: 9})
	if len(got) != 2 {
		t.Fatalf("entries %d", len(got))
	}
	if got[0].Response != AuditSensitiveResponse || !strings.Contains(got[0].Request, `"uid":7`) || got[0].Code != 0 {
		t.Fatalf("sensitive entry %+v", got[0])
	}
	if !strings.Contains(got[1].Response, "13800138000") {
		t.Fatalf("ordinary entry keeps its response: %+v", got[1])
	}
}
