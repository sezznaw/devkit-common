package hertzx

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sezznaw/devkit-common/metricsx"
)

func TestMetricsCountByRouteNotPath(t *testing.T) {
	h := server.New(server.WithDisablePrintRoute(true))
	h.Use(Metrics())
	h.GET("/m/:id", func(ctx context.Context, c *app.RequestContext) { c.String(200, "ok") })
	ut.PerformRequest(h.Engine, "GET", "/m/7", nil)
	ut.PerformRequest(h.Engine, "GET", "/m/8", nil)
	ut.PerformRequest(h.Engine, "GET", "/nowhere/abc", nil)
	if got := testutil.ToFloat64(metricsx.HTTPServerRequests.WithLabelValues("GET", "/m/:id", "200")); got != 2 {
		t.Errorf("route /m/:id 200: %v", got)
	}
	if got := testutil.ToFloat64(metricsx.HTTPServerRequests.WithLabelValues("GET", "unmatched", "404")); got != 1 {
		t.Errorf("unmatched 404: %v", got)
	}
}
