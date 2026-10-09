package kafkax

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// TestDLQTool: a failing handler parks the event; the tool lists it, replays
// it to the source topic where the (now fixed) handler consumes it, and marks
// it done; a second parked event is dropped.
func TestDLQTool(t *testing.T) {
	zlogtest.Discard(t)
	cl := cluster(t, "events.order", "events.order.dlq")
	cfg := Config{Enabled: true, Brokers: cl.ListenAddrs(), MaxRetries: 1}
	c, err := Open(context.Background(), cfg.Static(), cfg, "order", "t")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var fixed atomic.Bool
	var handled atomic.Int32
	c.Subscribe("events.order", func(ctx context.Context, ev *Event) error {
		if !fixed.Load() {
			return errors.New("bug")
		}
		handled.Add(1)
		return nil
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Publish(ctx, "events.order", "A1", "order.placed", map[string]any{"no": "A1"}); err != nil {
		t.Fatal(err)
	}
	// wait until parked
	var out bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for {
		out.Reset()
		if err := c.DLQ(ctx, []string{"list"}, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "events.order.dlq: 1 pending") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not parked: %s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(out.String(), `type=order.placed`) || !strings.Contains(out.String(), `error="bug"`) {
		t.Fatalf("list: %s", out.String())
	}
	// show
	out.Reset()
	if err := c.DLQ(ctx, []string{"show", "events.order", "0"}, &out); err != nil || !strings.Contains(out.String(), "payload:") || !strings.Contains(out.String(), "source-topic: events.order") {
		t.Fatalf("show: %v %s", err, out.String())
	}
	// fix, replay, consumed, marked done
	fixed.Store(true)
	out.Reset()
	if err := c.DLQ(ctx, []string{"replay", "events.order"}, &out); err != nil || !strings.Contains(out.String(), "1 message(s) marked done") {
		t.Fatalf("replay: %v %s", err, out.String())
	}
	deadline = time.Now().Add(10 * time.Second)
	for handled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if handled.Load() != 1 {
		t.Fatal("replayed event not consumed")
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "no pending dead letters") {
		t.Fatalf("after replay: %v %s", err, out.String())
	}
	// a second failure, dropped
	fixed.Store(false)
	if err := c.Publish(ctx, "events.order", "A2", "order.placed", map[string]any{"no": "A2"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		out.Reset()
		_ = c.DLQ(ctx, []string{"list"}, &out)
		if strings.Contains(out.String(), "1 pending") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second not parked: %s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"drop", "events.order"}, &out); err != nil || !strings.Contains(out.String(), "marked done") {
		t.Fatalf("drop: %v %s", err, out.String())
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "no pending dead letters") {
		t.Fatalf("after drop: %v %s", err, out.String())
	}
	if err := c.DLQ(ctx, []string{"bogus"}, &out); err == nil {
		t.Fatal("unknown command accepted")
	}
}
