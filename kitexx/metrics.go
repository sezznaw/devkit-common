package kitexx

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/pkg/endpoint"

	"github.com/sezznaw/devkit-common/metricsx"
)

// ServerMetrics counts and times every RPC handled
// (rpc_server_requests_total, rpc_server_duration_seconds), by service,
// method and result code (resultCode). Part of Options.
func ServerMetrics() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			start := time.Now()
			err := next(ctx, req, resp)
			service, method, _ := rpcNames(ctx)
			metricsx.RPCServerRequests.WithLabelValues(service, method, resultCode(ctx, err)).Inc()
			metricsx.RPCServerDuration.WithLabelValues(service, method).Observe(time.Since(start).Seconds())
			return err
		}
	}
}

// ClientMetrics is ServerMetrics for the calls this service makes
// (rpc_client_requests_total, rpc_client_duration_seconds, by the service
// called). Part of ClientOptions.
func ClientMetrics() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			start := time.Now()
			err := next(ctx, req, resp)
			service, method, _ := rpcNames(ctx)
			metricsx.RPCClientRequests.WithLabelValues(service, method, resultCode(ctx, err)).Inc()
			metricsx.RPCClientDuration.WithLabelValues(service, method).Observe(time.Since(start).Seconds())
			return err
		}
	}
}

// resultCode is the `code` label: "ok" for a success, the business code
// ("2001") for a call answered with one, "error" for any other failure (a
// plain error or a panic in the handler, a timeout, no connection, an open
// breaker). The business code is read where Kitex keeps it, the RPCInfo
// (bizCode): next returns nil for such a call, so looking at err alone
// counted it "ok". A business code is bounded (the table in the idl
// repository), so it is safe as a label; an error message is not.
func resultCode(ctx context.Context, err error) string {
	if code, ok := bizCode(ctx, err); ok {
		return strconv.Itoa(int(code))
	}
	if err != nil {
		return "error"
	}
	return "ok"
}
