package hertzx

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Guard is the middleware of the `guard:` section (New installs it): a
// request deadline, and request-rate limits per client IP, both for the
// whole service (guard.rate.per_ip) and per route (`// @limit 10/m` in the
// IDL, written to the OpenAPI document as x-rate-limit). The body size
// limit is a server option New sets (413 before the request is read).
//
// Counting is a fixed window per key in Redis when the service has it
// (shared by the replicas: "rl:<service>:ip:<ip>:<window>"), in memory
// otherwise (per replica; a warning says so at start). Over the limit is
// 429 with code 1008 and Retry-After. The limits run before the login
// check on purpose: a password-guessing client never reaches it.
//
// Guard 是 `guard:` 段的中间件（New 自动装）：每个请求的截止时间，按客户端 IP 的频率上限——
// 整个服务一个（guard.rate.per_ip），每条接口可再加一个（IDL 里 `// @limit 10/m`）。
// 请求体大小是 New 设的服务器选项（读之前就 413）。有 Redis 时各副本共用计数，没有就
// 每副本各算。超限回 429、code 1008、带 Retry-After。限流在登录校验之前：猜密码的请求到不了那一步。
func Guard(h *server.Hertz, cfg kitexx.GuardConfig, rdb *redis.Client, service string) app.HandlerFunc {
	g := &guard{cfg: cfg, rdb: rdb, service: service, h: h, local: map[string]*window{}}
	if rdb == nil {
		zlog.Warn("guard: no redis; request-rate limits count per replica, not across them")
	}
	return g.handle
}

type guard struct {
	cfg     kitexx.GuardConfig
	rdb     *redis.Client
	service string
	h       *server.Hertz

	once   sync.Once
	routes map[string]kitexx.Rate // "METHOD /path" -> @limit

	mu    sync.Mutex
	local map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func (g *guard) handle(ctx context.Context, c *app.RequestContext) {
	g.once.Do(func() { g.routes = RouteLimits(Spec(g.h)) })
	path := string(c.Path())
	if !isFrameworkPath(path) {
		ip := c.ClientIP()
		if r := g.cfg.PerIP(); !r.Off() {
			if retry := g.over(ctx, "ip:"+ip, r); retry > 0 {
				g.reject(ctx, c, "service", retry)
				return
			}
		}
		if r, ok := g.routes[string(c.Method())+" "+path]; ok && !r.Off() {
			if retry := g.over(ctx, "route:"+path+":"+ip, r); retry > 0 {
				g.reject(ctx, c, "route", retry)
				return
			}
		}
	}
	timeout := g.cfg.RequestTimeout()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c.Next(rctx)
	if errors.Is(rctx.Err(), context.DeadlineExceeded) && len(c.Response.Body()) == 0 {
		metricsx.HTTPRequestsRejected.WithLabelValues("timeout").Inc()
		zlog.Ctx(ctx).Warn("request timed out", zlog.Dur("timeout", timeout))
		c.AbortWithStatusJSON(504, map[string]any{"code": kitexx.CodeRequestTimeout, "msg": "处理超时，请稍后重试"})
	}
}

func (g *guard) reject(ctx context.Context, c *app.RequestContext, scope string, retry time.Duration) {
	metricsx.HTTPRequestsRejected.WithLabelValues("rate_limit").Inc()
	zlog.Ctx(ctx).Warn("rate limited", zlog.Str("scope", scope), zlog.Str("client_ip", c.ClientIP()))
	secs := int(retry.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	c.Response.Header.Set("Retry-After", strconv.Itoa(secs))
	c.AbortWithStatusJSON(429, map[string]any{"code": kitexx.CodeTooManyRequests, "msg": "请求太频繁，请稍后再试"})
}

// over counts one request against the key's window; the time until the
// window ends when the count is over the limit, 0 when within it.
func (g *guard) over(ctx context.Context, key string, r kitexx.Rate) time.Duration {
	now := time.Now()
	start := now.Truncate(r.Window)
	left := start.Add(r.Window).Sub(now)
	if g.rdb != nil {
		rk := "rl:" + g.service + ":" + key + ":" + strconv.FormatInt(start.Unix(), 10)
		pipe := g.rdb.Pipeline()
		incr := pipe.Incr(ctx, rk)
		pipe.PExpire(ctx, rk, r.Window+time.Second)
		if _, err := pipe.Exec(ctx); err == nil {
			if int(incr.Val()) > r.N {
				return left
			}
			return 0
		} else {
			zlog.Ctx(ctx).Warn("rate limit counter unavailable; counting locally", zlog.Err(err))
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.local[key]
	if w == nil || !w.start.Equal(start) {
		if len(g.local) > 10000 {
			for k, v := range g.local {
				if now.Sub(v.start) > time.Hour {
					delete(g.local, k)
				}
			}
		}
		w = &window{start: start}
		g.local[key] = w
	}
	w.n++
	if w.n > r.N {
		return left
	}
	return 0
}

// RouteLimits reads the x-rate-limit of every operation of the OpenAPI
// document (apidoc writes it from `// @limit`), keyed "METHOD /path".
func RouteLimits(spec []byte) map[string]kitexx.Rate {
	out := map[string]kitexx.Rate{}
	if len(spec) == 0 {
		return out
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Limit string `yaml:"x-rate-limit"`
		} `yaml:"paths"`
	}
	if yaml.Unmarshal(spec, &doc) != nil {
		return out
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if op.Limit == "" {
				continue
			}
			r, err := kitexx.ParseRate(op.Limit)
			if err != nil {
				zlog.Warn("bad @limit in the IDL; ignored", zlog.Str("path", path), zlog.Str("limit", op.Limit), zlog.Err(err))
				continue
			}
			out[strings.ToUpper(method)+" "+path] = r
		}
	}
	return out
}
