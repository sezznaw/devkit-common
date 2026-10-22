package kitexx

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// CodeForbidden is the business code of a request refused because its
// caller is not on the method's allow-list (callers.rules); the same code
// a gateway answers for a missing permission point.
const CodeForbidden int32 = 1007

// CallersConfig is the `callers:` section: which services may call which
// methods of this one. A method named in a rule accepts only the services
// the rule lists (and this service itself, for its own checks). A method in
// no rule follows Default: allow (the default) lets everybody through, log
// lets everybody through and counts and warns, deny refuses all but this
// service itself. "*" as a method covers every method; "*" in allow admits
// any caller. The caller is the name service_identity proved, otherwise the
// service name the Kitex client sends in the TTHeader (what the request log
// shows as `from`), so a call with no caller name is refused on a guarded
// method. A method nobody else calls is listed with allow: [<this service>].
//
//	callers:
//	  default: deny
//	  rules:
//	    - methods: [DebitBet, CreditSettle, ReverseBet]
//	      allow: [ser-bet]
//	    - methods: [ManualAdjust, ReviewApproval]
//	      allow: [ser-admin-api]
//
// callers: 哪些服务能调本服务的哪些方法。写进规则的方法只接受列出的服务（以及本服务自己，部署后检查用）；
// 没写进任何规则的方法按 default：allow（默认）谁都能调，log 放行但计数并记 warn，deny 只放本服务自己。
// 方法写 "*" 表示全部方法，allow 写 "*" 表示任何调用方。调用方优先取服务身份核对出的名字，否则是 Kitex
// 客户端在 TTHeader 里带的服务名（请求日志的 from），没带名字的请求在受保护的方法上被拒。被拒回 1007。
// 只有本服务自己调的方法写 allow: [<本服务名>]。default 没写时取环境变量 CALLERS_DEFAULT。
type CallersConfig struct {
	Default string       `yaml:"default"`
	Rules   []CallerRule `yaml:"rules"`
}

// Callers defaults.
const (
	CallersAllow = "allow"
	CallersLog   = "log"
	CallersDeny  = "deny"
)

// DefaultPolicy is Default, else the environment variable CALLERS_DEFAULT,
// else allow.
func (c CallersConfig) DefaultPolicy() string {
	d := strings.TrimSpace(c.Default)
	if d == "" {
		d = strings.TrimSpace(os.Getenv("CALLERS_DEFAULT"))
	}
	if d == "" {
		return CallersAllow
	}
	return d
}

// CallerRule allows a set of services on a set of methods.
type CallerRule struct {
	Methods []string `yaml:"methods"`
	Allow   []string `yaml:"allow"`
}

// Enabled says whether callers does anything: a rule, or a default other
// than allow.
func (c CallersConfig) Enabled() bool {
	return len(c.Rules) > 0 || c.DefaultPolicy() != CallersAllow
}

// Validate checks the default and that every rule names at least one
// method and one service.
func (c CallersConfig) Validate() error {
	switch d := c.DefaultPolicy(); d {
	case CallersAllow, CallersLog, CallersDeny:
	default:
		return fmt.Errorf("callers.default %q: allow, log or deny", d)
	}
	for i, r := range c.Rules {
		if len(r.Methods) == 0 || len(r.Allow) == 0 {
			return fmt.Errorf("callers.rules[%d]: methods and allow are both required", i)
		}
		for _, m := range r.Methods {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("callers.rules[%d]: empty method name", i)
			}
		}
	}
	return nil
}

// Covers says whether a rule names method (or "*"): what devkit lint
// requires of every method of the IDL.
func (c CallersConfig) Covers(method string) bool {
	for _, r := range c.Rules {
		for _, m := range r.Methods {
			if m == method || m == "*" {
				return true
			}
		}
	}
	return false
}

// Callers is the middleware of `callers:`. self is this service's name,
// always allowed.
func Callers(self string, cfg CallersConfig) endpoint.Middleware {
	allowed := map[string]map[string]bool{} // method ("*": every method) -> caller ("*": anyone) -> allowed
	for _, r := range cfg.Rules {
		for _, m := range r.Methods {
			set := allowed[m]
			if set == nil {
				set = map[string]bool{}
				allowed[m] = set
			}
			for _, s := range r.Allow {
				set[s] = true
			}
		}
	}
	policy := cfg.DefaultPolicy()
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		if len(allowed) == 0 && policy == CallersAllow {
			return next
		}
		return func(ctx context.Context, req, resp any) error {
			method, from := "", ""
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if ri.Invocation() != nil {
					method = ri.Invocation().MethodName()
				}
				if ri.From() != nil {
					from = ri.From().ServiceName()
				}
			}
			if proven := VerifiedCaller(ctx); proven != "" {
				from = proven // service_identity: proven by the caller's ServiceAccount token
			}
			if self != "" && from == self {
				return next(ctx, req, resp)
			}
			set, listed := allowed[method]
			all, wild := allowed["*"]
			if listed || wild {
				if set[from] || set["*"] || (from != "" && (all[from] || all["*"])) {
					return next(ctx, req, resp)
				}
				return refuseCaller(ctx, method, from, "caller")
			}
			switch policy {
			case CallersDeny:
				return refuseCaller(ctx, method, from, "caller_unlisted")
			case CallersLog:
				metricsx.RPCServerRejected.WithLabelValues(method, "caller_unlisted_log").Inc()
				zlog.Ctx(ctx).Warn("caller of an unlisted method (callers.default log: let through)", zlog.Str("method", method), zlog.Str("from", from))
			}
			return next(ctx, req, resp)
		}
	}
}

func refuseCaller(ctx context.Context, method, from, reason string) error {
	metricsx.RPCServerRejected.WithLabelValues(method, reason).Inc()
	zlog.Ctx(ctx).Warn("caller not allowed", zlog.Str("method", method), zlog.Str("from", from), zlog.Str("reason", reason))
	if from == "" {
		from = "an unnamed caller"
	}
	return BizError(ctx, kerrors.NewBizStatusError(CodeForbidden, fmt.Sprintf("%s may not call %s", from, method)))
}
