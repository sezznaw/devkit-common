package hertzx

import (
	"fmt"
	"context"
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

// scalarJS is the Scalar API Reference bundle (docs/README.md says which
// version), embedded so the page works inside the office network.
//
//go:embed docs/scalar.js
var scalarJS []byte

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
	specJSON, err := yamlToJSON(spec)
	if err != nil {
		zlog.Warn("openapi.yaml does not parse; /openapi.json not served", zlog.Err(err))
	}
	title := specTitle(spec)
	if title == "" {
		title = cfg.Service.Name
	}
	page := []byte(strings.NewReplacer("{{title}}", title+" · API 文档").Replace(docsPage))
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
// entered. A hook for a login flow (store the token, attach it, refresh it)
// is where the project's auth lands once there is one.
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
      (function () {
        var KEY = 'sportsbook_docs_access_token';
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
          if (tok && sameOrigin && !req.headers.get('Authorization')) {
            var h = new Headers(req.headers); h.set('Authorization', 'Bearer ' + tok);
            req = new Request(req, { headers: h });
          }
          return orig(req).then(function (resp) { if (sameOrigin) remember(resp); return resp; });
        };
      })();
    </script>
    <script id="api-reference" data-url="/openapi.yaml"
      data-configuration='{"orderSchemaPropertiesBy":"preserve","orderRequiredPropertiesFirst":false,"persistAuth":true,"hideClientButton":true,"defaultOpenAllTags":true,"authentication":{"preferredSecurityScheme":"bearerAuth"}}'></script>
    <script src="/docs/scalar.js"></script>
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
