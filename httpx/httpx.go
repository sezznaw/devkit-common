// Package httpx is how a service calls an external HTTP API (a sports data
// vendor, a payment gateway): resty underneath, and around it what every
// such call must have and no colleague should write twice: a base URL and
// headers from the configuration (secrets from the environment), a timeout,
// retries only where they are safe, the trace of the request carried on,
// one log record per call with the trace_id and nothing secret in it, and
// metrics. A service lists its providers under `providers:`; the Runtime
// opens them; `rt.Provider("odds-feed")` is the client.
package httpx

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Provider is one entry of the `providers:` section: an external API this
// service calls.
type Provider struct {
	// BaseURL is the scheme and host (and a path prefix if the vendor has
	// one): "https://api.vendor.com". Required.
	BaseURL string `yaml:"base_url"`
	// Timeout for one attempt, connect to last byte. Default 5s.
	Timeout config.Duration `yaml:"timeout"`
	// Retries after a failed attempt. Default 2. Only network errors, 429
	// and 5xx are retried, and only idempotent requests (GET, HEAD, OPTIONS,
	// PUT, DELETE): a POST is never sent twice by the framework, because
	// "pay out 100" twice is two payouts. Retries wait 200ms, 400ms, ...
	Retries *int `yaml:"retries"`
	// Headers sent with every request: the vendor's API key, usually
	// "X-Api-Key: ${ODDS_FEED_API_KEY}", so the secret is in the environment.
	Headers map[string]string `yaml:"headers"`
	// Insecure skips TLS verification. Only for a vendor's sandbox with a
	// self-signed certificate.
	Insecure bool `yaml:"insecure"`
	// MaxConcurrent calls in flight to this provider; further calls wait
	// (within their timeout) for a slot. Default 64. 0 means no limit.
	MaxConcurrent *int `yaml:"max_concurrent"`
	// BreakerFailures: after this many consecutive failed attempts (network
	// error, 429, 5xx) the circuit opens and calls fail at once with
	// ErrCircuitOpen for breaker_open_for, then one probe is let through.
	// Default 5. 0 disables the breaker.
	BreakerFailures *int `yaml:"breaker_failures"`
	// BreakerOpenFor is how long the circuit stays open. Default 30s.
	BreakerOpenFor config.Duration `yaml:"breaker_open_for"`
	// Auth is how requests are authenticated: bearer, basic, hmac-sha256,
	// oauth2 (client credentials) from configuration, or custom with a
	// Signer the integration sets (UseSigner). Secrets are ${ENV} values.
	Auth Auth `yaml:"auth"`
	// Callback is how this provider calls us back (payment results, event
	// settlements): how to verify the call, which field is the event id.
	// webhookx.Handle uses it.
	Callback Callback `yaml:"callback"`
	// Debug prints every request and response, headers and bodies, to the
	// log at debug level. Honoured on a developer's machine only
	// (APP_ENV local): in a deployment it is ignored with a warning,
	// because bodies carry secrets and personal data.
	Debug bool `yaml:"debug"`
}

// The worst case of one call is (retries + 1) attempts, each up to
// timeout, plus the waits between them (200ms, 400ms, ... up to 2s): with
// the defaults about 16s. A handler that cannot wait that long gives its
// ctx a deadline; the call stops when the ctx does.

const (
	defaultTimeout         = 5 * time.Second
	defaultRetries         = 2
	defaultMaxConcurrent   = 64
	defaultBreakerFailures = 5
	defaultBreakerOpenFor  = 30 * time.Second
)

func (p Provider) maxConcurrent() int {
	if p.MaxConcurrent == nil {
		return defaultMaxConcurrent
	}
	return max(0, *p.MaxConcurrent)
}

func (p Provider) breakerFailures() int {
	if p.BreakerFailures == nil {
		return defaultBreakerFailures
	}
	return max(0, *p.BreakerFailures)
}

func (p Provider) retries() int {
	if p.Retries == nil {
		return defaultRetries
	}
	return max(0, *p.Retries)
}

// Validate checks one provider's entry.
func (p Provider) Validate(name string) error {
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("httpx: providers.%s.base_url %q: must be http(s)://host[/prefix]", name, p.BaseURL)
	}
	for k, v := range p.Headers {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("httpx: providers.%s.headers.%s is empty (is its environment variable set?)", name, k)
		}
	}
	if err := p.Auth.Validate(name); err != nil {
		return err
	}
	return p.Callback.Validate(name)
}

// Client is the client of one provider. It is a *resty.Client: use R() for
// anything resty can do, or the two helpers for the common case.
type Client struct {
	*resty.Client
	Name string
	base *url.URL
	auth *authTransport
}

// New builds the client of one provider. Every call through it gets: the
// timeout, the retries (see Provider.Retries), a client span with the
// trace carried in the request (W3C traceparent), one log record with the
// trace_id (method, path without the query string, status, latency,
// attempt; never headers or bodies), and the http_client_* metrics.
func New(name string, p Provider) (*Client, error) {
	if err := p.Validate(name); err != nil {
		return nil, err
	}
	base, _ := url.Parse(p.BaseURL)
	// resty -> auth (sign each attempt) -> guard (concurrency, breaker) -> otelhttp (span) -> pooled transport
	pool := newTransport(p)
	traced := otelhttp.NewTransport(pool,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + name + " " + r.URL.Path }),
		otelhttp.WithSpanOptions(trace.WithAttributes(attribute.String("peer.service", name))))
	auth := newAuthTransport(name, p.Auth, newGuard(name, p, traced), pool)
	rc := resty.New().
		SetTransport(auth).
		SetBaseURL(p.BaseURL).
		SetHeaders(p.Headers).
		SetTimeout(p.Timeout.Or(defaultTimeout)).
		SetRetryCount(p.retries()).
		SetRetryWaitTime(200 * time.Millisecond).
		SetRetryMaxWaitTime(2 * time.Second).
		SetRetryResetReaders(true).
		SetLogger(restyLogger{name}).
		AddRetryCondition(retryCondition)
	c := &Client{Client: rc, Name: name, base: base, auth: auth}
	rc.OnAfterResponse(c.afterResponse)
	rc.OnError(c.onError)
	if p.Debug {
		if config.IsLocal() {
			rc.SetDebug(true)
			zlog.Warn("provider debug on: requests and responses, headers and bodies, are logged", zlog.Str("provider", name))
		} else {
			zlog.Warn("providers."+name+".debug is ignored outside the local environment: bodies carry secrets", zlog.Str("provider", name))
		}
	}
	return c, nil
}

// retryCondition: a network error, 429 or 5xx, and only for an idempotent
// method. resty's own default conditions are not used (they would retry a
// POST on a network error).
func retryCondition(r *resty.Response, err error) bool {
	if errors.Is(err, ErrCircuitOpen) || errors.Is(err, ErrTooManyInFlight) {
		return false // the breaker said no; asking again at once is pointless
	}
	if r == nil || r.Request == nil || r.Request.RawRequest == nil {
		return err != nil
	}
	switch r.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
	default:
		return false
	}
	if err != nil {
		return true
	}
	return r.StatusCode() == http.StatusTooManyRequests || r.StatusCode() >= 500
}

func (c *Client) afterResponse(_ *resty.Client, r *resty.Response) error {
	c.record(r.Request, r.StatusCode(), r.Time(), nil)
	return nil
}

func (c *Client) onError(req *resty.Request, err error) {
	var status int
	var took time.Duration
	if re, ok := err.(*resty.ResponseError); ok && re.Response != nil {
		status, took = re.Response.StatusCode(), re.Response.Time()
	}
	c.record(req, status, took, err)
}

// record is the log record and the metrics of one call (the final
// attempt; retries are visible as attempt > 1).
func (c *Client) record(req *resty.Request, status int, took time.Duration, err error) {
	path := req.URL
	if req.RawRequest != nil && req.RawRequest.URL != nil {
		path = req.RawRequest.URL.Path
	}
	statusLabel := "error"
	if status > 0 {
		statusLabel = strconv.Itoa(status)
	}
	metricsx.HTTPClientRequests.WithLabelValues(c.Name, req.Method, statusLabel).Inc()
	metricsx.HTTPClientDuration.WithLabelValues(c.Name, req.Method).Observe(took.Seconds())
	fields := []zlog.Field{zlog.Str("provider", c.Name), zlog.Str("method", req.Method), zlog.Str("path", path),
		zlog.Int("status", status), zlog.Dur("latency", took), zlog.Int("attempt", req.Attempt)}
	log := zlog.Ctx(req.Context())
	switch {
	case err != nil:
		log.Error("http call failed", append(fields, zlog.Err(err))...)
	case status >= 500:
		log.Error("http call", fields...)
	case status >= 400:
		log.Warn("http call", fields...)
	default:
		log.Info("http call", fields...)
	}
}

// GetJSON does GET path and decodes a 2xx JSON body into out. A non-2xx
// status is a *StatusError with the status and the first bytes of the body.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// PostJSON does POST path with body as JSON and decodes a 2xx JSON body
// into out (nil to ignore it). Never retried by the framework.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	r := c.R().SetContext(ctx).SetHeader("Accept", "application/json")
	if body != nil {
		r.SetBody(body).SetHeader("Content-Type", "application/json")
	}
	resp, err := r.Execute(method, path)
	if err != nil {
		return fmt.Errorf("httpx: %s %s %s: %w", c.Name, method, path, err)
	}
	if resp.IsError() {
		return &StatusError{Provider: c.Name, Method: method, Path: path, Status: resp.StatusCode(), Body: clip(resp.String(), 512)}
	}
	// Decoded here, not by resty's SetResult, which only decodes when the
	// vendor sets a JSON content type; many do not.
	if out != nil && len(resp.Body()) > 0 {
		if err := json.Unmarshal(resp.Body(), out); err != nil {
			return fmt.Errorf("httpx: %s %s %s: decode %T: %w", c.Name, method, path, out, err)
		}
	}
	return nil
}

// StatusError: the provider answered, with a status that is not 2xx.
type StatusError struct {
	Provider, Method, Path string
	Status                 int
	Body                   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpx: %s %s %s: status %d: %s", e.Provider, e.Method, e.Path, e.Status, e.Body)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// restyLogger routes what resty itself says to zlog, under the provider.
type restyLogger struct{ name string }

func (l restyLogger) Errorf(f string, v ...any) { zlog.Errorf("resty["+l.name+"]: "+f, v...) }
func (l restyLogger) Warnf(f string, v ...any)  { zlog.Warnf("resty["+l.name+"]: "+f, v...) }
func (l restyLogger) Debugf(f string, v ...any) { zlog.Debugf("resty["+l.name+"]: "+f, v...) }

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } //nolint:gosec // opted in per provider, sandboxes only
