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
