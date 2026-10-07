package webhookx

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestRawCallbackIsArchivedBeforeTheHandler(t *testing.T) {
	zlogtest.Capture(t)
	cl, err := kfake.NewCluster(kfake.SeedTopics(-1, "callbacks.pay"), kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	kc, err := kafkax.Open(ctx, kafkax.Target{Brokers: cl.ListenAddrs()}, kafkax.Config{Enabled: true}, "ser-pay", "tenant_a")
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}, EventID: httpx.EventIDSource{JSON: "id"}})
	rt.Kafka = kc
	h := server.New(server.WithDisablePrintRoute(true))
	Handle(rt, h, "pay", "/cb", func(context.Context, *Event) error { return nil }, WithVerifier(func(*Event) error { return nil }))
	body := `{"id":"evt-9","amount":100}`
	if w := ut.PerformRequest(h.Engine, "POST", "/cb", &ut.Body{Body: bytesReader(body), Len: len(body)}, ut.Header{Key: "X-Forwarded-For", Value: "203.0.113.9"}); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	// Read the archive topic.
	reader, err := kgo.NewClient(kgo.SeedBrokers(cl.ListenAddrs()...), kgo.ConsumeTopics("callbacks.pay"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	fetches := reader.PollFetches(ctx)
	recs := fetches.Records()
	if len(recs) != 1 {
		t.Fatalf("archived records: %d", len(recs))
	}
	var env kafkax.Event
	if err := json.Unmarshal(recs[0].Value, &env); err != nil {
		t.Fatal(err)
	}
	var data struct {
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
		Client string          `json:"client"`
	}
	if err := env.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if env.Type != "callback.received" || string(recs[0].Key) != "evt-9" || data.Path != "/cb" || string(data.Body) != body || data.Client != "203.0.113.9" {
		t.Errorf("archive: type=%s key=%s %+v", env.Type, recs[0].Key, data)
	}
}
