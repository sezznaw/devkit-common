package kafkax

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func cluster(t *testing.T, topics ...string) *kfake.Cluster {
	t.Helper()
	c, err := kfake.NewCluster(kfake.SeedTopics(-1, topics...), kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return rec
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConfigAndTarget(t *testing.T) {
	var c Config
	if c.Validate() != nil {
		t.Fatal("disabled is valid")
	}
	c.Enabled = true
	if got := c.Static().Brokers; len(got) != 1 || got[0] != "127.0.0.1:9092" {
		t.Errorf("default broker: %v", got)
	}
	c.Source = "cloud"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "kafka.source") {
		t.Errorf("bad source: %v", err)
	}
	if c.retries() != defaultRetries {
		t.Errorf("retries default %d", c.retries())
	}
}

func TestPublishAndConsumeWithTheTrace(t *testing.T) {
	zlogtest.Discard(t)
	rec := recordSpans(t)
	cl := cluster(t, "events.member", "events.member.dlq")
	c, err := Open(context.Background(), Target{Brokers: cl.ListenAddrs()}, Config{Enabled: true}, "notify", "tenant_a")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var got atomic.Pointer[Event]
	var gotTrace atomic.Value
	c.Subscribe("events.member", func(ctx context.Context, ev *Event) error {
		gotTrace.Store(trace.SpanContextFromContext(ctx).TraceID().String())
		got.Store(ev)
		return nil
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Publish inside a span: the consumer's span must be in the same trace.
	ctx, span := otel.Tracer("test").Start(context.Background(), "request")
	if err := c.Publish(ctx, "events.member", "2", "member.updated", map[string]any{"uid": 2, "username": "robert"}); err != nil {
		t.Fatal(err)
	}
	span.End()

	waitFor(t, "the event", func() bool { return got.Load() != nil })
	ev := got.Load()
	if ev.Type != "member.updated" || ev.TenantID != "tenant_a" || ev.Key != "2" || ev.ID == "" || ev.Time == 0 || ev.Topic != "events.member" {
		t.Errorf("envelope: %+v", ev)
	}
	var data struct {
		UID      int    `json:"uid"`
		Username string `json:"username"`
	}
	if err := ev.Decode(&data); err != nil || data.Username != "robert" {
		t.Errorf("data: %+v %v", data, err)
	}
	if gotTrace.Load() != span.SpanContext().TraceID().String() {
		t.Errorf("consumer trace %v, producer trace %s", gotTrace.Load(), span.SpanContext().TraceID())
	}
	waitFor(t, "spans", func() bool { return len(rec.Ended()) >= 3 })
}

func TestFailingHandlerGoesToTheDLQ(t *testing.T) {
	zlogtest.Discard(t)
	cl := cluster(t, "events.wallet", "events.wallet.dlq")
	c, err := Open(context.Background(), Target{Brokers: cl.ListenAddrs()}, Config{Enabled: true, MaxRetries: 2}, "report", "tenant_a")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var attempts atomic.Int32
	c.Subscribe("events.wallet", func(ctx context.Context, ev *Event) error {
		attempts.Add(1)
		if ev.Type == "wallet.boom" {
			return errors.New("cannot")
		}
		return nil
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), "events.wallet", "7", "wallet.boom", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}

	// Read the DLQ with a plain client.
	reader, err := kgo.NewClient(kgo.SeedBrokers(cl.ListenAddrs()...), kgo.ConsumeTopics("events.wallet.dlq"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fetches := reader.PollFetches(ctx)
	recs := fetches.Records()
	if len(recs) != 1 {
		t.Fatalf("dlq records: %d", len(recs))
	}
	var parked Event
	if err := json.Unmarshal(recs[0].Value, &parked); err != nil || parked.Type != "wallet.boom" {
		t.Errorf("parked: %+v %v", parked, err)
	}
	headers := map[string]string{}
	for _, h := range recs[0].Headers {
		headers[h.Key] = string(h.Value)
	}
	if headers["error"] != "cannot" || headers["source-topic"] != "events.wallet" || headers["consumer"] != "report" {
		t.Errorf("dlq headers: %v", headers)
	}
	if attempts.Load() != 2 {
		t.Errorf("retried %d times, want 2", attempts.Load())
	}
}

func TestCloseStopsConsumers(t *testing.T) {
	zlogtest.Discard(t)
	cl := cluster(t, "events.x")
	c, err := Open(context.Background(), Target{Brokers: cl.ListenAddrs()}, Config{Enabled: true}, "svc", "")
	if err != nil {
		t.Fatal(err)
	}
	c.Subscribe("events.x", func(ctx context.Context, ev *Event) error { return nil })
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hangs")
	}
}
