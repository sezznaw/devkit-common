package hertzx

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
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
	Request    string    `json:"request"`    // JSON body, secrets redacted, at most 4 KiB
	Status     int       `json:"status"`     // HTTP status
	Code       int32     `json:"code"`       // business code of the envelope, -1 when there is none
	Msg        string    `json:"msg"`
	TraceID    string    `json:"trace_id"`
	ClientIP   string    `json:"client_ip"`
	UserAgent  string    `json:"user_agent"`
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
				out[strings.ToUpper(method)+" "+path] = auditOp{title: op.Summary, permission: op.Permission}
			}
		}
	}
	return out
}

type auditOp struct{ title, permission string }

// AuditedOperations lists the audited operations of the document, for a
// page that explains what is recorded.
func AuditedOperations(spec []byte) []string {
	var out []string
	for k := range auditedOperations(spec) {
		out = append(out, k)
	}
	return out
}

const auditBodyLimit = 4096

// Audit installs the audit middleware: install it after RequireLogin so
// the entry carries the login. Every audited operation produces one
// entry, whatever its outcome (a refused write is worth knowing about).
//
// Audit 装审计中间件，放在 RequireLogin 之后以带上登录身份。每个被审计的接口每次调用记一条，
// 不论成功失败（被拒的写操作也值得记）。
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
			Status:     c.Response.StatusCode(),
			Code:       -1,
			TraceID:    otelx.TraceID(ctx),
			ClientIP:   c.ClientIP(),
			UserAgent:  string(c.UserAgent()),
			DurationMS: time.Since(start).Milliseconds(),
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

var secretKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|key|otp|pin|card)`)

// Redact returns the JSON body with the values of secret-looking keys
// replaced by "***", cut to 4 KiB. A body that is not a JSON object is
// kept as is (cut) unless it is empty.
func Redact(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		s := string(body)
		if len(s) > auditBodyLimit {
			s = s[:auditBodyLimit]
		}
		return s
	}
	redact(m)
	b, _ := json.Marshal(m)
	if len(b) > auditBodyLimit {
		b = b[:auditBodyLimit]
	}
	return string(b)
}

func redact(m map[string]any) {
	for k, v := range m {
		if secretKey.MatchString(k) {
			m[k] = "***"
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			redact(sub)
		}
	}
}

// AsyncSink turns a function that stores a batch (an RPC, a table) into a
// sink that never blocks a request: entries queue in memory (buffer
// 1000) and a worker flushes them in batches of up to 100 or every
// second. On overflow entries are dropped and counted in the log; on a
// store error the batch is retried once, then dropped and logged. Close
// flushes what is queued; register it with kitexx.OnShutdown.
//
// AsyncSink 把"存一批"的函数（RPC、表）变成不阻塞请求的 sink：内存队列 1000 条，后台每满
// 100 条或每秒刷一次；队列满丢弃并记日志，存储失败重试一次后丢弃并记日志。Close 刷完队列，
// 用 kitexx.OnShutdown 注册。
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
		return
	}
	select {
	case s.queue <- e:
	default:
		zlog.Ctx(ctx).Error("audit entry dropped: queue full", zlog.Str("action", e.Action), zlog.Int("uid", e.UID))
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
		if err != nil {
			zlog.Error("audit batch not stored", zlog.Int("entries", int64(len(batch))), zlog.Err(err))
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
