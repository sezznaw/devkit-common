package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

type seen struct {
	auth, sig, ts, custom string
	body                  string
}

func authServer(t *testing.T, tokenFetches *atomic.Int32) (*httptest.Server, *seen) {
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			tokenFetches.Add(1)
			if r.FormValue("grant_type") != "client_credentials" || r.FormValue("client_id") != "id1" && r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-123", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		b, _ := io.ReadAll(r.Body)
		s.auth, s.sig, s.ts, s.custom, s.body = r.Header.Get("Authorization"), r.Header.Get("X-Signature"), r.Header.Get("X-Timestamp"), r.Header.Get("X-Vendor-Sig"), string(b)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func TestAuthSchemes(t *testing.T) {
	zlogtest.Capture(t)
	var fetches atomic.Int32
	srv, s := authServer(t, &fetches)
	ctx := context.Background()

	c, err := New("b", Provider{BaseURL: srv.URL, Auth: Auth{Type: "bearer", Token: "abc"}})
	if err != nil {
		t.Fatal(err)
	}
	c.GetJSON(ctx, "/x", nil)
	if s.auth != "Bearer abc" {
		t.Errorf("bearer: %q", s.auth)
	}

	c, _ = New("ba", Provider{BaseURL: srv.URL, Auth: Auth{Type: "basic", Username: "u", Password: "p"}})
	c.GetJSON(ctx, "/x", nil)
	if s.auth != "Basic dTpw" {
		t.Errorf("basic: %q", s.auth)
	}

	c, _ = New("h", Provider{BaseURL: srv.URL, Auth: Auth{Type: "hmac-sha256", Secret: "k"}})
	c.PostJSON(ctx, "/pay", map[string]int{"amount": 1}, nil)
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte("POST\n/pay\n" + s.ts + "\n" + s.body))
	if s.ts == "" || s.sig != hex.EncodeToString(mac.Sum(nil)) || s.body != `{"amount":1}` {
		t.Errorf("hmac: sig=%q ts=%q body=%q", s.sig, s.ts, s.body)
	}

	c, _ = New("o", Provider{BaseURL: srv.URL, Auth: Auth{Type: "oauth2", TokenURL: srv.URL + "/oauth/token", ClientID: "id1", ClientSecret: "sec"}})
	c.GetJSON(ctx, "/x", nil)
	c.GetJSON(ctx, "/x", nil)
	if s.auth != "Bearer tok-123" || fetches.Load() != 1 {
		t.Errorf("oauth2: auth=%q token fetches=%d (cached after the first)", s.auth, fetches.Load())
	}

	c, _ = New("cu", Provider{BaseURL: srv.URL, Auth: Auth{Type: "custom"}})
	if err := c.GetJSON(ctx, "/x", nil); !errors.Is(err, ErrNoSigner) {
		t.Errorf("custom without signer: %v", err)
	}
	c.UseSigner(SignerFunc(func(req *http.Request, body []byte) error {
		req.Header.Set("X-Vendor-Sig", "sig("+req.Method+":"+string(body)+")")
		return nil
	}))
	c.PostJSON(ctx, "/x", map[string]string{"a": "b"}, nil)
	if s.custom != `sig(POST:{"a":"b"})` || s.body != `{"a":"b"}` {
		t.Errorf("custom signer: %q body=%q", s.custom, s.body)
	}

	for _, bad := range []Auth{{Type: "bearer"}, {Type: "basic", Username: "u"}, {Type: "hmac-sha256"}, {Type: "oauth2", TokenURL: "x"}, {Type: "magic"}, {Type: "hmac-sha256", Secret: "k", Encoding: "b32"}} {
		if err := bad.Validate("v"); err == nil || !strings.Contains(err.Error(), "providers.v.auth") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
