// Package webhookx receives the callbacks a provider sends us (a payment
// result, a settled event) on a Hertz service in the provider namespace.
// The integration writes the handler; the framework does what every
// callback needs: a body limit, the source-address check, the signature
// check, the raw copy to Kafka for disputes, "seen this event id already"
// through Redis, one log record and the metrics, and the answer the
// provider expects: 2xx when we have it, 5xx when we do not (so it
// retries), 4xx when it is not the provider or not a valid call.
package webhookx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Event is one callback, verified and not seen before.
type Event struct {
	Provider string
	// ID is the provider's id of the notification (callback.event_id), ""
	// when the provider has none configured.
	ID      string
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
	// ClientIP is the address the callback came from, as the listener's
	// trusted-proxy rule sees it (the same value allow_cidrs judges and the
	// archive records as "client"). A custom Verifier reads it when the
	// allowed sources are data, not configuration: a payment provider's
	// whitelist kept in the back office (v0.80.0).
	ClientIP string
}

// Decode unmarshals the JSON body into v.
func (e *Event) Decode(v any) error {
	if err := json.Unmarshal(e.Body, v); err != nil {
		return fmt.Errorf("webhookx: %s: decode %T: %w", e.Provider, v, err)
	}
	return nil
}

// Handler handles one callback. A nil return answers the provider with
// the route's reply (200 "ok" unless WithReply says otherwise); an error
// answers 500, which makes the provider retry, and forgets the event id
// so the retry runs the handler again. Make it idempotent anyway.
type Handler func(ctx context.Context, ev *Event) error

// Verifier is a vendor-specific check for callback.verify.type custom:
// return an error when the request is not from the provider (401), and
// ErrUnavailable when that cannot be told right now (503).
type Verifier func(req *Event) error

// Option configures one route.
type Option func(*route)

// WithVerifier sets the Verifier of a route whose provider has
// callback.verify.type custom.
func WithVerifier(v Verifier) Option { return func(r *route) { r.verifier = v } }

// WithReply sets the status and body the provider gets on success, for
// providers that want more than 200 "ok" ({"code":"SUCCESS"}).
func WithReply(status int, contentType, body string) Option {
	return func(r *route) { r.replyStatus, r.replyType, r.replyBody = status, contentType, body }
}

// ErrNotFromProvider is what a failed verification is wrapped in.
var ErrNotFromProvider = errors.New("webhookx: request is not from the provider")

// ErrUnavailable is what a custom Verifier returns (as it is, or wrapped
// with %w) when it cannot judge the request: the data it judges by, the
// provider's secret or its allowed sources, is not readable right now. That
// is our failure, not the caller's, and is answered as one: 503, which a
// provider retries, where every other error of a Verifier is the 401 of
// ErrNotFromProvider, which many providers stop on. The request is counted
// as verify_unavailable, not bad_signature (so that an alert on refused
// callbacks means what it says), and since it was not verified it is
// neither archived nor handled.
//
// ErrUnavailable：custom Verifier 判断不了这个请求时返回它（原样或用 %w 包一层）——它依据的
// 数据（厂商密钥、允许的来源）此刻读不到。这是我们自己的故障，不是对方的：回 503 让厂商重试，
// 而不是「不是这家厂商发的」那种 401；计入 verify_unavailable，不计 bad_signature；没验过的
// 请求不留档、不进 handler。
var ErrUnavailable = errors.New("webhookx: verifier unavailable")

type route struct {
	provider    string
	path        string
	cfg         httpx.Callback
	handler     Handler
	verifier    Verifier
	redis       *redis.Client
	kafka       *kafkax.Client
	replyStatus int
	replyType   string
	replyBody   string
	cidrs       []*net.IPNet
}

// results are the values of the result label of callbacks_received_total,
// one per way serve can end.
var results = []string{"ok", "duplicate", "bad_source", "bad_signature", "verify_unavailable", "too_large", "archive_failed", "dedupe_unavailable", "handler_error"}

const (
	defaultDedupeTTL = 7 * 24 * time.Hour
	defaultMaxBody   = 1 << 20
	defaultMaxSkew   = 5 * time.Minute
)

// Handle registers path on h as the callback endpoint of provider: POST
// (and PUT) requests go through the checks and then to handler. The
// provider must be listed under providers: with a callback: entry; the
// service runs in the provider namespace (NewRuntime checks that). Redis
// (redis.enabled) is needed for the event-id check and Kafka
// (kafka.enabled) for the raw copy; without them Handle logs what is
// missing and goes on without that part.
func Handle(rt *kitexx.Runtime, h *server.Hertz, provider, path string, handler Handler, opts ...Option) {
	p, ok := rt.Config.Providers[provider]
	if !ok {
		panic(fmt.Sprintf("webhookx: no provider %q in the configuration; add providers.%s with a callback: entry", provider, provider))
	}
	if !p.Callback.Configured() {
		panic(fmt.Sprintf("webhookx: providers.%s has no callback: entry (verify, event_id)", provider))
	}
	r := &route{provider: provider, path: path, cfg: p.Callback, handler: handler, redis: rt.Redis, kafka: rt.Kafka, replyStatus: 200, replyType: "text/plain", replyBody: "ok"}
	for _, o := range opts {
		o(r)
	}
	if r.cfg.Verify.Type == "custom" && r.verifier == nil {
		panic(fmt.Sprintf("webhookx: providers.%s.callback.verify.type is custom: pass webhookx.WithVerifier", provider))
	}
	// The listener's limit comes first (the body is refused before it is
	// read): a route that asks for more would never get it.
	if limit := int64(rt.Config.Callbacks.MaxBodyBytes); limit > 0 && r.cfg.MaxBody > limit {
		panic(fmt.Sprintf("webhookx: providers.%s.callback.max_body is %d, more than callbacks.max_body_bytes (%d): the listener refuses such a body before the route sees it; raise callbacks.max_body_bytes or lower max_body", provider, r.cfg.MaxBody, limit))
	}
	for _, c := range r.cfg.AllowCIDRs {
		_, n, _ := net.ParseCIDR(c)
		r.cidrs = append(r.cidrs, n)
	}
	// Every result series exists from the start, at 0: an alert on
	// increase(callbacks_received_total{result="bad_signature"}) needs a
	// sample before the first refusal, or it never sees that one.
	for _, res := range results {
		metricsx.CallbacksReceived.WithLabelValues(provider, path, res).Add(0)
	}
	log := zlog.With(zlog.Str("provider", provider), zlog.Str("path", path))
	if r.cfg.EventID.Header == "" && r.cfg.EventID.JSON == "" {
		log.Warn("callback has no event_id: a redelivered callback runs the handler again; make it idempotent")
	} else if r.redis == nil {
		log.Warn("callback dedupe needs redis.enabled; without it a redelivered callback runs the handler again")
	}
	if r.kafka == nil && r.archiveTopic() != "" {
		log.Warn("callback archive needs kafka.enabled; raw callbacks are not kept")
	}
	h.POST(path, r.serve)
	h.PUT(path, r.serve)
	log.Info("callback route registered", zlog.Str("verify", r.cfg.Verify.Type), zlog.Int("allow_cidrs", len(r.cidrs)), zlog.Str("archive_topic", r.archiveTopic()))
}

func (r *route) archiveTopic() string {
	switch r.cfg.ArchiveTopic {
	case "-":
		return ""
	case "":
		return "callbacks." + r.provider
	}
	return r.cfg.ArchiveTopic
}

func (r *route) serve(ctx context.Context, c *app.RequestContext) {
	log := zlog.Ctx(ctx).With(zlog.Str("provider", r.provider))
	result := func(res string, status int) {
		metricsx.CallbacksReceived.WithLabelValues(r.provider, r.path, res).Inc()
		c.SetStatusCode(status)
	}
	// 1. source address
	if len(r.cidrs) > 0 {
		ip := net.ParseIP(c.ClientIP())
		if ip == nil || !inAny(ip, r.cidrs) {
			log.Warn("callback rejected: source address not allowed", zlog.Str("client", c.ClientIP()))
			result("bad_source", http.StatusForbidden)
			return
		}
	}
	// 2. body limit
	max := r.cfg.MaxBody
	if max <= 0 {
		max = defaultMaxBody
	}
	body := c.Request.Body()
	if int64(len(body)) > max {
		log.Warn("callback rejected: body too large", zlog.Int("bytes", len(body)))
		result("too_large", http.StatusRequestEntityTooLarge)
		return
	}
	ev := &Event{Provider: r.provider, Method: string(c.Method()), Path: string(c.Path()), Headers: http.Header{}, Body: body, ClientIP: c.ClientIP()}
	c.Request.Header.VisitAll(func(k, v []byte) { ev.Headers.Add(string(k), string(v)) })
	// 3. signature
	if err := r.verify(ev, string(c.Request.QueryString())); err != nil {
		if errors.Is(err, ErrUnavailable) {
			log.Error("callback not verified: verifier unavailable; answering 503 so the provider retries", zlog.Err(err))
			result("verify_unavailable", http.StatusServiceUnavailable)
			return
		}
		log.Warn("callback rejected: verification failed", zlog.Err(err))
		result("bad_signature", http.StatusUnauthorized)
		return
	}
	// 4. event id
	ev.ID = r.eventID(ev)
	log = log.With(zlog.Str("event_id", ev.ID))
	ctx = zlog.CtxWith(ctx, zlog.Str("provider", r.provider), zlog.Str("event_id", ev.ID))
	// 5. raw copy, before anything can go wrong in our code
	if topic := r.archiveTopic(); topic != "" && r.kafka != nil {
		if err := r.kafka.Publish(ctx, topic, firstNonEmpty(ev.ID, r.provider), "callback.received", map[string]any{
			"path": ev.Path, "headers": ev.Headers, "body": json.RawMessage(rawOrString(body)), "client": c.ClientIP(),
		}); err != nil {
			log.Error("callback not archived; answering 500 so the provider retries", zlog.Err(err))
			result("archive_failed", http.StatusInternalServerError)
			return
		}
	}
	// 6. seen before?
	var key string
	if ev.ID != "" && r.redis != nil {
		key = "cb:" + r.provider + ":" + ev.ID
		ok, err := r.redis.SetNX(ctx, key, time.Now().UTC().Format(time.RFC3339), r.cfg.DedupeTTL.Or(defaultDedupeTTL)).Result()
		if err != nil {
			log.Error("callback dedupe store unavailable; answering 500 so the provider retries", zlog.Err(err))
			result("dedupe_unavailable", http.StatusInternalServerError)
			return
		}
		if !ok {
			log.Info("callback already handled; acknowledged again")
			r.reply(c)
			metricsx.CallbacksReceived.WithLabelValues(r.provider, r.path, "duplicate").Inc()
			return
		}
	}
	// 7. the handler
	start := time.Now()
	if err := r.run(ctx, ev); err != nil {
		if key != "" {
			r.redis.Del(context.WithoutCancel(ctx), key)
		}
		log.Error("callback handler failed; answering 500 so the provider retries", zlog.Err(err), zlog.Dur("latency", time.Since(start)))
		result("handler_error", http.StatusInternalServerError)
		return
	}
	log.Info("callback handled", zlog.Dur("latency", time.Since(start)))
	metricsx.CallbacksReceived.WithLabelValues(r.provider, r.path, "ok").Inc()
	r.reply(c)
}

func (r *route) run(ctx context.Context, ev *Event) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return r.handler(ctx, ev)
}

func (r *route) reply(c *app.RequestContext) {
	c.Data(r.replyStatus, r.replyType, []byte(r.replyBody))
}

func (r *route) verify(ev *Event, query string) error {
	v := r.cfg.Verify
	switch v.Type {
	case "":
		return nil
	case "custom":
		if err := r.verifier(ev); err != nil {
			if errors.Is(err, ErrUnavailable) {
				return err // not a verdict on the caller: serve answers 503
			}
			return fmt.Errorf("%w: %v", ErrNotFromProvider, err)
		}
		return nil
	case "basic":
		u, p, ok := parseBasic(ev.Headers.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(v.Username)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(v.Password)) != 1 {
			return fmt.Errorf("%w: basic credentials", ErrNotFromProvider)
		}
		return nil
	case "hmac-sha256":
		header := v.Header
		if header == "" {
			header = "X-Signature"
		}
		got := strings.TrimSpace(ev.Headers.Get(header))
		if got == "" {
			return fmt.Errorf("%w: no %s header", ErrNotFromProvider, header)
		}
		payload := v.Payload
		if payload == "" {
			payload = "{body}"
		}
		ts := ""
		if strings.Contains(payload, "{timestamp}") {
			tsHeader := v.TimestampHeader
			if tsHeader == "" {
				tsHeader = "X-Timestamp"
			}
			ts = strings.TrimSpace(ev.Headers.Get(tsHeader))
			sec, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				return fmt.Errorf("%w: %s is not a unix timestamp", ErrNotFromProvider, tsHeader)
			}
			if skew := time.Since(time.Unix(sec, 0)).Abs(); skew > v.MaxSkew.Or(defaultMaxSkew) {
				return fmt.Errorf("%w: timestamp off by %s", ErrNotFromProvider, skew.Round(time.Second))
			}
		}
		payload = strings.NewReplacer(`\n`, "\n", "{method}", ev.Method, "{path}", ev.Path, "{query}", query, "{timestamp}", ts, "{body}", string(ev.Body)).Replace(payload)
		mac := hmac.New(sha256.New, []byte(v.Secret))
		mac.Write([]byte(payload))
		sum := mac.Sum(nil)
		want := hex.EncodeToString(sum)
		if v.Encoding == "base64" {
			want = base64.StdEncoding.EncodeToString(sum)
		}
		// Some vendors prefix the scheme ("sha256=..."); compare the tail.
		if i := strings.LastIndexByte(got, '='); i >= 0 && v.Encoding != "base64" {
			got = got[i+1:]
		}
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(strings.ToLower(want))) != 1 {
			return fmt.Errorf("%w: signature mismatch", ErrNotFromProvider)
		}
		return nil
	}
	return fmt.Errorf("webhookx: unknown verify type %q", v.Type)
}

func (r *route) eventID(ev *Event) string {
	switch {
	case r.cfg.EventID.Header != "":
		return strings.TrimSpace(ev.Headers.Get(r.cfg.EventID.Header))
	case r.cfg.EventID.JSON != "":
		var doc any
		if json.Unmarshal(ev.Body, &doc) != nil {
			return ""
		}
		cur := doc
		for _, part := range strings.Split(r.cfg.EventID.JSON, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur = m[part]
		}
		switch v := cur.(type) {
		case string:
			return v
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func parseBasic(h string) (user, pass string, ok bool) {
	if !strings.HasPrefix(h, "Basic ") {
		return
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	if err != nil {
		return
	}
	u, p, found := strings.Cut(string(b), ":")
	return u, p, found
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// rawOrString keeps a JSON body as JSON in the archive and wraps anything
// else as a string.
func rawOrString(b []byte) []byte {
	if json.Valid(b) {
		return b
	}
	s, _ := json.Marshal(string(b))
	return s
}
