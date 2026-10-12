package kitexx

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// it subscribes to the takeover group and believes an empty list. One per
// process; a failure to make it is a warning (the baseline is the service,
// the takeover group a convenience) and leaves routing off for this process.
var takeoverWatcher struct {
	once sync.Once
	cli  *nacosx.Client
}

func takeoverClient(cfg Config) *nacosx.Client {
	takeoverWatcher.once.Do(func() {
		c := cfg.Nacos
		c.Group = devroute.TakeoverGroup
		off := false
		c.Register = &off
		c.WatchServices = nil
		cli, err := nacosx.NewWatcher(c)
		if err != nil {
			zlog.Warn("devroute: cannot subscribe to the takeover group; tagged routing is off in this process", zlog.Str("group", devroute.TakeoverGroup), zlog.Err(err))
			return
		}
		takeoverWatcher.cli = cli
	})
	return takeoverWatcher.cli
}

var guardOnce struct {
	once sync.Once
	g    *instanceGuard
}

// instanceGuardFor is the process's guard, made once.
func instanceGuardFor(cfg Config) *instanceGuard {
	guardOnce.once.Do(func() { guardOnce.g = newInstanceGuard(cfg) })
	return guardOnce.g
}

// instanceGuard decides which instances a client may be sent to. Baseline
// instances must live in the Pod network this process is in (by default its
// own /16; conf devroute.baseline_cidrs overrides); takeover instances must
// not, and must sit on the port range devkit takeover binds on the node.
// Without this, anyone holding the environment's Nacos account could
// register a plain instance that points at their own machine and receive
// everybody's requests. [takeover]
type instanceGuard struct {
	baseline []*net.IPNet
}

func newInstanceGuard(cfg Config) *instanceGuard {
	g := &instanceGuard{}
	for _, c := range cfg.DevRoute.BaselineCIDRs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			g.baseline = append(g.baseline, n)
		} else {
			zlog.Warn("devroute.baseline_cidrs: not a CIDR, ignored", zlog.Str("value", c))
		}
	}
	if len(g.baseline) == 0 {
		if own := net.ParseIP(nacosx.OwnIP(cfg.Nacos.Addrs)); own != nil && own.To4() != nil {
			g.baseline = []*net.IPNet{{IP: own.To4().Mask(net.CIDRMask(16, 32)), Mask: net.CIDRMask(16, 32)}}
		}
	}
	if len(g.baseline) == 0 {
		zlog.Warn("devroute: cannot tell the Pod network; baseline instances are not checked (set devroute.baseline_cidrs)")
	} else {
		zlog.Info("devroute: baseline instances must be in", zlog.Any("cidrs", cidrStrings(g.baseline)))
	}
	return g
}

func cidrStrings(list []*net.IPNet) []string {
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.String())
	}
	return out
}

func (g *instanceGuard) inBaselineNet(ip net.IP) bool {
	for _, n := range g.baseline {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// allowBaseline: a baseline instance is one of the deployment's own Pods.
func (g *instanceGuard) allowBaseline(addr string) bool {
	if g == nil || len(g.baseline) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && g.inBaselineNet(ip)
}

// allowTakeover: a takeover instance is the node's reverse tunnel, never a
// Pod address.
func (g *instanceGuard) allowTakeover(addr string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < devroute.TakeoverPortMin || port >= devroute.TakeoverPortMax {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return g == nil || len(g.baseline) == 0 || !g.inBaselineNet(ip)
}

// mergeInstances builds the instance list a client sees: the baseline
// without any instance that claims a tag or sits outside the Pod network,
// plus the takeover group's instances with a valid tag on the node's tunnel
// ports. Refused instances are logged once per address.
func mergeInstances(baseline, takeovers []nacosx.Instance, g *instanceGuard) []discovery.Instance {
	out := make([]discovery.Instance, 0, len(baseline)+len(takeovers))
	for _, in := range baseline {
		if tag := in.Metadata[devroute.TagMetadata]; tag != "" {
			refused(in.Addr, "a baseline instance claims a dev-tag")
			continue
		}
		if !g.allowBaseline(in.Addr) {
			refused(in.Addr, "a baseline instance outside the Pod network (someone registered their own address?)")
			continue
		}
		out = append(out, discovery.NewInstance("tcp", in.Addr, int(in.Weight), in.Metadata))
	}
	for _, in := range takeovers {
		tag := in.Metadata[devroute.TagMetadata]
		if !devroute.ValidTag(tag) {
			continue
		}
		if !g.allowTakeover(in.Addr) {
			refused(in.Addr, "a takeover instance not on the node's tunnel ports")
			continue
		}
		out = append(out, discovery.NewInstance("tcp", in.Addr, int(in.Weight), in.Metadata))
	}
	return out
}

var refusedSeen sync.Map

// refused logs an instance the guard dropped, once per address.
func refused(addr, why string) {
	if _, loaded := refusedSeen.LoadOrStore(addr+"|"+why, struct{}{}); !loaded {
		zlog.Warn("devroute: instance ignored: "+why, zlog.Str("addr", addr))
	}
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
