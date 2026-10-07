package httpx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Auth is the `auth:` entry of a provider: how every request proves who we
// are. The common schemes are configuration; a vendor's own scheme is a
// Signer the integration code sets with Client.UseSigner.
type Auth struct {
	// Type: "" (none; static headers only), "bearer", "basic",
	// "hmac-sha256", "oauth2" (client credentials), "custom" (UseSigner).
	Type string `yaml:"type"`

	// bearer: Authorization: Bearer <Token>.
	Token string `yaml:"token"`

	// basic: Authorization: Basic base64(Username:Password).
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// hmac-sha256: Header = encode(HMAC-SHA256(Secret, Payload)), where
	// Payload is a template with {method} {path} {query} {timestamp} {body}
	// and \n for a newline. Default "{method}\n{path}\n{timestamp}\n{body}".
	// The timestamp (unix seconds) is also sent in TimestampHeader so the
	// vendor can verify. Encoding "hex" (default) or "base64".
	Secret          string `yaml:"secret"`
	Header          string `yaml:"header"`           // default X-Signature
	TimestampHeader string `yaml:"timestamp_header"` // default X-Timestamp; "" with no {timestamp} in Payload sends none
	Payload         string `yaml:"payload"`
	Encoding        string `yaml:"encoding"`

	// oauth2: the client-credentials flow. The token is fetched from
	// TokenURL with ClientID / ClientSecret, cached, and refreshed before
	// it expires; the request gets Authorization: Bearer <token>.
	TokenURL     string   `yaml:"token_url"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
}

// Signer signs one request a vendor's own way: it reads the method, URL,
// headers and body (already read for it; the request's Body is intact) and
// sets whatever headers the vendor wants. It runs on every attempt, so a
// timestamp is fresh on a retry. Secrets come from the environment through
// the integration's own configuration, never from code.
type Signer interface {
	Sign(req *http.Request, body []byte) error
}

// SignerFunc adapts a function to Signer.
type SignerFunc func(req *http.Request, body []byte) error

func (f SignerFunc) Sign(req *http.Request, body []byte) error { return f(req, body) }

// ErrNoSigner: auth.type is custom and no Signer was set with UseSigner.
var ErrNoSigner = errors.New("httpx: auth.type custom but no signer set; call rt.Provider(name).UseSigner(...) in app.Setup")

const defaultHMACPayload = "{method}\n{path}\n{timestamp}\n{body}"

// Validate checks the auth entry for the fields its type needs.
func (a Auth) Validate(name string) error {
	need := func(fields ...[2]string) error {
		for _, f := range fields {
			if strings.TrimSpace(f[1]) == "" {
				return fmt.Errorf("httpx: providers.%s.auth.%s is required for type %s (is its environment variable set?)", name, f[0], a.Type)
			}
		}
		return nil
	}
	switch a.Type {
	case "", "custom":
		return nil
	case "bearer":
		return need([2]string{"token", a.Token})
	case "basic":
		return need([2]string{"username", a.Username}, [2]string{"password", a.Password})
	case "hmac-sha256":
		if a.Encoding != "" && a.Encoding != "hex" && a.Encoding != "base64" {
			return fmt.Errorf("httpx: providers.%s.auth.encoding %q: hex or base64", name, a.Encoding)
		}
		return need([2]string{"secret", a.Secret})
	case "oauth2":
		return need([2]string{"token_url", a.TokenURL}, [2]string{"client_id", a.ClientID}, [2]string{"client_secret", a.ClientSecret})
	}
	return fmt.Errorf("httpx: providers.%s.auth.type %q: one of bearer, basic, hmac-sha256, oauth2, custom", name, a.Type)
}

// authTransport is the RoundTripper that signs each attempt. It sits
// between resty and the guard, so raw R() calls and retries are signed too.
type authTransport struct {
	name   string
	auth   Auth
	next   http.RoundTripper
	signer atomic.Pointer[Signer]
	oauth  oauth2.TokenSource
}

func newAuthTransport(name string, a Auth, next http.RoundTripper, pool *http.Transport) *authTransport {
	t := &authTransport{name: name, auth: a, next: next}
	if a.Type == "oauth2" {
		cfg := clientcredentials.Config{TokenURL: a.TokenURL, ClientID: a.ClientID, ClientSecret: a.ClientSecret, Scopes: a.Scopes}
		// The token endpoint is called with our pooled transport, not the
		// default client; ReuseTokenSource caches and refreshes.
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: pool, Timeout: 10 * time.Second})
		t.oauth = oauth2.ReuseTokenSource(nil, cfg.TokenSource(ctx))
	}
	return t
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch t.auth.Type {
	case "":
		return t.next.RoundTrip(req)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+t.auth.Token)
	case "basic":
		req.SetBasicAuth(t.auth.Username, t.auth.Password)
	case "oauth2":
		tok, err := t.oauth.Token()
		if err != nil {
			return nil, fmt.Errorf("httpx: %s: oauth2 token: %w", t.name, err)
		}
		tok.SetAuthHeader(req)
	case "hmac-sha256":
		body, err := readBody(req)
		if err != nil {
			return nil, err
		}
		t.signHMAC(req, body)
	case "custom":
		s := t.signer.Load()
		if s == nil {
			return nil, ErrNoSigner
		}
		body, err := readBody(req)
		if err != nil {
			return nil, err
		}
		if err := (*s).Sign(req, body); err != nil {
			return nil, fmt.Errorf("httpx: %s: sign: %w", t.name, err)
		}
	}
	return t.next.RoundTrip(req)
}

func (t *authTransport) signHMAC(req *http.Request, body []byte) {
	a := t.auth
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := a.Payload
	if payload == "" {
		payload = defaultHMACPayload
	}
	payload = strings.NewReplacer(
		`\n`, "\n",
		"{method}", req.Method,
		"{path}", req.URL.Path,
		"{query}", req.URL.RawQuery,
		"{timestamp}", ts,
		"{body}", string(body),
	).Replace(payload)
	mac := hmac.New(sha256.New, []byte(a.Secret))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	if a.Encoding == "base64" {
		sig = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	header := a.Header
	if header == "" {
		header = "X-Signature"
	}
	req.Header.Set(header, sig)
	tsHeader := a.TimestampHeader
	if tsHeader == "" && strings.Contains(payloadTemplate(a), "{timestamp}") {
		tsHeader = "X-Timestamp"
	}
	if tsHeader != "" {
		req.Header.Set(tsHeader, ts)
	}
}

func payloadTemplate(a Auth) string {
	if a.Payload == "" {
		return defaultHMACPayload
	}
	return a.Payload
}

// readBody returns the request body and leaves the request able to send
// (and resend) it.
func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("httpx: read request body for signing: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	req.ContentLength = int64(len(b))
	return b, nil
}

// UseSigner installs the vendor-specific Signer of a provider whose
// auth.type is custom. Call it in app.Setup; a call before it fails with
// ErrNoSigner.
func (c *Client) UseSigner(s Signer) *Client {
	c.auth.signer.Store(&s)
	return c
}
