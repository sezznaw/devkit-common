package hertzx

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/hertz-contrib/swagger"
	swaggerFiles "github.com/swaggo/files"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

// ServeDocs mounts the API documentation when docs.enabled: /openapi.yaml
// is the OpenAPI document generated from the IDL by `make gen` (cmd/apidoc,
// embedded into the binary by main.go), /docs is Swagger UI reading it, with
// "Try it out" against this very service. Nothing is mounted when disabled.
func ServeDocs(h *server.Hertz, cfg kitexx.Config, spec []byte) {
	if !cfg.Docs.Enabled {
		return
	}
	if len(spec) == 0 {
		zlog.Warn("docs.enabled but the OpenAPI document is empty: run make gen")
		return
	}
	h.GET("/openapi.yaml", func(_ context.Context, c *app.RequestContext) {
		c.Data(200, "application/yaml; charset=utf-8", spec)
	})
	h.GET("/docs", func(_ context.Context, c *app.RequestContext) { c.Redirect(302, []byte("/docs/index.html")) })
	h.GET("/docs/*any", swagger.WrapHandler(swaggerFiles.Handler,
		swagger.URL("/openapi.yaml"),
		swagger.PersistAuthorization(true),
		swagger.DocExpansion("list"),
		swagger.DefaultModelsExpandDepth(1)))
	zlog.Info("api docs on", zlog.Str("page", "/docs"), zlog.Str("document", "/openapi.yaml"))
}
