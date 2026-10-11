package kitexx

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/client/callopt"
	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/loadbalance/lbcache"
	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/devroute"
	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Tagged routing and takeover mode, see package devroute. [takeover]
//
// Nothing here is on the request path unless cfg.DevRoute.Enabled(): APP_ENV
// dev, or a local service in takeover mode. A deployment to uat or prod gets
// the same client and server options as before this existed.

// takeover is the marker of this process, nil outside takeover mode.
func takeover() *config.TakeoverMarker { return config.Takeover() }

// applyTakeover adjusts a configuration for takeover mode, once, in
// Bootstrap: the instance is reached through the tool's tunnel and must not
// announce itself; it runs no background worker (the dev instance keeps
// consuming, relaying and scheduling) and serves no metrics port (two local
// services would collide).
func applyTakeover(cfg *Config) {
	m := takeover()
	if m == nil {
		return
	}
	if m.Service != cfg.Service.Name {
		zlog.Warn("takeover marker is for another service; ignored", zlog.Str("marker", m.Service), zlog.Str("service", cfg.Service.Name), zlog.Str("path", m.Path))
		return
	}
	off := false
	cfg.Nacos.Register = &off
	cfg.Metrics.Enabled = false
	zlog.Info("TAKEOVER MODE: this instance serves the "+m.Env+" environment for requests tagged "+m.Tag,
		zlog.Str("dev_tag", m.Tag), zlog.Str("profile", "conf/"+m.Env+".yaml"),
		zlog.Str("not_done", "register in Nacos, consume events, relay outbox, run delayed tasks, serve metrics"),
		zlog.Int("tunnels", int64(len(m.Rewrite))), zlog.Str("marker", m.Path))
}

// inTakeover says whether this process is the takeover instance of its
// service (the marker is for it).
func inTakeover(cfg Config) bool {
	m := takeover()
	return m != nil && m.Service == cfg.Service.Name
}

// rewriteAddr maps a cluster address to its tunnel in takeover mode.
func rewriteAddr(addr string) string { return config.RewriteAddr(addr) }

// rewriteURL is rewriteAddr for URLs.
func rewriteURL(u string) string { return config.RewriteURL(u) }

// devRouteCallOptions is the server middleware (and what hertzx's DevTag does
// for a gateway) that gives every RPC made from a tagged request the debug
// timeout: someone is on a breakpoint behind it. Kitex reads per-call
// options from the context (client.NewCtxWithCallOptions); a handler that
// passes the request's ctx on, which is the rule, carries them to the
// generated client. The client's own WithRPCTimeout is a lock, which is why
// this is a call option and not a change of the config in a middleware.
func devRouteCallOptions(c devroute.Config) endpoint.Middleware {
	timeout := c.Timeout()
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			return next(WithDebugTimeout(ctx, timeout), req, resp)
		}
	}
}

// WithDebugTimeout returns ctx with the debug timeout as a call option for
// every RPC made with it, when ctx carries dev tags; otherwise ctx itself.
// hertzx uses it for tagged HTTP requests.
func WithDebugTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if len(devroute.Tags(ctx)) == 0 || len(client.CallOptionsFromCtx(ctx)) > 0 {
		return ctx
	}
	return client.NewCtxWithCallOptions(ctx, []callopt.Option{callopt.WithRPCTimeout(timeout)})
}

// devRouteClientOptions are the client options of tagged routing: the
// balancer and the timeout middleware. Empty when routing is off.
func devRouteClientOptions(cfg Config) []client.Option {
	if !cfg.DevRoute.Enabled() {
		return nil
	}
	// A shorter resolver refresh than Kitex's 5s: a takeover coming or going
	// is seen within about two seconds. dev only, like the rest.
	return []client.Option{client.WithLoadBalancer(devroute.NewBalancer(), &lbcache.Options{RefreshInterval: 2 * time.Second})}
}

// takeoverResolver is the resolver of a takeover instance: the services it
// calls are reached through the tool's tunnels (or are local takeovers of
// the same machine), never through the Pod addresses Nacos holds.
type takeoverResolver struct {
	services map[string]string
}

func (r *takeoverResolver) Target(_ context.Context, target rpcinfo.EndpointInfo) string {
	return target.ServiceName()
}

func (r *takeoverResolver) Resolve(_ context.Context, desc string) (discovery.Result, error) {
	addr, ok := r.services[desc]
	if !ok {
		return discovery.Result{}, fmt.Errorf("kitexx: takeover mode has no tunnel to service %q (devkit takeover opens one per service it finds in the cluster; is %q deployed?)", desc, desc)
	}
	return discovery.Result{Cacheable: true, CacheKey: desc, Instances: []discovery.Instance{discovery.NewInstance("tcp", addr, 10, nil)}}, nil
}

func (r *takeoverResolver) Diff(cacheKey string, prev, next discovery.Result) (discovery.Change, bool) {
	return discovery.DefaultDiff(cacheKey, prev, next)
}

func (r *takeoverResolver) Name() string { return "takeover" }

// takeoverWatcher is the second Nacos client of a service with routing on:
// it subscribes to the takeover group and believes an empty list. Nil when
// routing is off. One per process.
var takeoverWatcher struct {
	cli *nacosx.Client
	err error
	did bool
}

func takeoverClient(cfg Config) (*nacosx.Client, error) {
	if takeoverWatcher.did {
		return takeoverWatcher.cli, takeoverWatcher.err
	}
	takeoverWatcher.did = true
	c := cfg.Nacos
	c.Group = devroute.TakeoverGroup
	off := false
	c.Register = &off
	c.WatchServices = nil
	takeoverWatcher.cli, takeoverWatcher.err = nacosx.NewWatcher(c)
	if takeoverWatcher.err != nil {
		takeoverWatcher.err = fmt.Errorf("devroute: cannot subscribe to the %s group: %w", devroute.TakeoverGroup, takeoverWatcher.err)
	}
	return takeoverWatcher.cli, takeoverWatcher.err
}

// mergeInstances builds the instance list a client sees: the baseline
// without any instance that claims a tag (defence: takeover instances live in
// their own group), plus the takeover group's instances with a valid tag.
func mergeInstances(baseline, takeovers []nacosx.Instance) []discovery.Instance {
	out := make([]discovery.Instance, 0, len(baseline)+len(takeovers))
	for _, in := range baseline {
		if tag := in.Metadata[devroute.TagMetadata]; tag != "" {
			zlog.Warn("devroute: an instance of the baseline group claims a dev-tag and is ignored", zlog.Str("addr", in.Addr), zlog.Str("dev_tag", tag))
			continue
		}
		out = append(out, discovery.NewInstance("tcp", in.Addr, int(in.Weight), in.Metadata))
	}
	for _, in := range takeovers {
		tag := in.Metadata[devroute.TagMetadata]
		if !devroute.ValidTag(tag) {
			continue
		}
		out = append(out, discovery.NewInstance("tcp", in.Addr, int(in.Weight), in.Metadata))
	}
	return out
}

// diffWithTags is discovery.DefaultDiff that also sees a change of the
// dev-tag metadata: the same address registered again under another tag must
// move to the other tag's list.
func diffWithTags(cacheKey string, prev, next discovery.Result) (discovery.Change, bool) {
	ch, changed := discovery.DefaultDiff(cacheKey, prev, next)
	if changed {
		return ch, true
	}
	if tagSignature(prev.Instances) != tagSignature(next.Instances) {
		return discovery.Change{Result: next, Added: next.Instances, Removed: prev.Instances}, true
	}
	return ch, false
}

func tagSignature(list []discovery.Instance) string {
	parts := make([]string, 0, len(list))
	for _, in := range list {
		tag, _ := in.Tag(devroute.TagMetadata)
		parts = append(parts, in.Address().String()+"="+tag)
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// localIPs lists this machine's addresses, for the "did my local service
// register itself as a baseline instance" check of the tool; exported for it.
func LocalIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
