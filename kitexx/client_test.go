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
