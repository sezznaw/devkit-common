package hertzx

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/kitexx"
)

func TestCORS(t *testing.T) {
	h := server.New(server.WithDisablePrintRoute(true))
	h.Use(CORS(kitexx.CORSConfig{Enabled: true, AllowedOrigins: []string{"https://tester.example.com"}}))
	h.POST("/v1/member/getProfile", func(ctx context.Context, c *app.RequestContext) { c.String(200, "ok") })

	// preflight from an allowed origin: answered without reaching the route
	w := ut.PerformRequest(h.Engine, "OPTIONS", "/v1/member/getProfile", nil,
		ut.Header{Key: "Origin", Value: "https://tester.example.com"},
		ut.Header{Key: "Access-Control-Request-Method", Value: "POST"},
		ut.Header{Key: "Access-Control-Request-Headers", Value: "authorization,content-type"})
	if w.Code != 204 {
		t.Fatalf("preflight: %d %s", w.Code, w.Body.String())
	}
	if got := string(w.Header().Peek("Access-Control-Allow-Origin")); got != "https://tester.example.com" {
		t.Fatalf("allow-origin %q", got)
	}
	if got := string(w.Header().Peek("Access-Control-Allow-Headers")); got == "" {
		t.Fatal("no allow-headers")
	}

	// the request itself carries the origin back and exposes x-trace-id
	w = ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", nil,
		ut.Header{Key: "Origin", Value: "https://tester.example.com"})
	if w.Code != 200 || string(w.Header().Peek("Access-Control-Allow-Origin")) != "https://tester.example.com" {
		t.Fatalf("request: %d origin=%q", w.Code, w.Header().Peek("Access-Control-Allow-Origin"))
	}
	if got := string(w.Header().Peek("Access-Control-Expose-Headers")); !strings.EqualFold(got, "x-trace-id") {
		t.Fatalf("expose %q", got)
	}

	// an origin not in the list is refused outright
	w = ut.PerformRequest(h.Engine, "POST", "/v1/member/getProfile", nil,
		ut.Header{Key: "Origin", Value: "https://evil.example.com"})
	if w.Code != 403 || len(w.Header().Peek("Access-Control-Allow-Origin")) != 0 {
		t.Fatalf("other origin: %d origin=%q", w.Code, w.Header().Peek("Access-Control-Allow-Origin"))
	}
}
