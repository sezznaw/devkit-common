package hertzx

import (
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestServeDocs(t *testing.T) {
	zlogtest.Capture(t)
	spec := []byte("openapi: 3.0.3\ninfo: {title: t, version: v}\npaths: {}\n")
	h := server.New(server.WithDisablePrintRoute(true))
	cfg := kitexx.Config{}
	cfg.Docs.Enabled = true
	ServeDocs(h, cfg, spec)
	if w := ut.PerformRequest(h.Engine, "GET", "/openapi.yaml", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "openapi: 3.0.3") {
		t.Errorf("openapi.yaml: %d %s", w.Code, w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/docs", nil); w.Code != 302 {
		t.Errorf("/docs redirect: %d", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/docs/index.html", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "swagger") {
		t.Errorf("/docs/index.html: %d", w.Code)
	}
	off := server.New(server.WithDisablePrintRoute(true))
	ServeDocs(off, kitexx.Config{}, spec)
	if w := ut.PerformRequest(off.Engine, "GET", "/openapi.yaml", nil); w.Code != 404 {
		t.Errorf("disabled still serves: %d", w.Code)
	}
}
