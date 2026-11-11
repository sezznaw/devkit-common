package hertzx

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

// scalarBundle is the Scalar API Reference bundle (docs/README.md says which
// version), embedded so the page works inside the office network. The file
// is upstream's, byte for byte; what the page loads is scalarJS.
//
//go:embed docs/scalar.js
var scalarBundle []byte

// scalarGroupHeading is the one statement of Scalar's navigation builder
// that turns a tag whose children are all tags (a domain whose methods all
// carry `// @docs group`, apidoc's `parent` tags) into a group heading: a
// fixed title in the left column, its groups listed below it level with the
// other domains. Without the statement such a tag is drawn like any other:
// a row that opens and closes, the groups nested inside — which is what a
// second level is for, and what Scalar itself does as soon as the parent
// has one operation of its own. x-tagGroups headings are built elsewhere
// and are not affected.
//
// The statement is cut from the served copy, not from the file, so the file
// stays the download the README names. After a Scalar upgrade the test
// TestScalarNestedTags says whether it is still there to cut.
const scalarGroupHeading = "(t.children??[]).some(e=>e.type===`tag`)&&!i.get(e)&&(t.isGroup=!0);"

// scalarJS is the bundle the page loads, scalarVersion the query string that
// makes a browser or CDN holding yesterday's copy (the route is cacheable
// for a day) fetch it again when it changes.
var scalarJS, scalarVersion = patchScalar(scalarBundle)

// patchScalar cuts scalarGroupHeading when the bundle has it exactly once
// (anything else is a bundle this code does not know: served untouched, the
// page works, a parent tag is a heading again).
func patchScalar(bundle []byte) (js []byte, version string) {
	js = bundle
	if bytes.Count(bundle, []byte(scalarGroupHeading)) == 1 {
		// "0;" keeps the `for (...)` the statement is the body of.
		js = bytes.Replace(bundle, []byte(scalarGroupHeading), []byte("0;"), 1)
	}
	sum := sha256.Sum256(js)
	return js, hex.EncodeToString(sum[:6])
}

// ServeDocs mounts the API documentation when docs.enabled: /openapi.yaml
// and /openapi.json are the OpenAPI document generated from the IDL by
// `make gen` (cmd/apidoc, embedded into the binary by main.go); /docs is the
// Scalar API Reference reading it, with "Send request" against this very
// service. Nothing is mounted when disabled.
func ServeDocs(h *server.Hertz, cfg kitexx.Config, spec []byte) {
	if !cfg.Docs.Enabled {
		return
	}
	if len(spec) == 0 {
		zlog.Warn("docs.enabled but the OpenAPI document is empty: run make gen")
		return
	}
	device := ""
	if d := cfg.Client.Device; d.Enabled() {
		// the headers hertzx.Client reads, for readers and the front end's generator
		patched, err := withDeviceHeaders(spec, d)
		if err != nil {
			zlog.Warn("openapi.yaml does not parse; device headers not documented", zlog.Err(err))
		} else {
			spec = patched
		}
		device = d.Types[0]
	}
	specJSON, err := yamlToJSON(spec)
	if err != nil {
		zlog.Warn("openapi.yaml does not parse; /openapi.json not served", zlog.Err(err))
	}
	title := specTitle(spec)
	if title == "" {
		title = cfg.Service.Name
	}
	page := []byte(strings.NewReplacer("{{title}}", title+" · API 文档", "{{device}}", device, "{{scalar}}", scalarVersion).Replace(docsPage))
	h.GET("/openapi.yaml", func(_ context.Context, c *app.RequestContext) {
		c.Data(200, "application/yaml; charset=utf-8", spec)
	})
	if specJSON != nil {
		h.GET("/openapi.json", func(_ context.Context, c *app.RequestContext) {
			c.Data(200, "application/json; charset=utf-8", specJSON)
		})
	}
	h.GET("/docs", func(_ context.Context, c *app.RequestContext) {
		c.Data(200, "text/html; charset=utf-8", page)
	})
	h.GET("/docs/scalar.js", func(_ context.Context, c *app.RequestContext) {
		c.Response.Header.Set("Cache-Control", "public, max-age=86400")
		c.Data(200, "application/javascript; charset=utf-8", scalarJS)
	})
	zlog.Info("api docs on", zlog.Str("page", "/docs"), zlog.Str("document", "/openapi.yaml"))
}

// docsPage is the documentation page: Scalar reading /openapi.yaml. The
// configuration keeps the IDL's field order and remembers the auth a reader
// entered. The page's fetch hook attaches the access token a login answered
// and, when the gateway reads X-Device-Type, that header with the first
// configured type unless the reader filled it in.
const docsPage = `<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{{title}}</title>
  </head>
  <body>
    <script>
      // 登录后自动带 token：拦截页面发出的 fetch，把登录 / 刷新接口返回的 data.access_token 记住，
      // 之后每个没有 Authorization 的请求自动加上。token 只存在这个浏览器的 localStorage 里。
      // 网关要求 X-Device-Type 时（DEVICE 非空），没填的请求自动带配置里的第一个值。
      (function () {
        var KEY = 'sportsbook_docs_access_token';
        var DEVICE = '{{device}}';
        var orig = window.fetch.bind(window);
        function remember(resp) {
          try {
            resp.clone().json().then(function (d) {
              if (d && d.data && typeof d.data.access_token === 'string') localStorage.setItem(KEY, d.data.access_token);
              if (d && d.code === 1004 || d && d.code === 1005) localStorage.removeItem(KEY);
            }).catch(function () {});
          } catch (e) {}
        }
        window.fetch = function (input, init) {
          var req = input instanceof Request ? input : new Request(input, init);
          var tok = localStorage.getItem(KEY);
          var sameOrigin = req.url.indexOf(location.origin) === 0;
          if (sameOrigin) {
            var h = new Headers(req.headers), changed = false;
            if (tok && !h.get('Authorization')) { h.set('Authorization', 'Bearer ' + tok); changed = true; }
            if (DEVICE && !h.get('X-Device-Type')) { h.set('X-Device-Type', DEVICE); changed = true; }
            if (changed) req = new Request(req, { headers: h });
          }
          return orig(req).then(function (resp) { if (sameOrigin) remember(resp); return resp; });
        };
      })();
    </script>
    <script id="api-reference" data-url="/openapi.yaml"
      data-configuration='{"orderSchemaPropertiesBy":"preserve","orderRequiredPropertiesFirst":false,"persistAuth":true,"hideClientButton":true,"defaultOpenAllTags":true,"authentication":{"preferredSecurityScheme":"bearerAuth"}}'></script>
    <script src="/docs/scalar.js?v={{scalar}}"></script>
  </body>
</html>
`

func yamlToJSON(spec []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(spec, &v); err != nil {
		return nil, err
	}
	return json.Marshal(normalize(v))
}

// normalize turns yaml's map[string]any / []any tree into one json can
// encode (yaml.v3 already uses string keys; nested maps may not).
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case map[any]any:
		// A key like 200 (a response status) parses as an int.
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = normalize(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
		return t
	}
	return v
}

// specTitle is info.title of the document: the browser tab shows the same
// name as the page.
func specTitle(spec []byte) string {
	var doc struct {
		Info struct {
			Title string `yaml:"title"`
		} `yaml:"info"`
	}
	if yaml.Unmarshal(spec, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Info.Title)
}
