package hertzx

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func clientServer(cfg kitexx.ClientConfig) (*server.Hertz, *kitexx.Client) {
	seen := &kitexx.Client{}
	h := server.New(server.WithDisablePrintRoute(true))
	h.Use(RequestLog(), Client(cfg))
	h.POST("/v1/x", func(ctx context.Context, c *app.RequestContext) {
		*seen = kitexx.ClientOf(ctx)
		c.String(200, "ok")
	})
	h.GET("/ping", func(ctx context.Context, c *app.RequestContext) { c.String(200, "pong") })
	return h, seen
}

func TestClientRequired(t *testing.T) {
	buf := zlogtest.Capture(t)
	cfg := kitexx.ClientConfig{}
	cfg.Device.Required = true
	cfg.Device.Types = []string{"PC", "H5", "Android", "iOS", "Telegram"}
	h, seen := clientServer(cfg)
	dev := func(v string) ut.Header { return ut.Header{Key: DeviceHeader, Value: v} }

	w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":1001`) || !strings.Contains(w.Body.String(), "X-Device-Type: 必填") {
		t.Fatalf("missing header: %d %s", w.Code, w.Body.String())
	}
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, dev("Windows"))
	if !strings.Contains(w.Body.String(), `"code":1001`) || !strings.Contains(w.Body.String(), "不合要求") {
		t.Fatalf("unknown device: %s", w.Body.String())
	}
	// framework paths are never refused
	if w = ut.PerformRequest(h.Engine, "GET", "/ping", nil); w.Code != 200 || w.Body.String() != "pong" {
		t.Fatalf("ping: %d %s", w.Code, w.Body.String())
	}
	// a valid header, any case, arrives as listed, with the id and the IP
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, dev("ios"), ut.Header{Key: DeviceIDHeader, Value: "phone-9"}, ut.Header{Key: "X-Forwarded-For", Value: "203.0.113.9"})
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("valid: %d %s", w.Code, w.Body.String())
	}
	if *seen != (kitexx.Client{Device: "iOS", DeviceID: "phone-9", IP: "203.0.113.9"}) {
		t.Fatalf("client in context: %+v", *seen)
	}
	if out := buf.String(); !strings.Contains(out, "device=iOS") && !strings.Contains(out, `"device":"iOS"`) {
		t.Fatalf("request log without device: %s", out)
	}
	// a device id that is not an id is refused
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, dev("PC"), ut.Header{Key: DeviceIDHeader, Value: "bad id with spaces"})
	if !strings.Contains(w.Body.String(), "X-Device-Id: 不合要求") {
		t.Fatalf("bad device id: %s", w.Body.String())
	}
}

func TestClientOptional(t *testing.T) {
	zlogtest.Capture(t)
	cfg := kitexx.ClientConfig{}
	cfg.Device.Types = []string{"PC", "H5"}
	h, seen := clientServer(cfg)
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, ut.Header{Key: "X-Forwarded-For", Value: "198.51.100.4"}); w.Code != 200 {
		t.Fatalf("no header, not required: %d %s", w.Code, w.Body.String())
	}
	if seen.Device != "" || seen.IP != "198.51.100.4" {
		t.Fatalf("client: %+v", *seen)
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, ut.Header{Key: DeviceHeader, Value: "Windows"}); !strings.Contains(w.Body.String(), `"code":1001`) {
		t.Fatalf("a wrong value is refused even when optional: %s", w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, ut.Header{Key: DeviceHeader, Value: "h5"}); w.Code != 200 || seen.Device != "H5" {
		t.Fatalf("valid optional: %d %+v", w.Code, *seen)
	}
}

func TestClientOff(t *testing.T) {
	zlogtest.Capture(t)
	h, seen := clientServer(kitexx.ClientConfig{})
	// no types: the header is not read, whatever it says; the IP still is
	if w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, ut.Header{Key: DeviceHeader, Value: "Windows"}); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("off: %d %s", w.Code, w.Body.String())
	}
	if seen.Device != "" || seen.IP == "" {
		t.Fatalf("client: %+v", *seen)
	}
}
