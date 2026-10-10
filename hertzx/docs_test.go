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
	spec := []byte("openapi: 3.0.3\ninfo: {title: 玩家网关, version: v}\npaths: {}\n")
	h := server.New(server.WithDisablePrintRoute(true))
	cfg := kitexx.Config{}
	cfg.Service.Name = "ser-api"
	cfg.Docs.Enabled = true
	ServeDocs(h, cfg, spec)
	if w := ut.PerformRequest(h.Engine, "GET", "/openapi.yaml", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "openapi: 3.0.3") {
		t.Errorf("openapi.yaml: %d %s", w.Code, w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/openapi.json", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"openapi":"3.0.3"`) {
		t.Errorf("openapi.json: %d %s", w.Code, w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/docs", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `id="api-reference"`) || !strings.Contains(w.Body.String(), "玩家网关 · API 文档") {
		t.Errorf("/docs page: %d", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/docs/scalar.js", nil); w.Code != 200 || w.Body.Len() < 1_000_000 {
		t.Errorf("/docs/scalar.js: %d %d bytes", w.Code, w.Body.Len())
	}
	off := server.New(server.WithDisablePrintRoute(true))
	ServeDocs(off, kitexx.Config{}, spec)
	if w := ut.PerformRequest(off.Engine, "GET", "/openapi.yaml", nil); w.Code != 404 {
		t.Errorf("disabled still serves: %d", w.Code)
	}
}

func TestYAMLToJSONIntKeys(t *testing.T) {
	out, err := yamlToJSON([]byte("paths:\n  /x:\n    post:\n      responses: {200: {description: ok}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"200":{"description":"ok"}`) {
		t.Fatalf("got %s", out)
	}
}
