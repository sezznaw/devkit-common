package metricsx

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListenAddr(t *testing.T) {
	good := []struct{ in, want string }{{"", ":9091"}, {"9100", ":9100"}, {"127.0.0.1:9100", "127.0.0.1:9100"}}
	for _, c := range good {
		got, err := Config{Addr: c.in}.ListenAddr()
		if err != nil || got != c.want {
			t.Errorf("%q: %q %v", c.in, got, err)
		}
	}
	bad := []string{"0", "70000", "nine", "host"}
	for _, b := range bad {
		cfg := Config{Addr: b}
		if _, err := cfg.ListenAddr(); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
}

func TestServe(t *testing.T) {
	RPCServerRequests.WithLabelValues("user", "GetProfile", "ok").Inc()
	addr, shutdown, err := Serve(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`rpc_server_requests_total{code="ok",rpc_method="GetProfile",rpc_service="user"} 1`, "go_goroutines", "process_open_fds"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if r, _ := http.Get("http://" + addr + "/healthz"); r == nil || r.StatusCode != 200 {
		t.Error("/healthz is not 200")
	}
	// The port is taken: Serve says so instead of serving nothing.
	if _, _, err := Serve(Config{Addr: addr}); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("second Serve on %s: %v", addr, err)
	}
}
