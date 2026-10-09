package kafkax

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// TestDedupe: the same event id delivered twice reaches the handler once; a
// failing handler forgets the id so a replay runs; a different id runs.
func TestDedupe(t *testing.T) {
	zlogtest.Discard(t)
	cl := cluster(t, "events.order", "events.order.dlq")
	cfg := Config{Enabled: true, Brokers: cl.ListenAddrs(), MaxRetries: 1}
	c, err := Open(context.Background(), cfg.Static(), cfg, "order", "t")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()
	c.SetDedupe(rdb)

	var handled atomic.Int32
	var fail atomic.Bool
	c.Subscribe("events.order", func(ctx context.Context, ev *Event) error {
		if fail.Load() {
			return errors.New("bug")
		}
		handled.Add(1)
		return nil
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	produce := func(id string) {
		b, _ := json.Marshal(Event{ID: id, Type: "order.placed", TenantID: "t", Time: time.Now().UnixMilli(), Data: json.RawMessage(`{}`)})
		if err := c.prod.ProduceSync(context.Background(), &kgo.Record{Topic: "events.order", Key: []byte("A1"), Value: b}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	produce("evt-1")
	produce("evt-1") // redelivered
	produce("evt-2")
	deadline := time.Now().Add(10 * time.Second)
	for handled.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a third run would show up here
	if handled.Load() != 2 {
		t.Fatalf("handled %d, want 2 (evt-1 once, evt-2 once)", handled.Load())
	}
	if !srv.Exists("dedupe:order:events.order:evt-1") {
		t.Fatal("dedupe key missing")
	}
	// a failure forgets the id: the replay (same id) runs the fixed handler
	fail.Store(true)
	produce("evt-3")
	deadline = time.Now().Add(10 * time.Second)
	for srv.Exists("dedupe:order:events.order:evt-3") || !c.dlqHas(t, "events.order.dlq") {
		if time.Now().After(deadline) {
			t.Fatal("evt-3 not parked / key not forgotten")
		}
		time.Sleep(50 * time.Millisecond)
	}
	fail.Store(false)
	produce("evt-3")
	deadline = time.Now().Add(10 * time.Second)
	for handled.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if handled.Load() != 3 {
		t.Fatalf("replayed evt-3 not handled: %d", handled.Load())
	}
}

// dlqHas reports whether the dlq topic has at least one record.
func (c *Client) dlqHas(t *testing.T, topic string) bool {
	cl, err := kgo.NewClient(append(c.baseOpts(), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	f := cl.PollFetches(ctx)
	return f.NumRecords() > 0
}
