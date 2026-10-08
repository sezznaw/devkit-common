package hertzx

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

// CodeForbidden is the business code of a request whose login lacks the
// permission point of the operation (HTTP 403).
const CodeForbidden = 1007

// PermissionLoader returns the permission points of a login: what the
// realm's account service says the user may do. The gateway supplies it
// (an RPC to that service); RequirePermission caches the answer.
type PermissionLoader func(ctx context.Context, id authx.Identity) ([]string, error)

// PermissionPoint is one `// @perm <name>` of the IDL: the operation it
// guards and its title, for a role editor to list.
type PermissionPoint struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Tag     string `json:"tag"`
	Method  string `json:"method"`
	Path    string `json:"path"`
}

// PermissionCache remembers the permission points of each login for a
// while, so a page's burst of requests costs one RPC. Invalidate after a
// role change, so it takes effect before the TTL.
type PermissionCache struct {
	ttl     time.Duration
	load    PermissionLoader
	mu      sync.Mutex
	entries map[string]permEntry
}

type permEntry struct {
	points  []string
	expires time.Time
}

// PermissionOption configures RequirePermission.
type PermissionOption func(*PermissionCache)

// WithPermissionCacheTTL sets how long a login's permission points are
// remembered. Default 30s.
func WithPermissionCacheTTL(d time.Duration) PermissionOption {
	return func(c *PermissionCache) { c.ttl = d }
}

// RequirePermission enforces the `// @perm` of the IDL: an operation with
// an x-permission in the OpenAPI document is served only when the login's
// permission points (from load, cached) allow it. Operations without a
// permission point are untouched; install it after RequireLogin, which
// supplies the identity. A login without the point gets HTTP 403 with
// code 1007. A point matches exactly, or through a wildcard the account
// holds: "*" allows everything, "member.*" every point under member.
//
// RequirePermission 落实 IDL 里的 `// @perm`：有权限点的接口只对持有该点的登录放行。
// 装在 RequireLogin 之后。没权限回 HTTP 403、code 1007。角色里的 "*" 是全部，
// "member.*" 是 member 下的全部。
func RequirePermission(h *server.Hertz, load PermissionLoader, opts ...PermissionOption) (*PermissionCache, error) {
	spec := Spec(h)
	if len(spec) == 0 {
		return nil, errors.New("hertzx: RequirePermission needs the OpenAPI document (main.go calls hertzx.SetSpec; run make gen)")
	}
	cache := &PermissionCache{ttl: 30 * time.Second, load: load, entries: map[string]permEntry{}}
	for _, o := range opts {
		o(cache)
	}
	required := map[string]string{}
	for _, p := range PermissionPoints(spec) {
		required[p.Method+" "+p.Path] = p.Name
	}
	if len(required) == 0 {
		zlog.Warn("RequirePermission: the IDL has no `// @perm` line; nothing is guarded")
	}
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		point, ok := required[string(c.Method())+" "+string(c.Path())]
		if !ok {
			c.Next(ctx)
			return
		}
		uid, logged := kitexx.UID(ctx)
		if !logged {
			reject(c, CodeUnauthenticated, "未登录")
			return
		}
		id := authx.Identity{Realm: kitexx.Realm(ctx), UID: uid}
		points, err := cache.Points(ctx, id)
		if err != nil {
			zlog.Ctx(ctx).Error("load permissions", zlog.Err(err))
			c.AbortWithStatusJSON(503, map[string]any{"code": 5001, "msg": "权限服务不可用"})
			return
		}
		if !Allowed(points, point) {
			zlog.Ctx(ctx).Warn("forbidden", zlog.Str("permission", point))
			c.AbortWithStatusJSON(403, map[string]any{"code": CodeForbidden, "msg": "没有权限：" + point})
			return
		}
		c.Next(ctx)
	})
	return cache, nil
}

var specs sync.Map // *server.Hertz -> []byte

// SetSpec gives the server its OpenAPI document (main.go does, from the
// embedded file of make gen); Spec reads it back, for the middlewares and
// handlers that need the IDL's annotations at run time.
func SetSpec(h *server.Hertz, spec []byte) { specs.Store(h, spec) }

// Spec returns the OpenAPI document given by SetSpec, or nil.
func Spec(h *server.Hertz) []byte {
	if v, ok := specs.Load(h); ok {
		return v.([]byte)
	}
	return nil
}

// Points returns the login's permission points, from the cache or the loader.
func (c *PermissionCache) Points(ctx context.Context, id authx.Identity) ([]string, error) {
	key := string(id.Realm) + ":" + itoa(id.UID)
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.points, nil
	}
	points, err := c.load(ctx, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = permEntry{points: points, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return points, nil
}

// Invalidate forgets one login's points (after its roles changed).
func (c *PermissionCache) Invalidate(realm authx.Realm, uid int64) {
	c.mu.Lock()
	delete(c.entries, string(realm)+":"+itoa(uid))
	c.mu.Unlock()
}

// InvalidateAll forgets every login's points (after a role's points changed).
func (c *PermissionCache) InvalidateAll() {
	c.mu.Lock()
	c.entries = map[string]permEntry{}
	c.mu.Unlock()
}

// Allowed reports whether the points held allow the point required:
// exactly, by "*", or by a "<prefix>.*" the required point falls under.
func Allowed(held []string, required string) bool {
	for _, p := range held {
		switch {
		case p == required || p == "*":
			return true
		case strings.HasSuffix(p, ".*") && strings.HasPrefix(required, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

// PermissionPoints lists the operations of the OpenAPI document that carry
// an x-permission, sorted by tag then name: the catalogue a role editor
// shows. One point may guard several operations.
func PermissionPoints(spec []byte) []PermissionPoint {
	var doc struct {
		Paths map[string]map[string]struct {
			Summary    string   `yaml:"summary"`
			Tags       []string `yaml:"tags"`
			Permission string   `yaml:"x-permission"`
		} `yaml:"paths"`
	}
	if yaml.Unmarshal(spec, &doc) != nil {
		return nil
	}
	var out []PermissionPoint
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if op.Permission == "" {
				continue
			}
			tag := ""
			if len(op.Tags) > 0 {
				tag = op.Tags[0]
			}
			out = append(out, PermissionPoint{Name: op.Permission, Summary: op.Summary, Tag: tag, Method: strings.ToUpper(method), Path: path})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tag != out[j].Tag {
			return out[i].Tag < out[j].Tag
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
