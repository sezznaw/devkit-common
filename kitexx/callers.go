package kitexx

import (
	"context"
	"fmt"
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
// the rule lists (and this service itself, for its own checks); a method
// in no rule accepts everybody. The caller is the service name the Kitex
// client sends in the TTHeader (what the request log shows as `from`), so
// a call with no caller name is refused on a guarded method.
//
//	callers:
//	  rules:
//	    - methods: [DebitBet, CreditSettle, ReverseBet]
//	      allow: [ser-bet]
//	    - methods: [ManualAdjust, ReviewApproval]
//	      allow: [ser-admin-api]
//
// callers: 哪些服务能调本服务的哪些方法。写进规则的方法只接受列出的服务（以及本服务自己，部署后检查用），
// 没写进任何规则的方法谁都能调。调用方是 Kitex 客户端在 TTHeader 里带的服务名（请求日志的 from），
// 没带名字的请求在受保护的方法上被拒。被拒回 1007。
type CallersConfig struct {
	Rules []CallerRule `yaml:"rules"`
}

// CallerRule allows a set of services on a set of methods.
type CallerRule struct {
	Methods []string `yaml:"methods"`
	Allow   []string `yaml:"allow"`
}

// Enabled says whether any rule is configured.
func (c CallersConfig) Enabled() bool { return len(c.Rules) > 0 }

// Validate checks every rule names at least one method and one service.
func (c CallersConfig) Validate() error {
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

// Callers is the middleware of `callers:`. self is this service's name,
// always allowed.
func Callers(self string, cfg CallersConfig) endpoint.Middleware {
	allowed := map[string]map[string]bool{} // method -> caller -> allowed
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
			if self != "" {
				set[self] = true
			}
		}
	}
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		if len(allowed) == 0 {
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
			set, guarded := allowed[method]
			if !guarded || set[from] {
				return next(ctx, req, resp)
			}
			metricsx.RPCServerRejected.WithLabelValues(method, "caller").Inc()
			zlog.Ctx(ctx).Warn("caller not allowed", zlog.Str("method", method), zlog.Str("from", from))
			if from == "" {
				from = "an unnamed caller"
			}
			return BizError(ctx, kerrors.NewBizStatusError(CodeForbidden, fmt.Sprintf("%s may not call %s", from, method)))
		}
	}
}
