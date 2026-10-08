package hertzx

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Business codes of the login state (idl/errors.md).
const (
	CodeUnauthenticated = 1004 // no token, or invalid / expired
	CodeSessionRevoked  = 1005 // logged out, replaced by a newer login, or revoked
)

// RequireLogin makes every endpoint require a valid access token except the
// ones the IDL marks `// @public` (the OpenAPI document, which apidoc
// generates, has no `security` on those) and the framework's own paths
// (/ping, /docs, /openapi.*). A logged-in request carries its identity in
// the context: kitexx.UID(ctx) in this process and in every RPC made with
// ctx. Without a token the answer is HTTP 401 with the envelope
// {code: 1004}; with a revoked session {code: 1005}.
func RequireLogin(h *server.Hertz, v *authx.Verifier, spec []byte, publicPaths ...string) {
	public := publicOperations(spec)
	for _, p := range publicPaths {
		public[p] = true
	}
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		path := string(c.Path())
		if isFrameworkPath(path) || public[string(c.Method())+" "+path] || public[path] {
			c.Next(ctx)
			return
		}
		token := authx.BearerToken(string(c.GetHeader("Authorization")))
		if token == "" {
			reject(c, CodeUnauthenticated, "未登录")
			return
		}
		id, err := v.Verify(ctx, token)
		switch {
		case errors.Is(err, authx.ErrSessionRevoked):
			reject(c, CodeSessionRevoked, "会话已失效，请重新登录")
			return
		case err != nil:
			reject(c, CodeUnauthenticated, "登录已过期，请重新登录")
			return
		}
		ctx = kitexx.WithIdentity(ctx, id)
		ctx = zlog.CtxWith(ctx, zlog.Int("uid", id.UID))
		c.Next(ctx)
	})
}

func reject(c *app.RequestContext, code int, msg string) {
	c.AbortWithStatusJSON(401, map[string]any{"code": code, "msg": msg})
}

func isFrameworkPath(p string) bool {
	return p == "/ping" || p == "/docs" || strings.HasPrefix(p, "/docs/") || strings.HasPrefix(p, "/openapi.")
}

// publicOperations reads the OpenAPI document and returns "METHOD /path"
// for every operation without a security requirement, plus the bare path.
func publicOperations(spec []byte) map[string]bool {
	out := map[string]bool{}
	var doc struct {
		Paths map[string]map[string]struct {
			Security *[]any `yaml:"security"`
		} `yaml:"paths"`
	}
	if yaml.Unmarshal(spec, &doc) != nil {
		return out
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if op.Security != nil && len(*op.Security) == 0 {
				out[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	return out
}
