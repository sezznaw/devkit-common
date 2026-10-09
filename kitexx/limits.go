package kitexx

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/limit"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/server"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// LimitsConfig is the `limits:` section of an RPC service: what one instance
// refuses so that an overload degrades into quick "busy" answers instead of
// a pile of slow ones. A refused request is answered with business code 5003
// (CodeBusy), which the gateway passes through like any other code and a
// caller may retry a moment later; Kitex's own limiter is not used for QPS
// because it closes the connection, which a client cannot tell from a crash.
//
// LimitsConfig 是 RPC 服务的 `limits:` 段：一个实例拒绝什么，让过载变成快速的"忙"而不是
// 一堆慢请求。被拒的请求回业务码 5003（CodeBusy），网关照常透传，调用方稍后可重试；QPS 不用
// Kitex 自带的限流器，因为它直接断连接，客户端分不出是过载还是崩了。
type LimitsConfig struct {
	// MaxInFlight caps the requests one instance handles at the same time;
	// the next one is refused at once with 5003. Default 1000; 0 is no cap
	// (not advised: a stalled downstream then piles up goroutines).
	MaxInFlight int `yaml:"max_in_flight"`
	// MaxQPS caps the requests one instance accepts per second (counted in
	// 100ms windows, so a burst of a tenth of it passes). Default off; set it
	// when the service has a known capacity.
	MaxQPS int `yaml:"max_qps"`
	// MaxConnections caps the open connections (Kitex's limiter: a connection
	// over the cap is closed). Default 10000.
	MaxConnections int `yaml:"max_connections"`
}

func (c LimitsConfig) inFlight() int {
	if c.MaxInFlight < 0 {
		return 0
	}
	if c.MaxInFlight == 0 {
		return 1000
	}
	return c.MaxInFlight
}

func (c LimitsConfig) connections() int {
	if c.MaxConnections <= 0 {
		return 10000
	}
	return c.MaxConnections
}

// CodeBusy is the business code of a request an instance refused for its
// limits (limits.max_in_flight, limits.max_qps): the caller may retry soon.
const CodeBusy int32 = 5003

// Limits is the middleware of `limits:`: an in-flight cap and a QPS cap per
// instance, both answered with CodeBusy. rt.Options installs it first in
// the chain, so a refused request costs nothing else.
func Limits(cfg LimitsConfig) endpoint.Middleware {
	l := &limiter{maxInFlight: int64(cfg.inFlight()), maxQPS: cfg.MaxQPS}
	if l.maxQPS > 0 {
		l.perWindow = int64(l.maxQPS) / 10
		if l.perWindow < 1 {
			l.perWindow = 1
		}
	}
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			if l.maxInFlight > 0 {
				if n := l.inFlight.Add(1); n > l.maxInFlight {
					l.inFlight.Add(-1)
					return l.refuse(ctx, "in_flight", "服务忙，请稍后重试")
				}
				defer l.inFlight.Add(-1)
			}
			if l.maxQPS > 0 && !l.take() {
				return l.refuse(ctx, "qps", "服务忙，请稍后重试")
			}
			return next(ctx, req, resp)
		}
	}
}

type limiter struct {
	maxInFlight int64
	inFlight    atomic.Int64

	maxQPS    int
	perWindow int64
	mu        sync.Mutex
	window    int64 // unix 100ms slot
	count     int64

	warnedAt atomic.Int64
}

// take counts one request against the current 100ms window.
func (l *limiter) take() bool {
	slot := time.Now().UnixNano() / int64(100*time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	if slot != l.window {
		l.window, l.count = slot, 0
	}
	if l.count >= l.perWindow {
		return false
	}
	l.count++
	return true
}

func (l *limiter) refuse(ctx context.Context, reason, msg string) error {
	method := ""
	if ri := rpcinfo.GetRPCInfo(ctx); ri != nil && ri.To() != nil {
		method = ri.To().Method()
	}
	metricsx.RPCServerRejected.WithLabelValues(method, reason).Inc()
	now := time.Now().Unix()
	if last := l.warnedAt.Load(); now-last >= 60 && l.warnedAt.CompareAndSwap(last, now) {
		zlog.Ctx(ctx).Warn("requests refused: instance over its limits (logged once a minute)", zlog.Str("reason", reason), zlog.Str("method", method), zlog.Int("in_flight", l.inFlight.Load()), zlog.Int("max_in_flight", l.maxInFlight), zlog.Int("max_qps", int64(l.maxQPS)))
	}
	return BizError(ctx, kerrors.NewBizStatusError(CodeBusy, msg))
}

// connectionLimit is Kitex's connection cap (a connection over it is closed
// at accept) with a reporter that counts the refusals.
func connectionLimit(cfg LimitsConfig) []server.Option {
	return []server.Option{
		server.WithLimit(&limit.Option{MaxConnections: cfg.connections()}),
		server.WithLimitReporter(limitReporter{}),
	}
}

type limitReporter struct{}

func (limitReporter) ConnOverloadReport() {
	metricsx.RPCServerRejected.WithLabelValues("", "connections").Inc()
	zlog.Warn("connection refused: limits.max_connections reached")
}
func (limitReporter) QPSOverloadReport() {}
