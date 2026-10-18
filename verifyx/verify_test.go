package verifyx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestRun(t *testing.T) {
	zlogtest.Discard(t)
	ran := []string{}
	checks := []Check{
		{Name: "ok", Run: func(ctx context.Context) error { ran = append(ran, "ok"); return nil }},
		{Name: "bad", Run: func(ctx context.Context) error { ran = append(ran, "bad"); return errors.New("nope") }},
		{Name: "panics", Run: func(ctx context.Context) error { panic("boom") }},
		{Name: "slow", Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{Name: "after", Run: func(ctx context.Context) error { ran = append(ran, "after"); return nil }},
	}
	err := Run(context.Background(), checks, Options{Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "3 of 5") || !strings.Contains(err.Error(), "bad, panics, slow") {
		t.Fatalf("summary: %v", err)
	}
	if strings.Join(ran, ",") != "ok,bad,after" {
		t.Fatalf("every check runs: %v", ran)
	}
	if err := Run(context.Background(), checks[:1], Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []Check{{Name: "x"}}, Options{}); err == nil {
		t.Fatal("a check without a function is a mistake")
	}
}

func TestRunRetries(t *testing.T) {
	logs := zlogtest.Capture(t)
	fails := 2
	flaky := Check{Name: "flaky", Run: func(ctx context.Context) error {
		if fails > 0 {
			fails--
			return errors.New("not yet")
		}
		return nil
	}}
	err := Run(context.Background(), []Check{flaky}, Options{Timeout: 50 * time.Millisecond, RetryFor: time.Second, RetryEvery: time.Millisecond})
	if err != nil || fails != 0 {
		t.Fatalf("passes within the window: %v (fails left %d)", err, fails)
	}
	if !logs.Has("WARN", "verify retrying") || !logs.Has("INFO", "verify ok") {
		t.Fatalf("retries are logged: %s", logs)
	}

	n := 0
	bad := Check{Name: "bad", Run: func(ctx context.Context) error { n++; return errors.New("nope") }}
	start := time.Now()
	err = Run(context.Background(), []Check{bad}, Options{Timeout: 50 * time.Millisecond, RetryFor: 30 * time.Millisecond, RetryEvery: 5 * time.Millisecond})
	if err == nil || n < 2 || time.Since(start) > time.Second {
		t.Fatalf("fails after the window: %v after %d attempts", err, n)
	}
	// Without RetryFor a check gets one attempt, as before.
	n = 0
	if err := Run(context.Background(), []Check{bad}, Options{}); err == nil || n != 1 {
		t.Fatalf("one attempt: %v %d", err, n)
	}
}

func TestGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			w.Write([]byte("pong"))
		case "/v1/auth/login":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"code":0,"msg":"","data":{"access_token":"t1"}}`))
		case "/v1/member/getProfile":
			if r.Header.Get("Authorization") != "Bearer t1" {
				w.WriteHeader(401)
				w.Write([]byte(`{"code":1004,"msg":"未登录"}`))
				return
			}
			w.Write([]byte(`{"code":2001,"msg":"member not found"}`))
		}
	}))
	defer srv.Close()
	t.Setenv(BaseURLEnv, srv.URL)
	g := NewGateway("http://127.0.0.1:1")
	if g.BaseURL != srv.URL {
		t.Fatal("env wins")
	}
	if b, err := g.Get(context.Background(), "/ping"); err != nil || string(b) != "pong" {
		t.Fatalf("ping %s %v", b, err)
	}
	env, err := g.Post(context.Background(), "/v1/auth/login", map[string]string{"username": "a"})
	if err != nil || !strings.Contains(string(env.Data), "t1") {
		t.Fatalf("login %+v %v", env, err)
	}
	if _, err := g.Post(context.Background(), "/v1/member/getProfile", map[string]any{}); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("401 is an error: %v", err)
	}
	g.Token = "t1"
	env, err = g.Post(context.Background(), "/v1/member/getProfile", map[string]any{})
	if err == nil || env == nil || env.Code != 2001 || !strings.Contains(err.Error(), "code 2001") {
		t.Fatalf("business code is an error with the envelope: %+v %v", env, err)
	}
}
