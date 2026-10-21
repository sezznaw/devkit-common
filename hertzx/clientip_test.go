package hertzx

import (
	"net"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/network"

	"github.com/sezznaw/devkit-common/kitexx"
)

// peerConn is a connection that only knows its remote address.
type peerConn struct {
	network.Conn
	addr net.Addr
}

func (p peerConn) RemoteAddr() net.Addr { return p.addr }

func TestClientIP(t *testing.T) {
	trusted, err := kitexx.ClientConfig{TrustedProxies: []string{"173.245.48.0/20", "2400:cb00::/32"}}.Trusted()
	if err != nil {
		t.Fatal(err)
	}
	resolve := ClientIP(trusted)
	traefik := &net.TCPAddr{IP: net.ParseIP("10.42.0.7"), Port: 5000}
	cases := []struct {
		name string
		peer net.Addr
		xff  []string
		want string
	}{
		{"direct from the internet: headers ignored", &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1}, []string{"61.28.141.85"}, "203.0.113.9"},
		{"through the ingress", traefik, []string{"203.0.113.9"}, "203.0.113.9"},
		{"through Cloudflare, spoofed entry on the left", traefik, []string{"61.28.141.85, 198.51.100.4, 173.245.48.10"}, "198.51.100.4"},
		{"spoof split over two headers", traefik, []string{"61.28.141.85", "198.51.100.4, 173.245.48.10"}, "198.51.100.4"},
		{"in-cluster call: no header", traefik, nil, "10.42.0.7"},
		{"in-cluster chain only", traefik, []string{"10.42.1.5"}, "10.42.1.5"},
		{"malformed rightmost entry: stop at the proxy", traefik, []string{"198.51.100.4, junk"}, "10.42.0.7"},
		{"IPv6 client through Cloudflare", traefik, []string{"2001:db8::1, 2400:cb00::5"}, "2001:db8::1"},
		{"in-process request (testx.From)", &net.TCPAddr{IP: net.IPv4zero}, []string{"198.51.100.4"}, "198.51.100.4"},
	}
	for _, c := range cases {
		ctx := app.NewContext(0)
		ctx.SetConn(peerConn{addr: c.peer})
		for _, v := range c.xff {
			ctx.Request.Header.Add("X-Forwarded-For", v)
		}
		ctx.Request.Header.Set("X-Real-IP", "61.28.141.85") // never read
		if got := resolve(ctx); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if _, err := (kitexx.ClientConfig{TrustedProxies: []string{"not-a-cidr"}}).Trusted(); err == nil {
		t.Fatal("a bad CIDR must be an error")
	}
}
