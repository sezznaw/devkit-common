package testx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"github.com/sezznaw/devkit-common/delayx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/kitexx"
)

func TestRuntimeInProcess(t *testing.T) {
	rt := New(t, "order", WithTopics("events.order"))
	ctx := Ctx(42)
	if uid := kitexx.MustUID(ctx); uid != 42 {
		t.Fatal(uid)
	}
	// Redis
	if err := rt.Redis.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.MiniRedis.Get("k"); v != "v" {
		t.Fatal("miniredis behind rt.Redis")
	}
	// Centrifugo recorder
	if err := rt.Centrifugo.Publish(ctx, rt.Centrifugo.UserChannel("42"), map[string]any{"type": "hello"}); err != nil {
		t.Fatal(err)
	}
	p := rt.WaitPush("user:42", time.Second)
	if string(p.Data) != `{"type":"hello"}` {
		t.Fatalf("%s", p.Data)
	}
	// Kafka publish + consume
	got := make(chan *kafkax.Event, 1)
	rt.Kafka.Subscribe("events.order", func(ctx context.Context, ev *kafkax.Event) error { got <- ev; return nil })
	rt.Start()
	if err := rt.Kafka.Publish(ctx, "events.order", "1", "order.placed", map[string]any{"no": "A1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		if ev.Type != "order.placed" {
			t.Fatal(ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not consumed")
	}
	if evs := rt.Events("events.order", 2*time.Second); len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
}

func TestRuntimeMySQL(t *testing.T) {
	if os.Getenv(DSNEnv) == "" {
		t.Skip(DSNEnv + " not set")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "000001_init.up.sql"), []byte("CREATE TABLE thing (id BIGINT PRIMARY KEY, name VARCHAR(32));\nINSERT INTO thing VALUES (1, 'one');\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "000002_tasks.up.sql"), []byte(delayx.DDL+";\n"+kafkax.OutboxDDL+";\n"), 0o644)
	rt := New(t, "order", WithMySQL(dir), WithTopics("events.order"))
	var n int64
	rt.DB.Table("thing").Count(&n)
	if n != 1 {
		t.Fatalf("migrations applied: %d rows", n)
	}
	// a delayed task scheduled for the past runs with RunDue
	ran := 0
	rt.Delay.Handle("order.cancel", func(ctx context.Context, task *delayx.Task) error { ran++; return nil })
	if _, err := rt.Delay.Schedule(context.Background(), rt.DB, "order.cancel", "A1", time.Now().Add(-time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if rt.RunDue() != 1 || ran != 1 {
		t.Fatalf("RunDue: ran=%d", ran)
	}
	// an outbox event reaches the topic once Start ran the relay
	rt.Start()
	err := rt.DB.Transaction(func(tx *gorm.DB) error {
		return rt.Kafka.PublishTx(context.Background(), tx, "events.order", "A1", "order.cancelled", map[string]any{"no": "A1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev := rt.WaitEvent("events.order", "order.cancelled", 5*time.Second); ev.Type != "order.cancelled" {
		t.Fatal(ev)
	}
}

func TestGatewayHelpers(t *testing.T) {
	h := server.New(server.WithDisablePrintRoute(true))
	h.POST("/v1/echo", func(ctx context.Context, c *app.RequestContext) {
		var req struct {
			Name string `json:"name"`
		}
		_ = c.BindJSON(&req)
		if req.Name == "" {
			c.JSON(200, map[string]any{"code": 1001, "msg": "name: 不合要求"})
			return
		}
		c.JSON(200, map[string]any{"code": 0, "data": map[string]any{"name": req.Name, "auth": string(c.GetHeader("Authorization"))}})
	})
	var out struct {
		Name string `json:"name"`
		Auth string `json:"auth"`
	}
	Post(t, h, "/v1/echo", map[string]any{"name": "alice"}, Bearer("tok")).MustCode(t, 200, 0).Decode(t, &out)
	if out.Name != "alice" || out.Auth != "Bearer tok" {
		t.Fatalf("%+v", out)
	}
	if r := Post(t, h, "/v1/echo", map[string]any{}); r.Code != 1001 || r.Msg == "" {
		t.Fatal(r)
	}
	if r := Get(t, h, "/nope"); r.Status != 404 || r.Code != -1 {
		t.Fatal(r)
	}
}
