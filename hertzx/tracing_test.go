package hertzx

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return rec
}

func tracedServer(t *testing.T) (*server.Hertz, *context.Context) {
	t.Helper()
	var seen context.Context
	h := server.New(server.WithDisablePrintRoute(true))
	h.Use(Tracing(), RequestLog(), Recovery())
	h.GET("/user/:id", func(ctx context.Context, c *app.RequestContext) {
		seen = ctx
		c.String(200, "ok")
	})
	h.GET("/broken", func(ctx context.Context, c *app.RequestContext) { c.String(502, "bad") })
	return h, &seen
}

func TestTracingStartsAServerSpan(t *testing.T) {
	rec := recordSpans(t)
	logs := zlogtest.Capture(t)
	h, seen := tracedServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/user/7?x=1", nil)
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected one span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Name() != "GET /user/:id" || sp.SpanKind() != trace.SpanKindServer {
		t.Errorf("span = %s %v", sp.Name(), sp.SpanKind())
	}
	id := sp.SpanContext().TraceID().String()
	if got := w.Header().Get(TraceHeader); got != id {
		t.Errorf("X-Trace-Id = %q, want the trace id %s", got, id)
	}
	if rc := logs.Records(); rc[len(rc)-1].Fields["trace_id"] != id {
		t.Errorf("request log trace_id = %v, want %s", rc[len(rc)-1].Fields["trace_id"], id)
	}
	// The handler's context is what it calls RPC services with: the same
	// trace, as span and as TRACE_ID.
	if !trace.SpanContextFromContext(*seen).IsValid() || kitexx.TraceID(*seen) != id {
		t.Errorf("the handler context must carry the span and TRACE_ID %s", id)
	}

	// 5xx marks the span.
	ut.PerformRequest(h.Engine, "GET", "/broken", nil)
	if st := rec.Ended()[1].Status(); st.Code.String() != "Error" {
		t.Errorf("a 502 must mark the span: %v", st)
	}
}

func TestTracingContinuesTheCallersTrace(t *testing.T) {
	rec := recordSpans(t)
	zlogtest.Discard(t)
	h, _ := tracedServer(t)

	// W3C traceparent: the span is a child of the caller's.
	ut.PerformRequest(h.Engine, "GET", "/user/7", nil,
		ut.Header{Key: "traceparent", Value: "00-4feadb850a943f0f6ec77710af952a07-00f067aa0ba902b7-01"})
	sp := rec.Ended()[0]
	if sp.SpanContext().TraceID().String() != "4feadb850a943f0f6ec77710af952a07" || sp.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("traceparent not continued: %s parent %s", sp.SpanContext().TraceID(), sp.Parent().SpanID())
	}

	// Only an X-Trace-Id that is a trace id: same trace.
	w := ut.PerformRequest(h.Engine, "GET", "/user/7", nil, ut.Header{Key: TraceHeader, Value: "0123456789abcdef0123456789abcdef"})
	if got := rec.Ended()[1].SpanContext().TraceID().String(); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("X-Trace-Id not continued: %s", got)
	}
	if got := w.Header().Get(TraceHeader); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("response X-Trace-Id = %q", got)
	}

	// An X-Trace-Id that is not a trace id: a new trace, and the response says
	// which, so the caller can map its own id to ours.
	w = ut.PerformRequest(h.Engine, "GET", "/user/7", nil, ut.Header{Key: TraceHeader, Value: "abc-DEF_12345678"})
	got := rec.Ended()[2].SpanContext().TraceID().String()
	if w.Header().Get(TraceHeader) != got {
		t.Errorf("response X-Trace-Id = %q, want the new trace id %s", w.Header().Get(TraceHeader), got)
	}
}
