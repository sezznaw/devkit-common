package hertzx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/otelx"
	"github.com/sezznaw/devkit-common/zlog"
)

// The audit log of a back-office gateway: who did what, when, to what,
// with which outcome. The middleware records every operation the IDL
// marks or implies as audited and hands the entry to a sink; the gateway
// decides where entries go (an RPC to the account service, which stores
// them). Handlers write nothing.
//
// Which operations are audited, from the OpenAPI document (apidoc):
//   - `// @audit` on the IDL method: yes (x-audit: true), e.g. a login;
//   - `// @noaudit`: no (x-audit: false), e.g. a noisy write;
//   - otherwise: yes when the operation has a permission point that does
//     not end in ".view" (a write), no when it has none or is a view.
//
// 后台网关的操作审计：谁、何时、对什么、做了什么、结果如何。中间件按 IDL 自动记录并交给
// sink，handler 不写任何东西。哪些接口记：方法注释 `// @audit` 一定记（如登录），`// @noaudit`
// 不记，其余看权限点——有权限点且不以 .view 结尾（写操作）就记。

// AuditEntry is one recorded operation.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Realm      string    `json:"realm"`
	UID        int64     `json:"uid"`
	Action     string    `json:"action"`     // the operation's title from the IDL comment
	Method     string    `json:"method"`     // HTTP method
	Path       string    `json:"path"`       // route
	Permission string    `json:"permission"` // the operation's permission point, if any
	Request    string    `json:"request"`    // body as JSON, secrets redacted, at most 4 KiB (AuditTruncated at the end when cut)
	Response   string    `json:"response"`   // the answer, likewise
	Status     int       `json:"status"`     // HTTP status
	Code       int32     `json:"code"`       // business code of the envelope, -1 when there is none
	Msg        string    `json:"msg"`
	TraceID    string    `json:"trace_id"`
	ClientIP   string    `json:"client_ip"`
	UserAgent  string    `json:"user_agent"` // at most 255 bytes
	DeviceID   string    `json:"device_id"`  // X-Device-Id the client sent (kitexx.DeviceID), "" when none
	DurationMS int64     `json:"duration_ms"`
}

// AuditSink receives entries; it must not block the request (see
// AsyncSink).
type AuditSink func(ctx context.Context, e AuditEntry)

// auditedOperations reads which operations to audit: "METHOD /path" ->
// title.
func auditedOperations(spec []byte) map[string]auditOp {
	out := map[string]auditOp{}
	var doc struct {
		Paths map[string]map[string]struct {
			Summary    string `yaml:"summary"`
			Permission string `yaml:"x-permission"`
			Audit      *bool  `yaml:"x-audit"`
			Sensitive  bool   `yaml:"x-sensitive"`
		} `yaml:"paths"`
	}
	if yaml.Unmarshal(spec, &doc) != nil {
		return out
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			audited := op.Permission != "" && !strings.HasSuffix(op.Permission, ".view")
			if op.Audit != nil {
				audited = *op.Audit
			}
			if audited {
				out[strings.ToUpper(method)+" "+path] = auditOp{title: op.Summary, permission: op.Permission, sensitive: op.Sensitive}
			}
		}
	}
	return out
}

type auditOp struct {
	title, permission string
	sensitive         bool // @sensitive: the response shows personal data in clear; not recorded
}

// AuditSensitiveResponse stands in the log for the response of a @sensitive
// operation: what it revealed is not kept where every reader of the log sees it.
//
// AuditSensitiveResponse 是 @sensitive 接口的响应在审计里的占位：它解锁出来的明文不留在人人可读的日志里。
const AuditSensitiveResponse = "[sensitive response not recorded]"

// AuditedOperations lists the audited operations of the document, for a
// page that explains what is recorded.
func AuditedOperations(spec []byte) []string {
	var out []string
	for k := range auditedOperations(spec) {
		out = append(out, k)
	}
	return out
}

const (
	// auditStoreDownAfter refusals in a row, entry by entry, end the retry of a batch.
	auditStoreDownAfter = 3
	auditBodyLimit      = 4096
	auditUALimit        = 255
)

// AuditTruncated ends a body that was longer than the limit and cut (at a
// character boundary), so a viewer can say so instead of showing broken JSON.
//
// AuditTruncated 是超长被截断的报文末尾的标记（在字符边界上截），查看页据此提示「已截断」。
const AuditTruncated = "…[truncated]"

// Audit installs the audit middleware: install it after RequireLogin so
// the entry carries the login. Every audited operation produces one
// entry, whatever its outcome (a refused write is worth knowing about).
// The entry keeps both bodies, request and response, redacted and cut to
// 4 KiB each, so a back office can show what was asked and what was
// answered; and the device id the client sent (the Client middleware put
// it in the context), for a login log.
//
// Audit 装审计中间件，放在 RequireLogin 之后以带上登录身份。每个被审计的接口每次调用记一条，
// 不论成功失败（被拒的写操作也值得记）。请求体和响应体都记（打码、各截 4 KiB），后台的
// 「请求 / 响应参数」弹窗用；还记客户端送来的设备号（Client 中间件放进 ctx 的），登录日志用。
func Audit(h *server.Hertz, spec []byte, sink AuditSink) {
	ops := auditedOperations(spec)
	if len(ops) == 0 {
		zlog.Warn("Audit: the IDL has nothing to audit (no @audit and no non-view permission point)")
	}
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		op, ok := ops[string(c.Method())+" "+string(c.Path())]
		if !ok {
			c.Next(ctx)
			return
		}
		start := time.Now()
		request := Redact(c.Request.Body())
		c.Next(ctx)
		e := AuditEntry{
			Time:       start,
			Realm:      string(kitexx.Realm(ctx)),
			Action:     op.title,
			Method:     string(c.Method()),
			Path:       string(c.Path()),
			Permission: op.permission,
			Request:    request,
			Response:   AuditSensitiveResponse,
			Status:     c.Response.StatusCode(),
			Code:       -1,
			TraceID:    otelx.TraceID(ctx),
			ClientIP:   c.ClientIP(),
			UserAgent:  cutUTF8(string(c.UserAgent()), auditUALimit, ""),
			DeviceID:   kitexx.DeviceID(ctx),
			DurationMS: time.Since(start).Milliseconds(),
		}
		if !op.sensitive {
			e.Response = Redact(c.Response.Body())
		}
		if uid, logged := kitexx.UID(ctx); logged {
			e.UID = uid
		}
		if e.TraceID == "" {
			e.TraceID = string(c.Response.Header.Peek(TraceHeader))
		}
		var env struct {
			Code *int32 `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(c.Response.Body(), &env) == nil && env.Code != nil {
			e.Code, e.Msg = *env.Code, env.Msg
		}
		sink(ctx, e)
	})
}

// secretKey matches the keys whose values never reach the audit log:
// credentials, and the identity documents and bank, payment and crypto
// accounts a back office handles (phone and email stay readable: a decision
// of the project, 2026-10-21). The operations that reveal personal data in
// clear say `// @sensitive` and keep no response at all.
var secretKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|key|otp|pin|card|credential|authorization|cookie|session|signature|ticket|id_?no|id_?number|identity|bank|_account|account_?no|crypto|wallet_?address)`)

// Redact returns a request or response body for the audit log: JSON (any
// shape: objects, arrays, at any depth) and form bodies come back as JSON
// with the values of secret-looking keys replaced by "***"; anything else
// is not recorded, only its size, since nothing in it can be redacted. The
// result is at most 4 KiB, cut at a character boundary and ended with
// AuditTruncated when longer, so it is always valid UTF-8.
//
// Redact 把请求 / 响应体整理成审计用的文字：JSON（对象、数组、任意层级）和表单都转成 JSON，
// 密钥类键的值换成 "***"；其他格式无法打码，只记大小不记内容。结果最多 4 KiB，在字符边界上截，
// 截了就以 AuditTruncated 结尾，保证是合法 UTF-8（MySQL 严格模式会拒绝半个字符，整批写不进去）。
func Redact(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() {
		form, ok := parseForm(body)
		if !ok {
			return fmt.Sprintf("<non-JSON body, %d bytes, not recorded>", len(body))
		}
		v = form
	}
	b, err := json.Marshal(redactValue(v))
	if err != nil {
		return fmt.Sprintf("<body, %d bytes, not recorded>", len(body))
	}
	return cutUTF8(string(b), auditBodyLimit, AuditTruncated)
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			if secretKey.MatchString(k) {
				x[k] = "***"
				continue
			}
			x[k] = redactValue(sub)
		}
	case []any:
		for i, sub := range x {
			x[i] = redactValue(sub)
		}
	}
	return v
}

// parseForm reads an application/x-www-form-urlencoded body (one value:
// a string, several: a list).
func parseForm(body []byte) (map[string]any, bool) {
	if !bytes.Contains(body, []byte("=")) || !utf8.Valid(body) {
		return nil, false
	}
	q, err := url.ParseQuery(string(body))
	if err != nil || len(q) == 0 {
		return nil, false
	}
	out := make(map[string]any, len(q))
	for k, vs := range q {
		if len(vs) == 1 {
			out[k] = vs[0]
			continue
		}
		list := make([]any, len(vs))
		for i, s := range vs {
			list[i] = s
		}
		out[k] = list
	}
	return out, true
}

// cutUTF8 makes s valid UTF-8 and at most limit bytes, cutting at a
// character boundary and ending a cut string with mark (counted in limit).
func cutUTF8(s string, limit int, mark string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= limit {
		return s
	}
	n := limit - len(mark)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + mark
}

// AsyncSink turns a function that stores a batch (an RPC, a table) into a
// sink that never blocks a request: entries queue in memory (buffer
// 1000) and a worker flushes them in batches of up to 100 or every
// second. On overflow entries are dropped. On a store error the batch is
// retried once, then stored entry by entry, so one entry the store refuses
// does not take the others with it; only the refused ones are dropped.
// Three refusals in a row mean the store is down, not the entries: the
// rest of the batch is dropped at once (reason store_down) instead of
// waiting out a timeout per entry while the queue fills behind it.
// Every drop is logged and counted (metric audit_entries_dropped_total by
// reason) for an alert. Close flushes what is queued; register it with
// kitexx.OnShutdown.
//
// AsyncSink 把"存一批"的函数（RPC、表）变成不阻塞请求的 sink：内存队列 1000 条，后台每满
// 100 条或每秒刷一次；队列满丢弃。存储失败先整批重试一次，再逐条存，一条被拒不连累同批的其他
// 记录，只丢被拒的那条。连续 3 条都被拒说明是存储宕了而不是记录有问题：同批剩下的直接丢弃
// （原因 store_down），不再每条等一次超时、让队列在后面堆满。每次丢弃都记日志并计数（指标
// audit_entries_dropped_total，按原因），可配告警。Close 刷完队列，用 kitexx.OnShutdown 注册。
type AsyncSinkT struct {
	store  func(ctx context.Context, batch []AuditEntry) error
	queue  chan AuditEntry
	done   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
	closed bool
}

// NewAsyncSink starts the worker.
func NewAsyncSink(store func(ctx context.Context, batch []AuditEntry) error) *AsyncSinkT {
	s := &AsyncSinkT{store: store, queue: make(chan AuditEntry, 1000), done: make(chan struct{})}
	go s.run()
	return s
}

// Sink is the AuditSink to give Audit.
func (s *AsyncSinkT) Sink(ctx context.Context, e AuditEntry) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		zlog.Ctx(ctx).Error("audit entry dropped: sink closed", zlog.Str("action", e.Action), zlog.Int("uid", e.UID))
		metricsx.AuditEntriesDropped.WithLabelValues("closed").Inc()
		return
	}
	select {
	case s.queue <- e:
	default:
		zlog.Ctx(ctx).Error("audit entry dropped: queue full", zlog.Str("action", e.Action), zlog.Int("uid", e.UID))
		metricsx.AuditEntriesDropped.WithLabelValues("queue_full").Inc()
	}
}

func (s *AsyncSinkT) run() {
	defer close(s.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var batch []AuditEntry
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.store(ctx, batch)
		if err != nil {
			err = s.store(ctx, batch)
		}
		cancel()
		if err != nil && len(batch) > 1 {
			zlog.Warn("audit batch not stored, storing one by one", zlog.Int("entries", int64(len(batch))), zlog.Err(err))
			failed := 0
			for i, e := range batch {
				if failed >= auditStoreDownAfter {
					rest := len(batch) - i
					zlog.Error("audit entries dropped: the store refuses everything", zlog.Int("entries", int64(rest)))
					metricsx.AuditEntriesDropped.WithLabelValues("store_down").Add(float64(rest))
					break
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := s.store(ctx, []AuditEntry{e}); err != nil {
					failed++
					zlog.Error("audit entry not stored", zlog.Str("action", e.Action), zlog.Int("uid", e.UID), zlog.Err(err))
					metricsx.AuditEntriesDropped.WithLabelValues("store_error").Inc()
				} else {
					failed = 0
				}
				cancel()
			}
		} else if err != nil {
			zlog.Error("audit entry not stored", zlog.Str("action", batch[0].Action), zlog.Int("uid", batch[0].UID), zlog.Err(err))
			metricsx.AuditEntriesDropped.WithLabelValues("store_error").Inc()
		}
		batch = nil
	}
	for {
		select {
		case e, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			// take what else is already waiting, then store at once: an
			// audited operation is rare and its record should not sit in
			// memory where a crash loses it
		drain:
			for len(batch) < 100 {
				select {
				case e, ok := <-s.queue:
					if !ok {
						flush()
						return
					}
					batch = append(batch, e)
				default:
					break drain
				}
			}
			flush()
		case <-ticker.C:
			flush()
		}
	}
}

// Close stops accepting entries, flushes the queue and returns.
func (s *AsyncSinkT) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.queue)
		s.mu.Unlock()
	})
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		zlog.Error("audit sink: flush timed out")
	}
	return nil
}
