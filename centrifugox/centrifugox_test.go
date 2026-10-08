package centrifugox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// fakeCentrifugo answers the three API methods the client uses.
func fakeCentrifugo(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var published []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params)
		switch strings.TrimPrefix(r.URL.Path, "/api/") {
		case "info":
			w.Write([]byte(`{"result":{"nodes":[{"name":"fake"}]}}`))
		case "publish":
			if params["channel"] == "forbidden:x" {
				w.Write([]byte(`{"error":{"code":103,"message":"permission denied"}}`))
				return
			}
			published = append(published, params)
			w.Write([]byte(`{"result":{}}`))
		case "history":
			pubs := []map[string]any{}
			for _, p := range published {
				if p["channel"] == params["channel"] {
					pubs = append(pubs, map[string]any{"data": p["data"]})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"publications": pubs}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &published
}

func TestPublishHistoryAndErrors(t *testing.T) {
	zlogtest.Discard(t)
	srv, published := fakeCentrifugo(t)
	c, err := Open(context.Background(), Target{APIAddr: srv.URL, APIKey: "key", TokenSecret: "s"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), "user:42", map[string]any{"type": "member.updated", "uid": 42}); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 1 || (*published)[0]["channel"] != "user:42" {
		t.Errorf("published: %v", *published)
	}
	hist, err := c.History(context.Background(), "user:42", 10)
	if err != nil || len(hist) != 1 || !strings.Contains(string(hist[0]), "member.updated") {
		t.Errorf("history: %v %v", hist, err)
	}
	if err := c.Publish(context.Background(), "forbidden:x", nil); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("an API error is returned as it is: %v", err)
	}
	if _, err := Open(context.Background(), Target{APIAddr: srv.URL, APIKey: "wrong"}, Config{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("a wrong key fails Open with the hint: %v", err)
	}
}

func TestConnectionToken(t *testing.T) {
	zlogtest.Discard(t)
	srv, _ := fakeCentrifugo(t)
	c, err := Open(context.Background(), Target{APIAddr: srv.URL, APIKey: "key", TokenSecret: "secret"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.ConnectionToken("42")
	if err != nil {
		t.Fatal(err)
	}
	claims := connectionClaims{}
	parsed, err := jwt.ParseWithClaims(tok, &claims, func(*jwt.Token) (any, error) { return []byte("secret"), nil })
	if err != nil || !parsed.Valid || claims.Subject != "42" || claims.ExpiresAt == nil {
		t.Errorf("token: %v %+v", err, claims)
	}
	if len(claims.Channels) != 1 || claims.Channels[0] != "user:42" || c.UserChannel("42") != "user:42" {
		t.Errorf("channels: %v", claims.Channels)
	}
	tok2, _ := c.ConnectionToken("42", "odds:live")
	claims = connectionClaims{}
	if _, err := jwt.ParseWithClaims(tok2, &claims, func(*jwt.Token) (any, error) { return []byte("secret"), nil }); err != nil || len(claims.Channels) != 2 {
		t.Errorf("extra channels: %v %v", err, claims.Channels)
	}
	none, _ := Open(context.Background(), Target{APIAddr: srv.URL, APIKey: "key"}, Config{})
	if _, err := none.ConnectionToken("42"); err == nil {
		t.Error("without a secret no token")
	}
}
