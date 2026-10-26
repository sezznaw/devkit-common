package kitexx

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sezznaw/devkit-common/metricsx"
)

func TestServerMetricsCountByCode(t *testing.T) {
	results := []error{nil, kerrors.NewBizStatusError(2001, "member not found"), errors.New("boom")}
	i := 0
	mw := ServerMetrics()(func(ctx context.Context, req, resp any) error { err := results[i]; i++; return err })
	ctx := withRPCInfo(context.Background(), "user-m", "GetProfile", "gateway")
	for range results {
		_ = mw(ctx, nil, nil)
	}
	for code, want := range map[string]float64{"ok": 1, "2001": 1, "error": 1} {
		if got := testutil.ToFloat64(metricsx.RPCServerRequests.WithLabelValues("user-m", "GetProfile", code)); got != want {
			t.Errorf("code %s: %v", code, got)
		}
	}
	if n := testutil.CollectAndCount(metricsx.RPCServerDuration, "rpc_server_duration_seconds"); n < 1 {
		t.Error("no duration observed")
	}
}

// Kitex does not return a business error to the middlewares: the server
// moves the one a handler returns into the RPCInfo (invokeHandleEndpoint;
// BizError does the same for a middleware), the client's TTHeader meta
// handler puts the one of the response there, and only Call returns it,
// after every middleware. Both metrics read it there; before, such a call
// was counted "ok". realkitex_test.go checks the same against real Kitex.
func TestMetricsReadTheBusinessCodeFromTheRPCInfo(t *testing.T) {
	answered := func(ctx context.Context, req, resp any) error {
		rpcinfo.GetRPCInfo(ctx).Invocation().(rpcinfo.InvocationSetter).SetBizStatusErr(kerrors.NewBizStatusError(3004, "conflict"))
		return nil
	}
	for side, c := range map[string]struct {
		mw  endpoint.Middleware
		vec *prometheus.CounterVec
	}{
		"server": {ServerMetrics(), metricsx.RPCServerRequests},
		"client": {ClientMetrics(), metricsx.RPCClientRequests},
	} {
		code := func(label string) float64 {
			return testutil.ToFloat64(c.vec.WithLabelValues("wallet-m", "Transfer", label))
		}
		biz, ok := code("3004"), code("ok")
		if err := c.mw(answered)(withRPCInfo(context.Background(), "wallet-m", "Transfer", "gateway"), nil, nil); err != nil {
			t.Fatal(err)
		}
		if code("3004") != biz+1 || code("ok") != ok {
			t.Errorf("%s: a business answer is counted as its code: 3004 %v -> %v, ok %v -> %v", side, biz, code("3004"), ok, code("ok"))
		}
	}
}
