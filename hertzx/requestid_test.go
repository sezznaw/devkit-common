package hertzx

import (
	"bytes"
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/kitexx"
)

func TestRequestIDFromBody(t *testing.T) {
	h := server.Default()
	h.Use(RequestID())
	var seen string
	handler := func(ctx context.Context, c *app.RequestContext) { seen = kitexx.RequestID(ctx) }
	h.POST("/w", handler)
	h.GET("/r", handler)

	for _, tc := range []struct{ method, path, body, want string }{
		{"POST", "/w", `{"request_id":"g1","name":"x"}`, "g1"},
		{"POST", "/w", " \n{\"name\":\"x\",\"request_id\":\"g2\"}", "g2"},
		{"POST", "/w", `{"name":"x"}`, ""},
		{"POST", "/w", `not json`, ""},
		{"GET", "/r", ``, ""},
	} {
		seen = "unset"
		ut.PerformRequest(h.Engine, tc.method, tc.path, &ut.Body{Body: bytes.NewBufferString(tc.body), Len: len(tc.body)})
		if seen != tc.want {
			t.Errorf("%s %s %q: request_id %q, want %q", tc.method, tc.path, tc.body, seen, tc.want)
		}
	}
}
