package kitexx

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/bytedance/gopkg/cloud/metainfo"
)

// The client that made the request (the browser or app behind the gateway)
// travels to the RPC services as persistent metainfo, like the identity and
// the trace id: its device type and device id, which only the client knows
// and reports in the request headers, and its IP, which only the gateway
// knows. A handler anywhere in the chain asks Device(ctx) / ClientIP(ctx)
// and no request body carries them (hertzx.Client is the gateway side).
//
// 发请求的客户端（网关后面的浏览器或 App）和 uid、trace_id 一样作为 metainfo 持久值跨每一跳：
// 设备类型和设备号只有客户端知道，由请求头报上来；IP 只有网关知道。链路上任何 handler 用
// Device(ctx) / ClientIP(ctx) 取，请求体里不再放这些字段（网关那边是 hertzx.Client）。
const (
	DeviceKey   = "DEVICE"
	DeviceIDKey = "DEVICE_ID"
	ClientIPKey = "CLIENT_IP"
	EdgeKey     = "EDGE"
)

// Client is what the gateway learned about the caller of a request.
type Client struct {
	Device   string // device type, one of client.device.types ("PC", "iOS", ...); "" unknown
	DeviceID string // the client's own device identifier; "" when not sent
	IP       string // the client IP the gateway saw; "" outside a request
	Edge     string // the edge (CDN, client.edges) that proved it forwarded the request; "" when none did
}

// WithClient puts c into ctx for this process and for every RPC made with
// ctx. Empty fields are not stored. The gateway's Client middleware calls
// it; tests of RPC handlers call it to play a request from a device.
func WithClient(ctx context.Context, c Client) context.Context {
	if c.Device != "" {
		ctx = metainfo.WithPersistentValue(ctx, DeviceKey, c.Device)
	}
	if c.DeviceID != "" {
		ctx = metainfo.WithPersistentValue(ctx, DeviceIDKey, c.DeviceID)
	}
	if c.IP != "" {
		ctx = metainfo.WithPersistentValue(ctx, ClientIPKey, c.IP)
	}
	if c.Edge != "" {
		ctx = metainfo.WithPersistentValue(ctx, EdgeKey, c.Edge)
	}
	return ctx
}

// ClientOf returns everything known about the request's client.
func ClientOf(ctx context.Context) Client {
	return Client{Device: Device(ctx), DeviceID: DeviceID(ctx), IP: ClientIP(ctx), Edge: Edge(ctx)}
}

// Edge is the name of the edge (client.edges) that forwarded the request
// and proved it with its key, "" when the request came in another way.
func Edge(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, EdgeKey)
	return v
}

// Device is the device type of the request's client ("PC", "H5", "iOS", ...
// as client.device.types of the gateway spells them), "" when unknown.
func Device(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, DeviceKey)
	return v
}

// DeviceID is the device identifier the client sent, "" when it sent none.
func DeviceID(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, DeviceIDKey)
	return v
}

// ClientIP is the IP the gateway saw the request come from, "" when the
// context did not start at a gateway (a job, a consumer, a test).
func ClientIP(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, ClientIPKey)
	return v
}

// ClientConfig is the `client:` section of an API service: what it learns
// about the caller from the request headers and passes on to the RPC
// services. The IP is always taken (from the connection, or the proxy's
// X-Forwarded-For); the device type only when client.device.types is set.
type ClientConfig struct {
	Device DeviceConfig `yaml:"device"`
	// TrustedProxies lists, as CIDRs, the proxies in front of the service
	// besides the private networks and loopback, which are always trusted
	// (the ingress, a load balancer in the VPC): a CDN the traffic comes
	// through, such as Cloudflare's ranges. The client IP is the rightmost
	// address of X-Forwarded-For that is not a trusted proxy; whatever a
	// client puts in front of it is ignored. X-Real-IP is never read.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Edges is how the CDNs the service is published through identify
	// themselves, so that the client IP is taken from what they wrote and
	// never from where the connection came from. See EdgesConfig.
	Edges EdgesConfig `yaml:"edges"`
}

// defaultTrustedProxies are always trusted: the cluster, the VPC, loopback.
var defaultTrustedProxies = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "::1/128", "fc00::/7"}

// Trusted returns the trusted proxy networks: the defaults plus
// client.trusted_proxies; an entry that is not a CIDR or an IP is an error.
func (c ClientConfig) Trusted() ([]*net.IPNet, error) {
	return parseCIDRs("client.trusted_proxies", append(append([]string{}, defaultTrustedProxies...), c.TrustedProxies...))
}

// parseCIDRs parses networks; a bare address is its own /32 or /128.
func parseCIDRs(field string, list []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
				s += "/32"
			} else {
				s += "/128"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a CIDR: %w", field, s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// EdgesConfig is `client.edges`: the edges (CDNs) the service is published
// through and how each one proves, on every request, that it is the one
// that forwarded it. An edge sets two request headers when it forwards to
// the origin, with "set" semantics so that whatever the client sent under
// those names is replaced: the key header with a secret only that edge and
// we know, and the IP header with the address it accepted the connection
// from. A request whose key header matches an edge's key takes its client
// IP from that edge's IP header; any other request is judged by its
// connection (ClientConfig.TrustedProxies), which nobody can forge either.
// So the source address of the connection never decides who is trusted:
// no CDN ranges to keep up to date, any number of CDNs and domains.
//
// With Required, a request that no edge vouches for is refused (403, code
// 1011) instead of being judged by its connection: in production the IP
// drives the region restrictions, and a wrong IP is worse than an outage,
// so a CDN whose configuration lost the headers fails loudly. Framework
// paths (/ping, /docs) are never refused. Leave it off where requests
// come in without a CDN on purpose (a developer's machine, the API tester).
//
// client.edges 是服务前面的各家 CDN（边缘）怎么证明「这个请求是我转发的」：每家 CDN 回源时
// 用「设置」语义写两个请求头（客户端自带的同名头会被覆盖）：密钥头，值是只有它和我们知道
// 的密钥；IP 头，值是它接受连接的那个地址。密钥对得上哪家，客户端 IP 就从那家写的 IP 头取；
// 对不上的请求按连接判定（trusted_proxies），同样伪造不了。来源 IP 从此不参与「可不可信」：
// 不用维护 CDN 网段，几家 CDN、多少域名都一样。required 时没有任何 CDN 担保的业务请求一律
// 拒绝（403、1011）而不是退回按连接判定：正式服的 IP 驱动地区限制，错的 IP 比停服更糟，CDN
// 配置丢了回源头要立刻报错；/ping、/docs 不拒。本机、api-tester 这类不经 CDN 的地方不要开。
type EdgesConfig struct {
	Required bool `yaml:"required"`
	// KeyHeader is the request header the edges put their key in; default
	// X-Edge-Key. IPHeader is where they put the client address; default
	// X-Edge-Client-IP. An edge that cannot write a variable into a header
	// of our name overrides IPHeader in its own entry.
	KeyHeader string       `yaml:"key_header"`
	IPHeader  string       `yaml:"ip_header"`
	List      []EdgeConfig `yaml:"list"`
}

// EdgeConfig is one edge of client.edges.
type EdgeConfig struct {
	// Name identifies the edge in logs, metrics and kitexx.Edge(ctx):
	// lowercase letters, digits and '-', unique.
	Name string `yaml:"name"`
	// Keys are the secrets this edge may present, from the environment
	// ("${EDGE_KEY_CLOUDFLARE}"), each at least 16 bytes; two during a
	// rotation (the new one first), an empty value is ignored so the
	// previous key's variable may be unset. A key belongs to one edge.
	Keys []string `yaml:"keys"`
	// IPHeader overrides EdgesConfig.IPHeader for this edge: the header this
	// CDN writes the client address into when it cannot use ours (a value of
	// "ip:port" is accepted too, CloudFront-Viewer-Address).
	IPHeader string `yaml:"ip_header"`
	// PeerCIDRs are the ranges this edge publishes for its origin
	// connections. Not a condition: a request with a valid key whose
	// connection (as TrustedProxies sees it) is outside them is still
	// served, with its edge IP, but counted (http_edge_requests_total
	// result="peer_outside") and warned about, which is how a leaked key or
	// a stale list shows up without anything breaking.
	PeerCIDRs []string `yaml:"peer_cidrs"`
}

// EdgeSet is EdgesConfig compiled: what hertzx.ClientIP and hertzx.Edge use.
type EdgeSet struct {
	Required  bool
	KeyHeader string
	IPHeader  string
	List      []EdgeRule
}

// EdgeRule is one compiled edge.
type EdgeRule struct {
	Name     string
	Keys     [][]byte
	IPHeader string
	Peers    []*net.IPNet
}

// Enabled says whether any edge is configured.
func (e *EdgeSet) Enabled() bool { return e != nil && len(e.List) > 0 }

// Names lists the edges, for the start-up record.
func (e *EdgeSet) Names() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.List))
	for _, x := range e.List {
		out = append(out, x.Name)
	}
	return out
}

// Default header names of client.edges.
const (
	DefaultEdgeKeyHeader = "X-Edge-Key"
	DefaultEdgeIPHeader  = "X-Edge-Client-IP"
	// EdgeKeyMinBytes is the shortest key accepted: a short one is guessable.
	EdgeKeyMinBytes = 16
)

// CodeNotViaEdge is the business code of a request refused because no edge
// vouched for it while client.edges.required is on (HTTP 403); declared in
// the idl repository's errors/common.thrift.
const CodeNotViaEdge int32 = 1011

var edgeNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// EdgeSet compiles client.edges; a mistake in it is an error at start.
func (c ClientConfig) EdgeSet() (*EdgeSet, error) {
	cfg := c.Edges
	out := &EdgeSet{Required: cfg.Required, KeyHeader: strings.TrimSpace(cfg.KeyHeader), IPHeader: strings.TrimSpace(cfg.IPHeader)}
	if out.KeyHeader == "" {
		out.KeyHeader = DefaultEdgeKeyHeader
	}
	if out.IPHeader == "" {
		out.IPHeader = DefaultEdgeIPHeader
	}
	for _, h := range []string{out.KeyHeader, out.IPHeader} {
		if !validHeaderName(h) {
			return nil, fmt.Errorf("client.edges: %q is not a header name", h)
		}
	}
	if strings.EqualFold(out.KeyHeader, out.IPHeader) {
		return nil, fmt.Errorf("client.edges: key_header and ip_header are the same header %q", out.KeyHeader)
	}
	if cfg.Required && len(cfg.List) == 0 {
		return nil, fmt.Errorf("client.edges: required is on but no edge is listed; every request would be refused")
	}
	names := map[string]bool{}
	var allKeys [][]byte
	for i, e := range cfg.List {
		name := strings.TrimSpace(e.Name)
		if !edgeNameRe.MatchString(name) {
			return nil, fmt.Errorf("client.edges.list[%d]: name %q must be 1 to 32 of [a-z0-9-]", i, e.Name)
		}
		if names[name] {
			return nil, fmt.Errorf("client.edges.list: edge %q listed twice", name)
		}
		names[name] = true
		x := EdgeRule{Name: name, IPHeader: strings.TrimSpace(e.IPHeader)}
		if x.IPHeader == "" {
			x.IPHeader = out.IPHeader
		} else if !validHeaderName(x.IPHeader) {
			return nil, fmt.Errorf("client.edges.list[%s]: ip_header %q is not a header name", name, e.IPHeader)
		}
		if strings.EqualFold(x.IPHeader, out.KeyHeader) {
			return nil, fmt.Errorf("client.edges.list[%s]: ip_header is the key header", name)
		}
		for _, k := range e.Keys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue // ${EDGE_KEY_X_PREV} outside a rotation
			}
			if len(k) < EdgeKeyMinBytes {
				return nil, fmt.Errorf("client.edges.list[%s]: a key shorter than %d bytes is not accepted", name, EdgeKeyMinBytes)
			}
			for _, other := range allKeys {
				if subtle.ConstantTimeCompare(other, []byte(k)) == 1 {
					return nil, fmt.Errorf("client.edges.list[%s]: a key is shared with another edge or listed twice", name)
				}
			}
			x.Keys = append(x.Keys, []byte(k))
			allKeys = append(allKeys, []byte(k))
		}
		if len(x.Keys) == 0 {
			return nil, fmt.Errorf("client.edges.list[%s]: no key set (is the environment variable exported?)", name)
		}
		peers, err := parseCIDRs("client.edges.list["+name+"].peer_cidrs", e.PeerCIDRs)
		if err != nil {
			return nil, err
		}
		x.Peers = peers
		out.List = append(out.List, x)
	}
	return out, nil
}

// Match finds the edge whose key the request presented, in constant time
// over every key so that the comparison itself says nothing about them.
func (e *EdgeSet) Match(presented []byte) (EdgeRule, bool) {
	var found EdgeRule
	ok := 0
	for _, x := range e.List {
		for _, k := range x.Keys {
			if subtle.ConstantTimeCompare(k, presented) == 1 && ok == 0 {
				found, ok = x, 1
			}
		}
	}
	return found, ok == 1
}

// validHeaderName accepts an HTTP token: no spaces, separators or control characters.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// DeviceConfig is `client.device`: the device types the API service
// accepts in X-Device-Type and whether every request must send one.
type DeviceConfig struct {
	// Required refuses a request without a valid X-Device-Type with code
	// 1001. Off, the header is read when present and ignored when absent.
	Required bool `yaml:"required"`
	// Types lists the accepted values, as they are stored and shown:
	// ["PC", "H5", "Android", "iOS", "Telegram"]. Matching ignores case
	// and the stored value is the spelling listed here. Empty: the header
	// is not read at all.
	Types []string `yaml:"types"`
}

// Enabled says whether X-Device-Type is read: when types are listed.
func (d DeviceConfig) Enabled() bool { return len(d.Types) > 0 }

// Canonical returns the listed spelling of a header value ("ios" ->
// "iOS") and whether the value is one of the types.
func (d DeviceConfig) Canonical(v string) (string, bool) {
	v = strings.TrimSpace(v)
	for _, t := range d.Types {
		if strings.EqualFold(t, v) {
			return t, true
		}
	}
	return "", false
}
