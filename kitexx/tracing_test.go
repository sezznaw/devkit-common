package kitexx

import (
	"context"
	"errors"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// recordSpans installs a tracer provider that keeps every span in memory for
// the test and restores the no-op provider afterwards.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return rec
}

func withRPCInfo(ctx context.Context, service, method, from string) context.Context {
	ri := rpcinfo.NewRPCInfo(
		rpcinfo.NewEndpointInfo(from, "", nil, nil),
		rpcinfo.NewEndpointInfo(service, method, nil, nil),
		rpcinfo.NewInvocation(service, method), nil, nil)
	return rpcinfo.NewCtxWithRPCInfo(ctx, ri)
}

func TestServerTracingStartsASpanAndTheLogFollowsIt(t *testing.T) {
	rec := recordSpans(t)
	logs := zlogtest.Capture(t)
	boom := errors.New("boom")
	ep := ServerTracing()(LoggingMiddleware()(func(ctx context.Context, req, resp any) error {
		if !trace.SpanContextFromContext(ctx).IsValid() {
			t.Error("the handler must run inside the server span")
		}
		return boom
	}))
	ctx := withRPCInfo(context.Background(), "order", "Create", "gateway")
	if err := ep(ctx, nil, nil); err != boom {
		t.Fatalf("error not propagated: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected one span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Name() != "order/Create" || sp.SpanKind() != trace.SpanKindServer {
		t.Errorf("span = %s %v", sp.Name(), sp.SpanKind())
	}
	if sp.Status().Code.String() != "Error" {
		t.Errorf("an error must mark the span: %v", sp.Status())
	}
	want := sp.SpanContext().TraceID().String()
	rc := logs.Records()
	if len(rc) != 1 || rc[0].Fields["trace_id"] != want {
		t.Fatalf("the request log must carry the span's trace id %s: %+v", want, rc)
	}
}

func TestClientTracingPropagatesToTheServer(t *testing.T) {
	rec := recordSpans(t)
	zlogtest.Discard(t)
	var serverCtx context.Context
	server := ServerTracing()(LoggingMiddleware()(func(ctx context.Context, req, resp any) error {
		serverCtx = ctx
		return nil
	}))
	// In one process the transient metainfo values a client sets are what the
	// server reads, which is what the TTHeader does between two processes.
	client := ClientTracing()(func(ctx context.Context, req, resp any) error {
		return server(withRPCInfo(ctx, "order", "Create", "gateway"), req, resp)
	})
	if err := client(withRPCInfo(context.Background(), "order", "Create", ""), nil, nil); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("expected a server and a client span, got %d", len(spans))
	}
	srv, cli := spans[0], spans[1]
	if srv.SpanKind() != trace.SpanKindServer || cli.SpanKind() != trace.SpanKindClient {
		t.Fatalf("kinds: %v %v", srv.SpanKind(), cli.SpanKind())
	}
	if srv.Parent().SpanID() != cli.SpanContext().SpanID() {
		t.Errorf("the server span must be a child of the client span")
	}
	if srv.SpanContext().TraceID() != cli.SpanContext().TraceID() {
		t.Errorf("one trace, two ids: %s %s", srv.SpanContext().TraceID(), cli.SpanContext().TraceID())
	}
	if TraceID(serverCtx) != cli.SpanContext().TraceID().String() {
		t.Errorf("TRACE_ID = %q, want the trace id", TraceID(serverCtx))
	}
}

func TestServerTracingContinuesATraceIDOnly(t *testing.T) {
	rec := recordSpans(t)
	zlogtest.Discard(t)
	ep := ServerTracing()(LoggingMiddleware()(func(ctx context.Context, req, resp any) error { return nil }))

	// A caller on an older common sends only TRACE_ID; it is a trace id.
	id := NewTraceID()
	ctx := metainfo.WithPersistentValue(withRPCInfo(context.Background(), "order", "Create", ""), TraceIDKey, id)
	if err := ep(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := rec.Ended()[0].SpanContext().TraceID().String(); got != id {
		t.Errorf("trace id = %s, want the caller's %s", got, id)
	}

	// A value that is not a trace id starts a new trace, and the log says the
	// new id, which is what reaches the services called.
	logs := zlogtest.Capture(t)
	ctx = metainfo.WithPersistentValue(withRPCInfo(context.Background(), "order", "Create", ""), TraceIDKey, "from-caller")
	if err := ep(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := rec.Ended()[1].SpanContext().TraceID().String()
	if rc := logs.Records(); rc[0].Fields["trace_id"] != got {
		t.Errorf("log trace_id = %v, want the span's %s", rc[0].Fields["trace_id"], got)
	}
}

func TestTracingOffKeepsTheOldTraceID(t *testing.T) {
	otel.SetTracerProvider(noop.NewTracerProvider())
	logs := zlogtest.Capture(t)
	ep := ServerTracing()(LoggingMiddleware()(func(ctx context.Context, req, resp any) error { return nil }))
	ctx := metainfo.WithPersistentValue(withRPCInfo(context.Background(), "order", "Create", ""), TraceIDKey, "from-caller")
	if err := ep(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if rc := logs.Records(); rc[0].Fields["trace_id"] != "from-caller" {
		t.Errorf("with tracing off the caller's id is logged as it is: %v", rc[0].Fields["trace_id"])
	}
}
