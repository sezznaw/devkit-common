package hertzx

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/sezznaw/devkit-common/otelx"
)

// Tracing starts a server span for every request, continuing the trace the
// caller sent (W3C traceparent header, or only an X-Trace-Id that is 32 hex
// characters), and puts it into the context: RequestLog logs its trace id
// and the RPC clients a handler calls with that context make child spans of
// it. It comes before RequestLog in New; with tracing off (otel.endpoint
// empty) the span records nothing.
func Tracing() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		ctx = otel.GetTextMapPropagator().Extract(ctx, headerCarrier{c})
		if !trace.SpanContextFromContext(ctx).IsValid() {
			ctx, _ = otelx.WithRemoteTraceID(ctx, string(c.GetHeader(TraceHeader)))
		}
		method, path := string(c.Method()), string(c.Path())
		route := c.FullPath()
		if route == "" {
			route = path
		}
		ctx, span := otelx.Tracer().Start(ctx, method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(method),
				semconv.HTTPRoute(route),
				semconv.URLPath(path),
				semconv.ClientAddress(c.ClientIP())))
		defer span.End()

		c.Next(ctx)

		status := c.Response.StatusCode()
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if status >= 500 {
			span.SetStatus(codes.Error, "")
		}
	}
}

// headerCarrier lets the propagator read the request headers.
type headerCarrier struct{ c *app.RequestContext }

var _ propagation.TextMapCarrier = headerCarrier{}

func (h headerCarrier) Get(key string) string { return string(h.c.GetHeader(key)) }
func (h headerCarrier) Set(key, value string) { h.c.Request.Header.Set(key, value) }
func (h headerCarrier) Keys() []string {
	var keys []string
	h.c.Request.Header.VisitAll(func(k, _ []byte) { keys = append(keys, string(k)) })
	return keys
}
