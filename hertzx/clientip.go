package hertzx

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// ClientIP returns the function that finds the client's address, for
// engine.SetClientIPFunc (hertzx.New and the callback listener install it).
//
// First the edges (client.edges): a request whose key header holds the key
// of an edge takes its address from that edge's IP header, since the edge
// wrote both when it forwarded the request and replaced whatever the client
// had sent under those names. Nothing about the connection is consulted:
// where a CDN connects from is not a fact worth keeping up to date.
//
// Otherwise the connection: its address when the peer is not a trusted
// proxy; when it is, the rightmost address of X-Forwarded-For that is not a
// trusted proxy, since each proxy appends the address it saw and only the
// entries added by our own proxies can be believed. A client that sends
// X-Forwarded-For itself only adds entries to the left of the real one.
// X-Real-IP is not read: a trusted proxy passes on whatever the client put
// there.
//
// Hertz's default trusts every address and takes the leftmost entry, which
// the client chooses: through Cloudflare a request with
// "X-Forwarded-For: <office IP>" passed for the office (found 2026-10-21).
//
// ClientIP 取客户端地址。先看边缘（client.edges）：密钥头对上哪家 CDN，就从那家写的 IP 头取，
// 两个头都是 CDN 回源时覆盖写入的，客户端自带的同名头到不了这里；连接从哪来不参与判断，CDN
// 的回源网段不值得维护。否则按连接：连接方不是可信代理就用连接地址；是可信代理就从
// X-Forwarded-For 右往左取第一个不是可信代理的地址（每层代理在右边追加自己看到的地址，只有我们
// 自己的代理加的才可信，客户端自己带的只会在左边）。不读 X-Real-IP。hertz 默认信任一切并取最
// 左边的值，经 Cloudflare 填办公室 IP 就能冒充（2026-10-21 实测）。
func ClientIP(trusted []*net.IPNet, edges *kitexx.EdgeSet) app.ClientIP {
	forwarded := forwardedIP(trusted)
	return func(c *app.RequestContext) string {
		if r := edgeOf(c, edges, forwarded); r.ip != "" {
			return r.ip
		}
		return forwarded(c)
	}
}

// forwardedIP is the connection rule of ClientIP on its own.
func forwardedIP(trusted []*net.IPNet) func(c *app.RequestContext) string {
	isTrusted := func(ip net.IP) bool {
		// 0.0.0.0 / :: is no network peer: an in-process request (a test,
		// testx.From) has no connection; a real TCP peer never has it.
		if ip.IsUnspecified() {
			return true
		}
		return inAny(ip, trusted)
	}
	return func(c *app.RequestContext) string {
		addr := c.RemoteAddr()
		if addr == nil {
			return ""
		}
		peer, _, err := net.SplitHostPort(strings.TrimSpace(addr.String()))
		if err != nil {
			peer = strings.TrimSpace(addr.String())
		}
		peerIP := net.ParseIP(peer)
		if peerIP == nil || !isTrusted(peerIP) {
			return peer
		}
		client := peer
		var entries []string
		for _, h := range c.Request.Header.PeekAll("X-Forwarded-For") {
			entries = append(entries, strings.Split(string(h), ",")...)
		}
		for i := len(entries) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(entries[i]))
			if ip == nil {
				break // a malformed entry: nothing to its left can be believed
			}
			client = ip.String()
			if !isTrusted(ip) {
				break
			}
		}
		return client
	}
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// The outcomes of the edge check, the result label of http_edge_requests_total.
const (
	edgeNone        = "none"         // no key header: judged by the connection
	edgeBadKey      = "bad_key"      // a key header that matches no edge (or more than one key header)
	edgeBadIP       = "bad_ip"       // a valid key but no single, parseable IP header
	edgeOK          = "ok"           //
	edgePeerOutside = "peer_outside" // ok, but the connection is outside the edge's peer_cidrs
)

// edgeCtxKey keeps the outcome in the request so that it is computed once
// (c.ClientIP() is asked by several middlewares) and the Edge middleware
// and the request log can read it; edgeNameCtxKey is the name alone, for
// the request log and hertzx.Client.
const (
	edgeCtxKey     = "hertzx.edgeResult"
	edgeNameCtxKey = "hertzx.edge"
)

type edgeResult struct {
	edge   string // the edge's name, "" when none matched
	ip     string // the client address the edge wrote, "" when the request is judged by its connection
	result string
	peer   string // with peer_outside: the connection's address
}

// edgeOf evaluates the edge headers of the request once.
func edgeOf(c *app.RequestContext, edges *kitexx.EdgeSet, forwarded func(*app.RequestContext) string) edgeResult {
	if v, ok := c.Get(edgeCtxKey); ok {
		return v.(edgeResult)
	}
	r := edgeResult{result: edgeNone}
	defer func() { c.Set(edgeCtxKey, r) }()
	if !edges.Enabled() {
		return r
	}
	keys := c.Request.Header.PeekAll(edges.KeyHeader)
	if len(keys) == 0 {
		return r
	}
	r.result = edgeBadKey
	if len(keys) != 1 {
		return r // an edge sets exactly one; two means one got through it
	}
	edge, ok := edges.Match(keys[0])
	if !ok {
		return r
	}
	r.edge, r.result = edge.Name, edgeBadIP
	vals := c.Request.Header.PeekAll(edge.IPHeader)
	if len(vals) != 1 {
		return r // same: a header the edge sets arrives exactly once
	}
	ip, ok := parseEdgeIP(string(vals[0]))
	if !ok {
		return r
	}
	r.ip, r.result = ip, edgeOK
	if len(edge.Peers) > 0 {
		peer := forwarded(c)
		if p := net.ParseIP(peer); p == nil || !inAny(p, edge.Peers) {
			r.result, r.peer = edgePeerOutside, peer
		}
	}
	return r
}

// parseEdgeIP reads the address an edge wrote: an IP, or "ip:port" /
// "[ip]:port" (CloudFront-Viewer-Address). IPv4 mapped into IPv6 comes out
// as IPv4, a zone is dropped, so that one client has one spelling in the
// lists, the logs and the rate-limit keys.
func parseEdgeIP(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		host, _, e := net.SplitHostPort(s)
		if e != nil {
			return "", false
		}
		if addr, err = netip.ParseAddr(host); err != nil {
			return "", false
		}
	}
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() || addr.IsUnspecified() {
		return "", false
	}
	return addr.String(), true
}

// Edge is the middleware of client.edges (installed by New after the
// request log, before the guard, so that a refusal is a log record and a
// request from an edge is rate limited by its real address): it evaluates
// the edge headers (through c.ClientIP, once per request), counts the
// outcome in http_edge_requests_total, warns about a key that matches no
// edge, an IP header that cannot be used and a connection outside the
// edge's ranges, puts the edge's name into the request log, removes the key
// header so that no handler, archive or log downstream sees it, and with
// client.edges.required refuses a request no edge vouched for (403, code
// 1011) unless it is a framework path.
func Edge(trusted []*net.IPNet, edges *kitexx.EdgeSet) app.HandlerFunc {
	resolve := ClientIP(trusted, edges)
	return func(ctx context.Context, c *app.RequestContext) {
		// The engine installs the same function for the network path; an
		// in-process request (ut.PerformRequest, testx) does not get it, so
		// the middleware sets it on the request as well.
		c.SetClientIPFunc(resolve)
		resolve(c) // evaluates the edge headers once; the outcome stays in the request
		var r edgeResult
		if v, ok := c.Get(edgeCtxKey); ok {
			r = v.(edgeResult)
		} else {
			r = edgeResult{result: edgeNone}
		}
		c.Request.Header.Del(edges.KeyHeader)
		metricsx.HTTPEdgeRequests.WithLabelValues(r.edge, r.result).Inc()
		if r.edge != "" {
			c.Set(edgeNameCtxKey, r.edge)
			ctx = zlog.CtxWith(ctx, zlog.Str("edge", r.edge))
		}
		log := zlog.Ctx(ctx)
		switch r.result {
		case edgeBadKey:
			log.Warn("edge key not recognised: a key of no edge, or more than one key header")
		case edgeBadIP:
			log.Warn("edge IP header unusable: missing, repeated or not an address", zlog.Str("header", ipHeaderOf(edges, r.edge)))
		case edgePeerOutside:
			log.Warn("edge connection outside the edge's ranges: a leaked key or a stale peer_cidrs", zlog.Str("peer", r.peer))
		}
		if edges.Required && r.ip == "" && !isFrameworkPath(string(c.Path())) {
			metricsx.HTTPRequestsRejected.WithLabelValues("not_via_edge").Inc()
			c.AbortWithStatusJSON(403, map[string]any{"code": kitexx.CodeNotViaEdge, "msg": "请求未经平台入口"})
			return
		}
		c.Next(ctx)
	}
}

func ipHeaderOf(edges *kitexx.EdgeSet, name string) string {
	for _, e := range edges.List {
		if e.Name == name {
			return e.IPHeader
		}
	}
	return edges.IPHeader
}
