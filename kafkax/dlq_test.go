package kafkax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// TestDLQTool: a failing handler parks the event; the tool lists it; a replay
// while the handler still fails leaves it pending without parking it again;
// once fixed, a replay runs the handler in this process (nothing is produced
// to the source topic) and marks it done; a record parked by another service
// is listed as such and skipped; a second parked event is dropped.
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
	adm := kadm.NewClient(c.prod)
	endOf := func(topic string) int64 {
		ends, err := adm.ListEndOffsets(ctx, topic)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		ends.Each(func(o kadm.ListedOffset) { n += o.Offset })
		return n
	}
	if err := c.Publish(ctx, "events.order", "A1", "order.placed", map[string]any{"no": "A1"}); err != nil {
		t.Fatal(err)
	}
	// wait until parked
	var out bytes.Buffer
	waitList := func(want string) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			out.Reset()
			if err := c.DLQ(ctx, []string{"list"}, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %q: %s", want, out.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitList("events.order.dlq: 1 pending")
	if !strings.Contains(out.String(), `type=order.placed`) || !strings.Contains(out.String(), `error="bug"`) {
		t.Fatalf("list: %s", out.String())
	}
	// show
	out.Reset()
	if err := c.DLQ(ctx, []string{"show", "events.order", "0"}, &out); err != nil || !strings.Contains(out.String(), "payload:") || !strings.Contains(out.String(), "source-topic: events.order") || !strings.Contains(out.String(), "consumer: order") {
		t.Fatalf("show: %v %s", err, out.String())
	}
	// a replay while the handler still fails: an error, still pending, not parked a second time, nothing produced
	sourceEnd, dlqEnd := endOf("events.order"), endOf("events.order.dlq")
	out.Reset()
	if err := c.DLQ(ctx, []string{"replay", "events.order"}, &out); err == nil || !strings.Contains(err.Error(), "still fail") {
		t.Fatalf("replay of a failing handler: %v %s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 still failing") || !strings.Contains(out.String(), "0 message(s) marked done") {
		t.Fatalf("replay of a failing handler: %s", out.String())
	}
	if endOf("events.order") != sourceEnd || endOf("events.order.dlq") != dlqEnd {
		t.Fatal("a failed replay produced a record")
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "events.order.dlq: 1 pending") {
		t.Fatalf("after the failed replay: %v %s", err, out.String())
	}
	// fix, replay: handled in this process, nothing produced to the source topic, marked done
	fixed.Store(true)
	out.Reset()
	if err := c.DLQ(ctx, []string{"replay", "events.order"}, &out); err != nil || !strings.Contains(out.String(), "1 replayed") || !strings.Contains(out.String(), "1 message(s) marked done") {
		t.Fatalf("replay: %v %s", err, out.String())
	}
	if handled.Load() != 1 {
		t.Fatalf("replayed event handled %d times (want 1, synchronously)", handled.Load())
	}
	if endOf("events.order") != sourceEnd {
		t.Fatal("the replay published to the source topic")
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "no pending dead letters") {
		t.Fatalf("after replay: %v %s", err, out.String())
	}
	// a record another service parked: listed as such, skipped by a replay, committed past
	foreign, _ := json.Marshal(Event{ID: "f-1", Type: "order.placed", TenantID: "t", Time: time.Now().UnixMilli(), Data: json.RawMessage(`{"no":"F1"}`)})
	if err := c.prod.ProduceSync(ctx, &kgo.Record{Topic: "events.order.dlq", Key: []byte("F1"), Value: foreign, Headers: []kgo.RecordHeader{
		{Key: "error", Value: []byte("their bug")}, {Key: "source-topic", Value: []byte("events.order")}, {Key: "consumer", Value: []byte("billing")},
	}}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	waitList("parked by other services")
	if !strings.Contains(out.String(), "0 pending, 1 parked by other services") || !strings.Contains(out.String(), "(parked by billing)") {
		t.Fatalf("list with a foreign record: %s", out.String())
	}
	foreignOffset := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "(parked by billing)") {
			foreignOffset = strings.TrimPrefix(strings.Fields(line)[0], "offset=")
		}
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"replay", "events.order", foreignOffset}, &out); err == nil || !strings.Contains(err.Error(), "parked by billing") {
		t.Fatalf("replaying another service's record (offset %q): %v %s", foreignOffset, err, out.String())
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"replay", "events.order"}, &out); err != nil || !strings.Contains(out.String(), "0 replayed, 1 parked by other services skipped") {
		t.Fatalf("replay with a foreign record: %v %s", err, out.String())
	}
	if handled.Load() != 1 {
		t.Fatalf("the foreign record ran our handler: %d", handled.Load())
	}
	out.Reset()
	if err := c.DLQ(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "no pending dead letters") {
		t.Fatalf("after skipping: %v %s", err, out.String())
	}
	// a second failure, dropped
	fixed.Store(false)
	if err := c.Publish(ctx, "events.order", "A2", "order.placed", map[string]any{"no": "A2"}); err != nil {
		t.Fatal(err)
	}
	waitList("1 pending")
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
	// a topic this service does not subscribe cannot be replayed here
	if err := c.DLQ(ctx, []string{"replay", "events.other"}, &out); err == nil || !strings.Contains(err.Error(), "does not subscribe") {
		t.Fatalf("replay of an unsubscribed topic: %v", err)
	}
}
