package kitexx

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/kitex/pkg/kerrors"
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
