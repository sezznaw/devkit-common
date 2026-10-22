package kitexx

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/golang-jwt/jwt/v5"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// a cluster in miniature: an RSA key whose public half a TLS server serves as JWKS, and the projected files.
type fakeCluster struct {
	key  *rsa.PrivateKey
	kid  string
	srv  *httptest.Server
	dir  string
	cfg  ServiceIdentityConfig
	hits int
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCluster{key: key, kid: "k1", dir: t.TempDir()}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits++
		if r.Header.Get("Authorization") != "Bearer api-token-value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": c.kid, "alg": "RS256", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(c.srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw})
	for name, data := range map[string][]byte{"ca.crt": ca, "api-token": []byte("api-token-value\n")} {
		if err := os.WriteFile(filepath.Join(c.dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.cfg = ServiceIdentityConfig{Dir: c.dir, JWKSURL: c.srv.URL + "/openid/v1/jwks", Issuer: "https://kubernetes.default.svc.cluster.local",
		Namespaces: []string{"sportsbook-dev", "sportsbook-dev-provider"}}
	return c
}

func (c *fakeCluster) token(t *testing.T, key *rsa.PrivateKey, sub, aud string, exp time.Time) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer: "https://kubernetes.default.svc.cluster.local", Subject: sub, Audience: jwt.ClaimStrings{aud}, ExpiresAt: jwt.NewNumericDate(exp)})
	tok.Header["kid"] = c.kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServiceIdentity(t *testing.T) {
	zlogtest.Discard(t)
	c := newFakeCluster(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	hour := time.Now().Add(time.Hour)
	sa := func(ns, name string) string { return "system:serviceaccount:" + ns + ":" + name }
	call := func(from, token string) context.Context {
		ctx := callerCtx(from, "Issue")
		if token != "" {
			ctx = metainfo.WithValue(ctx, SvcTokenKey, token)
		}
		return ctx
	}
	cases := []struct {
		name   string
		ctx    context.Context
		proven string
	}{
		{"the real ser-admin-api", call("ser-admin-api", c.token(t, c.key, sa("sportsbook-dev", "ser-admin-api"), "sportsbook-rpc", hour)), "ser-admin-api"},
		{"ser-vendor from the provider namespace", call("ser-vendor", c.token(t, c.key, sa("sportsbook-dev-provider", "ser-vendor"), "sportsbook-rpc", hour)), "ser-vendor"},
		{"no token", call("ser-admin-api", ""), ""},
		{"ser-vendor's token, calling itself ser-admin-api", call("ser-admin-api", c.token(t, c.key, sa("sportsbook-dev-provider", "ser-vendor"), "sportsbook-rpc", hour)), ""},
		{"signed by another key", call("ser-admin-api", c.token(t, other, sa("sportsbook-dev", "ser-admin-api"), "sportsbook-rpc", hour)), ""},
		{"the API audience, not ours", call("ser-admin-api", c.token(t, c.key, sa("sportsbook-dev", "ser-admin-api"), "https://kubernetes.default.svc.cluster.local", hour)), ""},
		{"expired", call("ser-admin-api", c.token(t, c.key, sa("sportsbook-dev", "ser-admin-api"), "sportsbook-rpc", time.Now().Add(-time.Hour))), ""},
		{"a ServiceAccount of the CI namespace", call("ser-admin-api", c.token(t, c.key, sa("gitlab-runner", "ser-admin-api"), "sportsbook-rpc", hour)), ""},
		{"not a ServiceAccount", call("ser-admin-api", c.token(t, c.key, "system:node:x", "sportsbook-rpc", hour)), ""},
		{"garbage", call("ser-admin-api", "a.b.c"), ""},
	}
	for _, mode := range []string{IdentityLog, IdentityEnforce} {
		cfg := c.cfg
		cfg.Mode = mode
		mw, err := ServiceIdentityCheck(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			seen, called := "", false
			_ = mw(func(ctx context.Context, req, resp any) error { called, seen = true, VerifiedCaller(ctx); return nil })(tc.ctx, nil, nil)
			if seen != tc.proven {
				t.Fatalf("%s, %s: proven %q, want %q", mode, tc.name, seen, tc.proven)
			}
			wantCalled := mode == IdentityLog || tc.proven != ""
			if called != wantCalled {
				t.Fatalf("%s, %s: called=%v", mode, tc.name, called)
			}
			if !called && busyCode(tc.ctx) != CodeForbidden {
				t.Fatalf("%s, %s: want 1007, got %d", mode, tc.name, busyCode(tc.ctx))
			}
		}
	}

	// callers matches the proven name, not the one the caller writes
	callers := Callers("ser-auth", CallersConfig{Rules: []CallerRule{{Methods: []string{"Issue"}, Allow: []string{"ser-admin-api"}}}})
	cfg := c.cfg
	cfg.Mode = IdentityEnforce
	mw, _ := ServiceIdentityCheck(cfg)
	ep := mw(callers(func(ctx context.Context, req, resp any) error { return nil }))
	good := call("ser-admin-api", c.token(t, c.key, sa("sportsbook-dev", "ser-admin-api"), "sportsbook-rpc", hour))
	if err := ep(good, nil, nil); err != nil || busyCode(good) != 0 {
		t.Fatalf("proven ser-admin-api refused: %v %d", err, busyCode(good))
	}
	forged := call("ser-admin-api", c.token(t, c.key, sa("sportsbook-dev", "ser-api"), "sportsbook-rpc", hour))
	if _ = ep(forged, nil, nil); busyCode(forged) != CodeForbidden {
		t.Fatalf("ser-api writing ser-admin-api in the header got through: %d", busyCode(forged))
	}

	// enforce refuses to start without the cluster's keys; off needs nothing; a bad mode stops the start
	bad := c.cfg
	bad.Mode, bad.JWKSURL = IdentityEnforce, c.srv.URL+"/nowhere-but-401"
	_ = os.WriteFile(filepath.Join(c.dir, "api-token"), []byte("wrong"), 0o600)
	if _, err := ServiceIdentityCheck(bad); err == nil {
		t.Fatal("enforce started without the keys")
	}
	if _, err := ServiceIdentityCheck(ServiceIdentityConfig{}); err != nil {
		t.Fatalf("off: %v", err)
	}
	if _, err := ServiceIdentityCheck(ServiceIdentityConfig{Mode: "strict"}); err == nil {
		t.Fatal("bad mode accepted")
	}
}

// The client attaches the pod's projected token to every call, one hop; without the file, nothing.
func TestClientServiceToken(t *testing.T) {
	dir := t.TempDir()
	var got string
	next := func(ctx context.Context, req, resp any) error {
		got, _ = metainfo.GetValue(ctx, SvcTokenKey)
		return nil
	}
	mw := ClientServiceToken(ServiceIdentityConfig{Dir: dir})
	_ = mw(next)(context.Background(), nil, nil)
	if got != "" {
		t.Fatalf("no file, yet %q attached", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "rpc-token"), []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mw = ClientServiceToken(ServiceIdentityConfig{Dir: dir})
	_ = mw(next)(context.Background(), nil, nil)
	if got != "tok-1" {
		t.Fatalf("attached %q", got)
	}
}
