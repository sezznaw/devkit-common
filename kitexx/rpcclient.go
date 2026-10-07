package kitexx

import (
	"errors"
	"sync"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/circuitbreak"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/retry"
	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog"
)

// RPCClientConfig is the `rpc_client:` section: how this service calls the
// other services. Defaults are what a service in the same cluster should
// have; a section is only needed to change them.
type RPCClientConfig struct {
	// Timeout of one call, default 3s. A downstream that hangs then costs
	// one request 3s, not every goroutine of the process.
	Timeout config.Duration `yaml:"timeout"`
	// ConnectTimeout, default 1s.
	ConnectTimeout config.Duration `yaml:"connect_timeout"`
	// Retries after a failed attempt, default 1. Only when the request was
	// never sent (no connection, no instance): a timeout or an error from
	// the handler is not retried by the framework, because the call may
	// have been executed. A method with a request_id is safe to retry by
	// the caller itself.
	Retries *int `yaml:"retries"`
	// Breaker, default true: calls to a service whose error rate is over
	// 50% (of at least 200 calls in the window) fail at once with
	// kerrors.ErrCircuitBreak until it recovers.
	Breaker *bool `yaml:"breaker"`
}

const (
	defaultRPCTimeout     = 3 * time.Second
	defaultConnectTimeout = time.Second
	defaultRPCRetries     = 1
)

func (c RPCClientConfig) retries() int {
	if c.Retries == nil {
		return defaultRPCRetries
	}
	return max(0, *c.Retries)
}

func (c RPCClientConfig) breaker() bool { return c.Breaker == nil || *c.Breaker }

// rpcClientOptions are the protections of every client this service makes.
func rpcClientOptions(cfg Config) []client.Option {
	c := cfg.RPCClient
	opts := []client.Option{
		client.WithRPCTimeout(c.Timeout.Or(defaultRPCTimeout)),
		client.WithConnectTimeout(c.ConnectTimeout.Or(defaultConnectTimeout)),
	}
	if n := c.retries(); n > 0 {
		p := retry.NewFailurePolicyWithResultRetry(&retry.ShouldResultRetry{ErrorRetry: retryOnlyUnsent})
		p.WithMaxRetryTimes(n)
		opts = append(opts, client.WithFailureRetry(p))
	}
	if c.breaker() {
		opts = append(opts, client.WithCircuitBreaker(cbSuite()))
	}
	return opts
}

// retryOnlyUnsent says yes only for errors that mean the request never
// reached a server: no instance, no connection. Everything else (timeout,
// handler error, transport error after sending) may have been executed.
func retryOnlyUnsent(err error, _ rpcinfo.RPCInfo) bool {
	return errors.Is(err, kerrors.ErrGetConnection) || errors.Is(err, kerrors.ErrNoConnection) ||
		errors.Is(err, kerrors.ErrNoDestAddress) || errors.Is(err, kerrors.ErrNoDestService)
}

var (
	cbOnce sync.Once
	cb     *circuitbreak.CBSuite
)

// cbSuite is one circuit-breaker suite per process, keyed by the service
// and method called; state changes are logged.
func cbSuite() *circuitbreak.CBSuite {
	cbOnce.Do(func() {
		cb = circuitbreak.NewCBSuite(circuitbreak.RPCInfo2Key)
		OnShutdown("circuit breaker", cb.Close)
		zlog.Info("rpc client protections on", zlog.Str("timeout", defaultRPCTimeout.String()), zlog.Str("retry", "only unsent requests"), zlog.Bool("breaker", true))
	})
	return cb
}
