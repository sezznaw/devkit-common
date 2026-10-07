package webhookx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func runtimeWith(t *testing.T, cb httpx.Callback) *kitexx.Runtime {
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })
	rt := &kitexx.Runtime{Redis: rdb}
	rt.Config.Providers = map[string]httpx.Provider{"pay": {BaseURL: "https://pay.example", Callback: cb}}
	return rt
}

func sign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestHMACVerifiedDedupedAndRetriedOnError(t *testing.T) {
	logs := zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{
		Verify:  httpx.CallbackVerify{Type: "hmac-sha256", Secret: "s3", Payload: "{timestamp}.{body}"},
		EventID: httpx.EventIDSource{JSON: "data.id"},
	})
	h := server.New(server.WithDisablePrintRoute(true))
	var handled []string
	var fail bool
	Handle(rt, h, "pay", "/callbacks/pay/paid", func(ctx context.Context, ev *Event) error {
		if fail {
			return errors.New("db down")
		}
		var d struct{ Data struct{ ID, Status string } }
		if err := ev.Decode(&d); err != nil {
			return err
		}
		handled = append(handled, ev.ID+":"+d.Data.Status)
		return nil
	}, WithReply(200, "application/json", `{"code":"SUCCESS"}`))

	body := `{"data":{"id":"evt-1","status":"PAID"}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	post := func(sig, tstamp string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, "POST", "/callbacks/pay/paid", &ut.Body{Body: bytesReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"}, ut.Header{Key: "X-Signature", Value: sig}, ut.Header{Key: "X-Timestamp", Value: tstamp})
	}
	// bad signature
	if w := post("deadbeef", ts); w.Code != 401 {
		t.Fatalf("bad signature: %d", w.Code)
	}
	// stale timestamp
	old := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	if w := post(sign("s3", old, body), old); w.Code != 401 {
		t.Fatalf("stale: %d", w.Code)
	}
	// good, "sha256=" prefix tolerated
	w := post("sha256="+sign("s3", ts, body), ts)
	if w.Code != 200 || w.Body.String() != `{"code":"SUCCESS"}` || len(handled) != 1 || handled[0] != "evt-1:PAID" {
		t.Fatalf("good: %d %s handled=%v", w.Code, w.Body.String(), handled)
	}
	// redelivered: acknowledged, handler not run
	if w := post(sign("s3", ts, body), ts); w.Code != 200 || len(handled) != 1 {
		t.Fatalf("duplicate: %d handled=%v", w.Code, handled)
	}
	if !logs.Has("INFO", "callback already handled; acknowledged again") {
		t.Error("duplicate is logged")
	}
	// handler error: 500 and the id is forgotten, so the retry runs again
	fail = true
	body2 := `{"data":{"id":"evt-2","status":"PAID"}}`
	post2 := func() *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, "POST", "/callbacks/pay/paid", &ut.Body{Body: bytesReader(body2), Len: len(body2)},
			ut.Header{Key: "X-Signature", Value: sign("s3", ts, body2)}, ut.Header{Key: "X-Timestamp", Value: ts})
	}
	if w := post2(); w.Code != 500 {
		t.Fatalf("handler error: %d", w.Code)
	}
	fail = false
	if w := post2(); w.Code != 200 || len(handled) != 2 {
		t.Fatalf("retry after error: %d handled=%v", w.Code, handled)
	}
	for res, want := range map[string]float64{"ok": 2, "duplicate": 1, "bad_signature": 2, "handler_error": 1} {
		if got := testutil.ToFloat64(metricsx.CallbacksReceived.WithLabelValues("pay", "/callbacks/pay/paid", res)); got != want {
			t.Errorf("metric %s = %v, want %v", res, got, want)
		}
	}
}

func TestSourceCIDRBasicAndCustom(t *testing.T) {
	zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{
		Verify:     httpx.CallbackVerify{Type: "basic", Username: "pay", Password: "pw"},
		AllowCIDRs: []string{"203.0.113.0/24"},
		EventID:    httpx.EventIDSource{Header: "X-Event-Id"},
	})
	h := server.New(server.WithDisablePrintRoute(true))
	n := 0
	Handle(rt, h, "pay", "/cb", func(ctx context.Context, ev *Event) error { n++; return nil })
	req := func(ip, auth string) int {
		return ut.PerformRequest(h.Engine, "POST", "/cb", &ut.Body{Body: bytesReader("{}"), Len: 2},
			ut.Header{Key: "X-Forwarded-For", Value: ip}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "X-Event-Id", Value: "e" + strconv.Itoa(n)}).Code
	}
	if c := req("198.51.100.9", "Basic cGF5OnB3"); c != 403 {
		t.Errorf("wrong source: %d", c)
	}
	if c := req("203.0.113.7", "Basic cGF5Om5vcGU="); c != 401 {
		t.Errorf("wrong password: %d", c)
	}
	if c := req("203.0.113.7", "Basic cGF5OnB3"); c != 200 || n != 1 {
		t.Errorf("good: %d n=%d", c, n)
	}

	rt2 := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	h2 := server.New(server.WithDisablePrintRoute(true))
	Handle(rt2, h2, "pay", "/cb", func(ctx context.Context, ev *Event) error { return nil },
		WithVerifier(func(ev *Event) error {
			if ev.Headers.Get("X-Token") != "t" {
				return errors.New("bad token")
			}
			return nil
		}))
	if c := ut.PerformRequest(h2.Engine, "POST", "/cb", &ut.Body{Body: bytesReader("{}"), Len: 2}).Code; c != 401 {
		t.Errorf("custom reject: %d", c)
	}
	if c := ut.PerformRequest(h2.Engine, "POST", "/cb", &ut.Body{Body: bytesReader("{}"), Len: 2}, ut.Header{Key: "X-Token", Value: "t"}).Code; c != 200 {
		t.Errorf("custom accept: %d", c)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("custom without WithVerifier panics at registration")
		}
	}()
	Handle(rt2, server.New(), "pay", "/cb2", func(context.Context, *Event) error { return nil })
}

func TestCallbackValidate(t *testing.T) {
	bad := []httpx.Callback{
		{EventID: httpx.EventIDSource{Header: "X"}},                                         // no verify, no cidrs
		{Verify: httpx.CallbackVerify{Type: "hmac-sha256"}},                                 // no secret
		{Verify: httpx.CallbackVerify{Type: "basic", Username: "u"}},                        // no password
		{Verify: httpx.CallbackVerify{Type: "jwt"}},                                         // unknown
		{Verify: httpx.CallbackVerify{Type: "custom"}, AllowCIDRs: []string{"203.0.113.7"}}, // not a CIDR
		{Verify: httpx.CallbackVerify{Type: "custom"}, EventID: httpx.EventIDSource{Header: "a", JSON: "b"}},
	}
	for _, b := range bad {
		if err := b.Validate("pay"); err == nil {
			t.Errorf("%+v accepted", b)
		}
	}
	if err := (httpx.Callback{AllowCIDRs: []string{"203.0.113.0/24"}}).Validate("pay"); err != nil {
		t.Error(err)
	}
}
