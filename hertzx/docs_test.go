package hertzx

import (
	"bytes"
	"encoding/json"
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
	// the left column starts closed (every domain, the first one too) and the breadcrumb bar is hidden
	if w := ut.PerformRequest(h.Engine, "GET", "/docs", nil); !strings.Contains(w.Body.String(), `"defaultOpenAllTags":false,"defaultOpenFirstTag":false,`) || !strings.Contains(w.Body.String(), `"customCss":"nav.context-bar{display:none!important}"`) {
		t.Errorf("/docs page configuration:\n%s", w.Body.String())
	}
	// the configuration is JSON inside a single-quoted attribute
	if w := ut.PerformRequest(h.Engine, "GET", "/docs", nil); true {
		page := w.Body.String()
		i := strings.Index(page, "data-configuration='")
		j := strings.Index(page[i+len("data-configuration='"):], "'")
		var conf map[string]any
		if err := json.Unmarshal([]byte(page[i+len("data-configuration='"):][:j]), &conf); err != nil || conf["defaultOpenAllTags"] != false {
			t.Errorf("data-configuration is not the JSON it should be: %v %v", err, conf)
		}
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/docs/scalar.js", nil); w.Code != 200 || w.Body.Len() < 1_000_000 {
		t.Errorf("/docs/scalar.js: %d %d bytes", w.Code, w.Body.Len())
	}
	// with client.device.types the served document carries the headers and the page the default
	withDev := server.New(server.WithDisablePrintRoute(true))
	cfg.Client.Device.Types = []string{"PC", "H5"}
	ServeDocs(withDev, cfg, []byte("openapi: 3.0.3\ninfo: {title: t, version: v}\npaths:\n  /v1/x:\n    post:\n      summary: x\n"))
	if w := ut.PerformRequest(withDev.Engine, "GET", "/openapi.json", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"X-Device-Type"`) || !strings.Contains(w.Body.String(), `"enum":["PC","H5"]`) {
		t.Errorf("openapi.json without device header: %d %s", w.Code, w.Body.String())
	}
	if w := ut.PerformRequest(withDev.Engine, "GET", "/docs", nil); !strings.Contains(w.Body.String(), "var DEVICE = 'PC';") {
		t.Errorf("/docs page without the default device")
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

// A domain whose methods all carry `// @docs group` is a tag with child tags
// and no operation of its own. Scalar draws such a tag as a fixed heading;
// the served bundle has that one statement cut, so the domain opens and
// closes with its groups nested inside. Fails after a Scalar upgrade that
// renamed or moved the statement: find its successor in the new bundle (the
// navigation builder sets isGroup on a tag whose children are tags), update
// scalarGroupHeading, and check /docs of a gateway with groups in a browser.
func TestScalarNestedTags(t *testing.T) {
	if n := bytes.Count(scalarBundle, []byte(scalarGroupHeading)); n != 1 {
		t.Fatalf("the embedded Scalar bundle has scalarGroupHeading %d times, want 1: a domain with groups is a fixed heading again", n)
	}
	if bytes.Contains(scalarJS, []byte(scalarGroupHeading)) || len(scalarJS) != len(scalarBundle)-len(scalarGroupHeading)+len("0;") {
		t.Fatalf("served bundle not patched: %d bytes from %d", len(scalarJS), len(scalarBundle))
	}
	// the statement was the body of a for loop: what replaces it must be one too
	if !bytes.Contains(scalarJS, []byte("for(let[e,t]of r)0;return e.filter(")) {
		t.Fatal("the cut left the surrounding loop without a body")
	}
	// an unknown bundle is served as it is, and the version follows the content
	other, v := patchScalar([]byte("console.log(1)"))
	if string(other) != "console.log(1)" || v == scalarVersion || len(v) != 12 {
		t.Fatalf("unknown bundle: %q %q", other, v)
	}
	// the page asks for the patched copy by its version, so a cached older one is not reused
	h := server.New(server.WithDisablePrintRoute(true))
	cfg := kitexx.Config{}
	cfg.Docs.Enabled = true
	ServeDocs(h, cfg, []byte("openapi: 3.0.3\ninfo: {title: t, version: v}\npaths: {}\n"))
	if w := ut.PerformRequest(h.Engine, "GET", "/docs", nil); !strings.Contains(w.Body.String(), `<script src="/docs/scalar.js?v=`+scalarVersion+`"></script>`) {
		t.Errorf("/docs does not load the versioned bundle:\n%s", w.Body.String())
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/docs/scalar.js?v="+scalarVersion, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), scalarJS) {
		t.Errorf("/docs/scalar.js: %d, %d bytes", w.Code, w.Body.Len())
	}
}
