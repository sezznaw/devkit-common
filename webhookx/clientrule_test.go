package webhookx

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// edgeKey is the key of the test edge. An invented string.
const edgeKey = "invented-edge-key-0123456789abcdef"

func edgeRule(required bool) kitexx.ClientConfig {
	return kitexx.ClientConfig{Edges: kitexx.EdgesConfig{Required: required, List: []kitexx.EdgeConfig{{Name: "cdn", Keys: []string{edgeKey}}}}}
}

// sourceRoute mounts a callback route of provider "pay" on h whose verifier
// records what it was given, and returns the recorder.
type sourceSeen struct {
	mu      sync.Mutex
	ip      string
	headers http.Header
}

func (s *sourceSeen) get() (string, http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ip, s.headers
}

func sourceRoute(t *testing.T, rt *kitexx.Runtime, h *server.Hertz, path string) *sourceSeen {
	t.Helper()
	seen := &sourceSeen{}
	Handle(rt, h, "pay", path, func(context.Context, *Event) error { return nil }, WithVerifier(func(ev *Event) error {
		seen.mu.Lock()
		seen.ip, seen.headers = ev.ClientIP, ev.Headers.Clone()
		seen.mu.Unlock()
		return nil
	}))
	return seen
}

func header(k, v string) ut.Header { return ut.Header{Key: k, Value: v} }

// TestUseClientRule: an engine a test mounts callback routes on judges a
// request's source by the listener's rule once UseClientRule is on it, for
// in-process requests too (which never get the engine's ClientIP function):
// without it Hertz takes the leftmost X-Forwarded-For entry, the one the
// caller chooses.
func TestUseClientRule(t *testing.T) {
	zlogtest.Capture(t)
	post := func(h *server.Hertz, path string, hs ...ut.Header) int {
		return ut.PerformRequest(h.Engine, "POST", path, &ut.Body{Body: bytesReader("{}"), Len: 2}, hs...).Code
	}
	t.Run("no edges: the rightmost address no trusted proxy vouches for", func(t *testing.T) {
		rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
		h := server.New(server.WithDisablePrintRoute(true))
		if err := UseClientRule(h, kitexx.ClientConfig{}); err != nil {
			t.Fatal(err)
		}
		seen := sourceRoute(t, rt, h, "/cb")
		for xff, want := range map[string]string{
			"13.196.48.67, 198.51.100.9":            "198.51.100.9", // the caller put the first entry there itself
			"198.51.100.9, 13.196.48.67":            "13.196.48.67",
			"13.196.48.67, 198.51.100.9, 10.42.0.7": "198.51.100.9", // our own proxy appended its view
			"203.0.113.9":                           "203.0.113.9",
		} {
			if code := post(h, "/cb", header("X-Forwarded-For", xff)); code != 200 {
				t.Fatalf("%s: status %d", xff, code)
			}
			if ip, _ := seen.get(); ip != want {
				t.Errorf("X-Forwarded-For %q: source %s, want %s", xff, ip, want)
			}
		}
	})
	t.Run("edges required: the source is what the edge wrote, and nothing else gets in", func(t *testing.T) {
		rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
		h := server.New(server.WithDisablePrintRoute(true))
		if err := UseClientRule(h, edgeRule(true)); err != nil {
			t.Fatal(err)
		}
		seen := sourceRoute(t, rt, h, "/cb")
		if code := post(h, "/cb", header("X-Forwarded-For", "13.196.48.67")); code != 403 {
			t.Errorf("no key header: status %d, want 403", code)
		}
		if code := post(h, "/cb", header("X-CDN-Key", "another-invented-key-0123456789abcdef"), header("X-CDN-Client-IP", "13.196.48.67")); code != 403 {
			t.Errorf("a wrong key: status %d, want 403", code)
		}
		if code := post(h, "/cb", header("X-CDN-Key", edgeKey), header("X-CDN-Client-IP", "not-an-address")); code != 403 {
			t.Errorf("a valid key with an unusable IP header: status %d, want 403 (fail closed)", code)
		}
		if ip, _ := seen.get(); ip != "" {
			t.Fatalf("a refused request reached the verifier (source %s)", ip)
		}
		// the edge's header wins over whatever the caller sent along
		if code := post(h, "/cb", header("X-CDN-Key", edgeKey), header("X-CDN-Client-IP", "2a06:98c0:3600::103"), header("X-Forwarded-For", "13.196.48.67")); code != 200 {
			t.Fatalf("via the edge: status %d", code)
		}
		ip, headers := seen.get()
		if ip != "2a06:98c0:3600::103" {
			t.Errorf("source %s, want the edge's 2a06:98c0:3600::103", ip)
		}
		if headers.Get("X-CDN-Key") != "" {
			t.Error("the edge key reached the verifier (and would reach the archive)")
		}
	})
	t.Run("a bad client section is an error", func(t *testing.T) {
		bad := kitexx.ClientConfig{Edges: kitexx.EdgesConfig{List: []kitexx.EdgeConfig{{Name: "cdn", Keys: []string{"short"}}}}}
		if err := UseClientRule(server.New(server.WithDisablePrintRoute(true)), bad); err == nil {
			t.Error("a 5-byte edge key was accepted")
		}
		if err := UseClientRule(server.New(server.WithDisablePrintRoute(true)), kitexx.ClientConfig{TrustedProxies: []string{"not-a-cidr"}}); err == nil {
			t.Error("a trusted proxy that is no CIDR was accepted")
		}
	})
}

// TestServerUsesTheClientRule: the listener itself is built with
// UseClientRule, so what a test gets from it is what production runs.
// Over the network: edges required, the source from the edge's header.
func TestServerUsesTheClientRule(t *testing.T) {
	zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	rt.Config.Service.Name = "ser-vendor"
	port := freePort(t)
	rt.Config.Callbacks = kitexx.CallbacksConfig{Enabled: true, Addr: "127.0.0.1:" + port, MaxBodyBytes: 1 << 20}
	rt.Config.Client = edgeRule(true)
	h := Server(rt)
	seen := sourceRoute(t, rt, h, "/callbacks/pay/rule")
	if err := startAll(rt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kitexx.RunShutdownHooks)
	send := func(hs map[string]string) int {
		t.Helper()
		var resp *http.Response
		var err error
		for i := 0; i < 100; i++ {
			req, _ := http.NewRequest("POST", "http://127.0.0.1:"+port+"/callbacks/pay/rule", strings.NewReader("{}"))
			for k, v := range hs {
				req.Header.Set(k, v)
			}
			if resp, err = oneShot.Do(req); err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
		}
		t.Fatal(err)
		return 0
	}
	if code := send(map[string]string{"X-Forwarded-For": "13.196.48.67"}); code != 403 {
		t.Errorf("not through the edge: status %d, want 403", code)
	}
	if code := send(map[string]string{"X-CDN-Key": edgeKey, "X-CDN-Client-IP": "13.196.48.67", "X-Forwarded-For": "198.51.100.9"}); code != 200 {
		t.Fatalf("through the edge: status %d", code)
	}
	if ip, headers := seen.get(); ip != "13.196.48.67" || headers.Get("X-CDN-Key") != "" {
		t.Errorf("source %s (want 13.196.48.67), key header present: %v", ip, headers.Get("X-CDN-Key") != "")
	}
}
