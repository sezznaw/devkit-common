package otelx

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func TestInitOffIsANoop(t *testing.T) {
	shutdown, err := Init(Config{}, Identity{Service: "order"})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, span := Tracer().Start(context.Background(), "x")
	if span.IsRecording() {
		t.Error("with no endpoint nothing must be recorded")
	}
	span.End()
	// The propagator is installed regardless, so a trace that arrives is
	// passed on.
	if otel.GetTextMapPropagator() == nil || len(otel.GetTextMapPropagator().Fields()) == 0 {
		t.Error("propagator must be installed with tracing off")
	}
}

func TestInitOnExportsToTheEndpoint(t *testing.T) {
	// The exporter connects lazily: an address nobody listens on must not
	// fail Init, only cost the spans.
	shutdown, err := Init(Config{Endpoint: "127.0.0.1:1"}, Identity{Service: "order", Env: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := Tracer().Start(context.Background(), "x")
	if !span.IsRecording() {
		t.Error("with an endpoint spans are recorded")
	}
	if TraceID(ctx) != span.SpanContext().TraceID().String() {
		t.Error("TraceID reads the span in the context")
	}
	span.End()
	ctx2, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(ctx2) // the export fails, which is not this test's business
}

func TestWithRemoteTraceID(t *testing.T) {
	ctx, ok := WithRemoteTraceID(context.Background(), "4feadb850a943f0f6ec77710af952a07")
	if !ok {
		t.Fatal("a 32-hex id is a trace id")
	}
	sc := trace.SpanContextFromContext(ctx)
	if sc.TraceID().String() != "4feadb850a943f0f6ec77710af952a07" || !sc.IsRemote() || !sc.IsSampled() {
		t.Errorf("remote parent = %+v", sc)
	}
	for _, bad := range []string{"", "abc", "from-caller", "00000000000000000000000000000000", "zz"} {
		if _, ok := WithRemoteTraceID(context.Background(), bad); ok {
			t.Errorf("%q must not be accepted", bad)
		}
	}
	if TraceID(context.Background()) != "" {
		t.Error("no span, no id")
	}
}
