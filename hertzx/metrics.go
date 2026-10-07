package hertzx

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/sezznaw/devkit-common/metricsx"
)

// Metrics counts and times every HTTP request (http_server_requests_total,
// http_server_duration_seconds) by method, route and status. The route is
// the pattern of the IDL ("/v1/member/getProfile", "/user/:id"), never the
// raw path, so a scan of random URLs cannot grow the metric: a request that
// matched no route is counted as route "unmatched". Part of New.
func Metrics() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		c.Next(ctx)
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		method := string(c.Method())
		metricsx.HTTPServerRequests.WithLabelValues(method, route, strconv.Itoa(c.Response.StatusCode())).Inc()
		metricsx.HTTPServerDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	}
}
