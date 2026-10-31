package hertzx

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

const (
	cfKey  = "cloudflare-key-0123456789abcdef"
	awsKey = "cloudfront-key-0123456789abcdef"
	awsOld = "cloudfront-old-0123456789abcdef"
)

func testEdges(t *testing.T, required bool) *kitexx.EdgeSet {
	t.Helper()
	cfg := kitexx.ClientConfig{}
	cfg.Edges.Required = required
	cfg.Edges.List = []kitexx.EdgeConfig{
		{Name: "cloudflare", Keys: []string{cfKey, ""}, PeerCIDRs: []string{"10.42.0.0/16"}},
		{Name: "cloudfront", Keys: []string{awsKey, awsOld}, IPHeader: "CloudFront-Viewer-Address"},
	}
	edges, err := cfg.EdgeSet()
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

// The edge rule of ClientIP: the key decides, the IP header is read, the
// connection is only the fallback.
func TestClientIPEdge(t *testing.T) {
	trusted, _ := kitexx.ClientConfig{}.Trusted()
	resolve := ClientIP(trusted, testEdges(t, false))
	traefik := &net.TCPAddr{IP: net.ParseIP("10.42.0.7"), Port: 5000}
	internet := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1}
	type hdr struct{ k, v string }
	cases := []struct {
		name string
		peer net.Addr
		hdrs []hdr
		want string
	}{
		{"cloudflare key: the edge's IP, not the connection", traefik, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "61.28.141.85"}, {"X-Forwarded-For", "95.40.203.136"}}, "61.28.141.85"},
		{"cloudfront key: its own header, ip:port", traefik, []hdr{{"X-Edge-Key", awsKey}, {"CloudFront-Viewer-Address", "61.28.141.85:40312"}, {"X-Forwarded-For", "95.40.203.136"}}, "61.28.141.85"},
		{"cloudfront previous key during a rotation", traefik, []hdr{{"X-Edge-Key", awsOld}, {"CloudFront-Viewer-Address", "[2001:db8::1]:40312"}}, "2001:db8::1"},
		{"IPv4 mapped in IPv6 comes out as IPv4", traefik, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "::ffff:61.28.141.85"}}, "61.28.141.85"},
		{"wrong key: judged by the connection", traefik, []hdr{{"X-Edge-Key", "not-a-key-but-long-enough-xxxx"}, {"X-Edge-Client-IP", "61.28.141.85"}, {"X-Forwarded-For", "95.40.203.136"}}, "95.40.203.136"},
		{"key of one edge, IP header of the other: unusable", traefik, []hdr{{"X-Edge-Key", cfKey}, {"CloudFront-Viewer-Address", "61.28.141.85"}, {"X-Forwarded-For", "95.40.203.136"}}, "95.40.203.136"},
		{"two IP headers: the edge did not replace the client's", traefik, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "1.2.3.4"}, {"X-Edge-Client-IP", "61.28.141.85"}, {"X-Forwarded-For", "95.40.203.136"}}, "95.40.203.136"},
		{"two key headers: same", traefik, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "61.28.141.85"}, {"X-Forwarded-For", "95.40.203.136"}}, "95.40.203.136"},
		{"IP header that is not an address", traefik, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "unknown"}, {"X-Forwarded-For", "95.40.203.136"}}, "95.40.203.136"},
		{"IP header unspecified", traefik, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "0.0.0.0"}, {"X-Forwarded-For", "95.40.203.136"}}, "95.40.203.136"},
		{"a client on the internet forging both headers without a key", internet, []hdr{{"X-Edge-Client-IP", "61.28.141.85"}, {"X-Forwarded-For", "61.28.141.85"}}, "203.0.113.9"},
		{"a direct connection with a valid key still takes the header (the network keeps keys away from the internet)", internet, []hdr{{"X-Edge-Key", cfKey}, {"X-Edge-Client-IP", "61.28.141.85"}}, "61.28.141.85"},
		{"no edge headers at all", traefik, []hdr{{"X-Forwarded-For", "203.0.113.9"}}, "203.0.113.9"},
	}
	for _, c := range cases {
		ctx := app.NewContext(0)
		ctx.SetConn(peerConn{addr: c.peer})
		for _, h := range c.hdrs {
			ctx.Request.Header.Add(h.k, h.v)
		}
		if got := resolve(ctx); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
		if got := resolve(ctx); got != c.want { // memoised: the same answer twice
			t.Errorf("%s: second call got %s, want %s", c.name, got, c.want)
		}
	}
	// no edges configured: a key header is just a header
	plain := ClientIP(trusted, nil)
	ctx := app.NewContext(0)
	ctx.SetConn(peerConn{addr: traefik})
	ctx.Request.Header.Add("X-Edge-Key", cfKey)
	ctx.Request.Header.Add("X-Edge-Client-IP", "61.28.141.85")
	ctx.Request.Header.Add("X-Forwarded-For", "95.40.203.136")
	if got := plain(ctx); got != "95.40.203.136" {
		t.Fatalf("without edges: got %s", got)
	}
}

func edgeServer(t *testing.T, required bool) (*server.Hertz, *kitexx.Client, *bool) {
	t.Helper()
	edges := testEdges(t, required)
	trusted, _ := kitexx.ClientConfig{}.Trusted()
	seen, keyLeaked := &kitexx.Client{}, new(bool)
	h := server.New(server.WithDisablePrintRoute(true))
	h.SetClientIPFunc(ClientIP(trusted, edges))
	h.Use(RequestLog(), Edge(trusted, edges), Client(kitexx.ClientConfig{}))
	h.POST("/v1/x", func(ctx context.Context, c *app.RequestContext) {
		*seen = kitexx.ClientOf(ctx)
		*keyLeaked = len(c.Request.Header.Peek("X-Edge-Key")) > 0
		c.String(200, "ok")
	})
	h.GET("/ping", func(ctx context.Context, c *app.RequestContext) { c.String(200, "pong") })
	return h, seen, keyLeaked
}

func TestEdgeMiddleware(t *testing.T) {
	buf := zlogtest.Capture(t)
	h, seen, keyLeaked := edgeServer(t, true)
	hd := func(k, v string) ut.Header { return ut.Header{Key: k, Value: v} }
	count := func(edge, result string) float64 {
		return testutil.ToFloat64(metricsx.HTTPEdgeRequests.WithLabelValues(edge, result))
	}
	okBefore, noneBefore, badBefore, outsideBefore := count("cloudflare", "ok"), count("", "none"), count("", "bad_key"), count("cloudflare", "peer_outside")
	refusedBefore := testutil.ToFloat64(metricsx.HTTPRequestsRejected.WithLabelValues("not_via_edge"))

	// through an edge: served, the edge's IP and name in the context, the key gone
	w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, hd("X-Edge-Key", cfKey), hd("X-Edge-Client-IP", "61.28.141.85"), hd("X-Forwarded-For", "10.42.3.3"))
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("via edge: %d %s", w.Code, w.Body.String())
	}
	if *seen != (kitexx.Client{IP: "61.28.141.85", Edge: "cloudflare"}) {
		t.Fatalf("client in context: %+v", *seen)
	}
	if *keyLeaked {
		t.Fatal("the key header reached the handler")
	}
	if out := buf.String(); !strings.Contains(out, "edge=cloudflare") && !strings.Contains(out, `"edge":"cloudflare"`) {
		t.Fatalf("request log without edge: %s", out)
	}
	if strings.Contains(buf.String(), cfKey) {
		t.Fatal("the key was logged")
	}
	if got := count("cloudflare", "ok") - okBefore; got != 1 {
		t.Fatalf("ok counted %v", got)
	}

	// required: no key is 403 / 1011, counted, logged as a warning with the path
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, hd("X-Forwarded-For", "95.40.203.136"))
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":1011`) {
		t.Fatalf("no key: %d %s", w.Code, w.Body.String())
	}
	if got := count("", "none") - noneBefore; got != 1 {
		t.Fatalf("none counted %v", got)
	}
	if got := testutil.ToFloat64(metricsx.HTTPRequestsRejected.WithLabelValues("not_via_edge")) - refusedBefore; got != 1 {
		t.Fatalf("refusal counted %v", got)
	}
	// a wrong key is refused too and warned about
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, hd("X-Edge-Key", "guess-guess-guess-guess-guess"), hd("X-Edge-Client-IP", "61.28.141.85"))
	if w.Code != 403 {
		t.Fatalf("bad key: %d", w.Code)
	}
	if got := count("", "bad_key") - badBefore; got != 1 {
		t.Fatalf("bad_key counted %v", got)
	}
	if !strings.Contains(buf.String(), "edge key not recognised") {
		t.Fatalf("no warning for a bad key: %s", buf.String())
	}
	// a valid key with an unusable IP header is refused: fail closed
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, hd("X-Edge-Key", cfKey), hd("X-Edge-Client-IP", "nope"))
	if w.Code != 403 {
		t.Fatalf("bad ip: %d", w.Code)
	}
	// framework paths are never refused
	if w = ut.PerformRequest(h.Engine, "GET", "/ping", nil); w.Code != 200 || w.Body.String() != "pong" {
		t.Fatalf("ping: %d %s", w.Code, w.Body.String())
	}
	// a connection outside the edge's ranges is served and counted
	w = ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, hd("X-Edge-Key", cfKey), hd("X-Edge-Client-IP", "61.28.141.85"), hd("X-Forwarded-For", "198.51.100.7"))
	if w.Code != 200 || seen.IP != "61.28.141.85" {
		t.Fatalf("peer outside: %d %+v", w.Code, *seen)
	}
	if got := count("cloudflare", "peer_outside") - outsideBefore; got != 1 {
		t.Fatalf("peer_outside counted %v", got)
	}
	if !strings.Contains(buf.String(), "outside the edge's ranges") || !strings.Contains(buf.String(), "198.51.100.7") {
		t.Fatalf("no warning for a peer outside: %s", buf.String())
	}
}

// Not required: a request without a key is judged by its connection.
func TestEdgeOptional(t *testing.T) {
	zlogtest.Capture(t)
	h, seen, _ := edgeServer(t, false)
	w := ut.PerformRequest(h.Engine, "POST", "/v1/x", nil, ut.Header{Key: "X-Forwarded-For", Value: "203.0.113.9"})
	if w.Code != 200 || *seen != (kitexx.Client{IP: "203.0.113.9"}) {
		t.Fatalf("no key, not required: %d %+v", w.Code, *seen)
	}
}

func TestParseEdgeIP(t *testing.T) {
	cases := map[string]string{
		"61.28.141.85":           "61.28.141.85",
		" 61.28.141.85 ":         "61.28.141.85",
		"61.28.141.85:1234":      "61.28.141.85",
		"2001:db8::1":            "2001:db8::1",
		"[2001:db8::1]:1234":     "2001:db8::1",
		"::ffff:61.28.141.85":    "61.28.141.85",
		"fe80::1%eth0":           "fe80::1",
		"":                       "",
		"unknown":                "",
		"0.0.0.0":                "",
		"::":                     "",
		"61.28.141.85, 1.2.3.4":  "",
		"61.28.141.85:notaport:": "",
	}
	for in, want := range cases {
		got, ok := parseEdgeIP(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: got %q %v, want %q", in, got, ok, want)
		}
	}
}
