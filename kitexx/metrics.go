package kitexx

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"

	"github.com/sezznaw/devkit-common/metricsx"
)

// ServerMetrics counts and times every RPC handled
// (rpc_server_requests_total, rpc_server_duration_seconds), by service,
// method and result code: the business code of a BizStatusError, "ok", or
// "error" for any other failure. Part of Options.
func ServerMetrics() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			start := time.Now()
			err := next(ctx, req, resp)
			service, method, _ := rpcNames(ctx)
			metricsx.RPCServerRequests.WithLabelValues(service, method, resultCode(err)).Inc()
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
			metricsx.RPCClientRequests.WithLabelValues(service, method, resultCode(err)).Inc()
			metricsx.RPCClientDuration.WithLabelValues(service, method).Observe(time.Since(start).Seconds())
			return err
		}
	}
}

// resultCode is the `code` label: "ok", the business code, or "error".
// A business code is bounded (the table in the idl repository), so it is
// safe as a label; an error message is not.
func resultCode(err error) string {
	if err == nil {
		return "ok"
	}
	if b, ok := kerrors.FromBizStatusError(err); ok {
		return strconv.Itoa(int(b.BizStatusCode()))
	}
	return "error"
}
