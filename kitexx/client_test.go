package kitexx

import (
	"context"
	"testing"
)

func TestClientInContext(t *testing.T) {
	ctx := context.Background()
	if c := ClientOf(ctx); c != (Client{}) {
		t.Fatalf("empty context: %+v", c)
	}
	ctx = WithClient(ctx, Client{Device: "iOS", DeviceID: "dev-1", IP: "203.0.113.9"})
	if got := ClientOf(ctx); got != (Client{Device: "iOS", DeviceID: "dev-1", IP: "203.0.113.9"}) {
		t.Fatalf("round trip: %+v", got)
	}
	// an empty field does not erase what is there
	ctx = WithClient(ctx, Client{IP: "198.51.100.1"})
	if Device(ctx) != "iOS" || ClientIP(ctx) != "198.51.100.1" {
		t.Fatalf("partial: %+v", ClientOf(ctx))
	}
}

func TestDeviceConfigCanonical(t *testing.T) {
	var off DeviceConfig
	if off.Enabled() {
		t.Fatal("no types should be off")
	}
	d := DeviceConfig{Types: []string{"PC", "H5", "Android", "iOS", "Telegram"}}
	for in, want := range map[string]string{"iOS": "iOS", "ios": "iOS", " H5 ": "H5", "android": "Android"} {
		if got, ok := d.Canonical(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "Windows", "iOS2"} {
		if _, ok := d.Canonical(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestEdgeSet(t *testing.T) {
	good := func() ClientConfig {
		var c ClientConfig
		c.Edges.List = []EdgeConfig{
			{Name: "cloudflare", Keys: []string{"cloudflare-key-0123456789abcdef", ""}},
			{Name: "cloudfront", Keys: []string{"cloudfront-key-0123456789abcdef"}, IPHeader: "CloudFront-Viewer-Address", PeerCIDRs: []string{"13.32.0.0/15", "2600:9000::/28"}},
		}
		return c
	}
	e, err := good().EdgeSet()
	if err != nil {
		t.Fatal(err)
	}
	if !e.Enabled() || e.KeyHeader != "X-Edge-Key" || e.IPHeader != "X-Edge-Client-IP" || len(e.List) != 2 {
		t.Fatalf("compiled: %+v", e)
	}
	if len(e.List[0].Keys) != 1 || e.List[0].IPHeader != "X-Edge-Client-IP" || e.List[1].IPHeader != "CloudFront-Viewer-Address" || len(e.List[1].Peers) != 2 {
		t.Fatalf("entries: %+v", e.List)
	}
	if x, ok := e.Match([]byte("cloudfront-key-0123456789abcdef")); !ok || x.Name != "cloudfront" {
		t.Fatalf("match: %+v %v", x, ok)
	}
	if _, ok := e.Match([]byte("cloudfront-key-0123456789abcde")); ok {
		t.Fatal("a near miss matched")
	}
	if names := e.Names(); len(names) != 2 || names[0] != "cloudflare" {
		t.Fatalf("names: %v", names)
	}
	empty, err := (ClientConfig{}).EdgeSet()
	if err != nil || empty.Enabled() || (*EdgeSet)(nil).Enabled() {
		t.Fatalf("nothing configured: %+v %v", empty, err)
	}

	bad := map[string]func(c *ClientConfig){
		"required without edges":  func(c *ClientConfig) { c.Edges.List, c.Edges.Required = nil, true },
		"bad name":                func(c *ClientConfig) { c.Edges.List[0].Name = "Cloud Flare" },
		"name twice":              func(c *ClientConfig) { c.Edges.List[1].Name = "cloudflare" },
		"short key":               func(c *ClientConfig) { c.Edges.List[0].Keys = []string{"short"} },
		"no key (unset variable)": func(c *ClientConfig) { c.Edges.List[0].Keys = []string{"", " "} },
		"key shared":              func(c *ClientConfig) { c.Edges.List[1].Keys = []string{"cloudflare-key-0123456789abcdef"} },
		"key twice":               func(c *ClientConfig) { c.Edges.List[1].Keys = append(c.Edges.List[1].Keys, c.Edges.List[1].Keys[0]) },
		"bad cidr":                func(c *ClientConfig) { c.Edges.List[1].PeerCIDRs = []string{"13.32.0.0/15", "nope"} },
		"bad header name":         func(c *ClientConfig) { c.Edges.KeyHeader = "X Edge Key" },
		"bad ip header name":      func(c *ClientConfig) { c.Edges.List[0].IPHeader = "bad:header" },
		"same header twice":       func(c *ClientConfig) { c.Edges.IPHeader = "x-edge-key" },
		"ip header is the key":    func(c *ClientConfig) { c.Edges.List[0].IPHeader = "X-Edge-Key" },
	}
	for name, mutate := range bad {
		c := good()
		mutate(&c)
		if _, err := c.EdgeSet(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
