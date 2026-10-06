// Package centrifugox is the push server (Centrifugo) for a service, opened
// by the framework from the `centrifugo:` section: Publish sends a message
// to a channel through Centrifugo's HTTP API (a span of the request's trace),
// ConnectionToken signs the JWT a client connects with. kitexx opens it and
// hands it over as Runtime.Centrifugo.
package centrifugox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the `centrifugo:` section of a service configuration.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Source  string `yaml:"source"` // static (default) or platform (infra.yaml in Nacos)

	// APIAddr "http://host:9000", APIKey (the HTTP API key) and TokenSecret
	// (the HMAC secret client tokens are signed with): the server, with
	// source static.
	APIAddr     string `yaml:"api_addr"`
	APIKey      string `yaml:"api_key"`
	TokenSecret string `yaml:"token_secret"`
	// TokenTTL is how long a connection token is valid. Default 24h.
	TokenTTL config.Duration `yaml:"token_ttl"`
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"

	defaultTokenTTL = 24 * time.Hour
	tracerName      = "github.com/sezznaw/devkit-common/centrifugox"
)

func (c Config) SourceName() string {
	if s := strings.TrimSpace(c.Source); s != "" {
		return s
	}
	return SourceStatic
}

// Validate rejects a configuration that cannot work, naming the setting.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.SourceName() {
	case SourceStatic:
		if strings.TrimSpace(c.APIAddr) == "" {
			return fmt.Errorf("centrifugox: centrifugo.api_addr is required with source static")
		}
	case SourcePlatform:
	default:
		return fmt.Errorf("centrifugox: centrifugo.source is %q; it must be %s or %s", c.Source, SourceStatic, SourcePlatform)
	}
	return nil
}

// Target is a server to talk to, once resolved.
type Target struct {
	APIAddr     string
	APIKey      string
	TokenSecret string
}

// Static is the target of a static configuration.
func (c Config) Static() Target {
	return Target{APIAddr: strings.TrimRight(strings.TrimSpace(c.APIAddr), "/"), APIKey: c.APIKey, TokenSecret: c.TokenSecret}
}

// Client talks to one Centrifugo.
type Client struct {
	api    string
	key    string
	secret []byte
	ttl    time.Duration
	http   *http.Client
}

// Open checks the server answers (the info call) and returns the client.
func Open(ctx context.Context, t Target, cfg Config) (*Client, error) {
	if t.APIAddr == "" || t.APIKey == "" {
		return nil, fmt.Errorf("centrifugox: api_addr and api_key are required")
	}
	c := &Client{
		api:    strings.TrimRight(t.APIAddr, "/"),
		key:    t.APIKey,
		secret: []byte(t.TokenSecret),
		ttl:    cfg.TokenTTL.Or(defaultTokenTTL),
		http:   &http.Client{Timeout: 10 * time.Second},
	}
	var info json.RawMessage
	if err := c.call(ctx, "info", map[string]any{}, &info); err != nil {
		return nil, fmt.Errorf("centrifugox: %s: %w (is Centrifugo up, is the API key right?)", t.APIAddr, err)
	}
	return c, nil
}

// Publish sends data (JSON) to a channel: what a subscriber of that channel
// receives. Channels are "<namespace>:<name>", e.g. "user:42".
func (c *Client) Publish(ctx context.Context, channel string, data any) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "centrifugo.publish",
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("messaging.destination.name", channel)))
	defer span.End()
	var result json.RawMessage
	if err := c.call(ctx, "publish", map[string]any{"channel": channel, "data": data}, &result); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		zlog.Ctx(ctx).Error("push not published", zlog.Str("channel", channel), zlog.Err(err))
		return err
	}
	zlog.Ctx(ctx).Info("push published", zlog.Str("channel", channel))
	return nil
}

// History returns the last messages of a channel (the namespace must keep
// history); handy for tests and for a client that reconnects.
func (c *Client) History(ctx context.Context, channel string, limit int) ([]json.RawMessage, error) {
	var result struct {
		Publications []struct {
			Data json.RawMessage `json:"data"`
		} `json:"publications"`
	}
	if err := c.call(ctx, "history", map[string]any{"channel": channel, "limit": limit}, &result); err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(result.Publications))
	for _, p := range result.Publications {
		out = append(out, p.Data)
	}
	return out, nil
}

// ConnectionToken signs the JWT a client presents when it connects: its
// subject is the user id, so Centrifugo knows who it is and the service can
// publish to "user:<id>". The gateway hands it to a logged-in client.
func (c *Client) ConnectionToken(userID string) (string, error) {
	if len(c.secret) == 0 {
		return "", fmt.Errorf("centrifugox: token_secret is not set; connection tokens cannot be signed")
	}
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(c.ttl)),
	})
	return tok.SignedString(c.secret)
}

// apiError is Centrifugo's error object.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("centrifugo error %d: %s", e.Code, e.Message) }

// call POSTs one API method and decodes its result.
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.key)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("centrifugo %s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var envelope struct {
		Error  *apiError       `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("centrifugo %s: %w: %s", method, err, strings.TrimSpace(string(raw)))
	}
	if envelope.Error != nil && envelope.Error.Code != 0 {
		return envelope.Error
	}
	if result != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
