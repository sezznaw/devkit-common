package webhookx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/metricsx"
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

// TestVerifierUnavailable: a custom verifier that cannot judge the request
// (the data it judges by is not readable right now) returns ErrUnavailable:
// the provider gets a 503, which it retries, not the 401 of "not from the
// provider"; the request is counted as verify_unavailable, not archived (it
// is not verified) and does not reach the handler. Any other error of the
// verifier stays a 401.
func TestVerifierUnavailable(t *testing.T) {
	logs := zlogtest.Capture(t)
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
	const path = "/cb-unavailable"
	var verdict error
	handled := 0
	Handle(rt, h, "pay", path, func(context.Context, *Event) error { handled++; return nil }, WithVerifier(func(*Event) error { return verdict }))
	count := func(res string) float64 {
		return testutil.ToFloat64(metricsx.CallbacksReceived.WithLabelValues("pay", path, res))
	}
	post := func(id string) int {
		body := `{"id":"` + id + `"}`
		return ut.PerformRequest(h.Engine, "POST", path, &ut.Body{Body: bytesReader(body), Len: len(body)}).Code
	}
	unavailable, refused := count("verify_unavailable"), count("bad_signature")
	for i, v := range []error{ErrUnavailable, fmt.Errorf("provider records: %w", fmt.Errorf("%w: database is down", ErrUnavailable))} {
		verdict = v
		if code := post("evt-u" + strconv.Itoa(i)); code != 503 {
			t.Fatalf("verifier returned %v: status %d, want 503", v, code)
		}
	}
	if got := count("verify_unavailable") - unavailable; got != 2 || count("bad_signature") != refused || handled != 0 {
		t.Fatalf("verify_unavailable +%v (want 2), bad_signature +%v (want 0), handler ran %d times (want 0)", got, count("bad_signature")-refused, handled)
	}
	if !logs.Has("ERROR", "callback not verified: verifier unavailable; answering 503 so the provider retries") {
		t.Errorf("not logged at error:\n%s", logs)
	}
	// any other error is still "not from the provider"
	verdict = errors.New("signature mismatch")
	if code := post("evt-bad"); code != 401 || count("bad_signature") != refused+1 || handled != 0 {
		t.Fatalf("another verifier error: status %d, bad_signature +%v, handler ran %d times; want 401, 1, 0", code, count("bad_signature")-refused, handled)
	}
	// the provider's retry, once the verifier can judge again, is handled and archived: the only archived record
	verdict = nil
	if code := post("evt-u0"); code != 200 || handled != 1 {
		t.Fatalf("retry: status %d, handler ran %d times", code, handled)
	}
	reader, err := kgo.NewClient(kgo.SeedBrokers(cl.ListenAddrs()...), kgo.ConsumeTopics("callbacks.pay"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var keys []string
	for len(keys) < 1 {
		fetches := reader.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatal("nothing archived")
		}
		for _, r := range fetches.Records() {
			keys = append(keys, string(r.Key))
		}
	}
	pollCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	for _, r := range reader.PollFetches(pollCtx).Records() {
		keys = append(keys, string(r.Key))
	}
	if len(keys) != 1 || keys[0] != "evt-u0" {
		t.Fatalf("archived %v, want only the verified retry evt-u0: a request that was not verified must not be archived", keys)
	}
}
