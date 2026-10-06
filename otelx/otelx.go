// Package otelx sets up OpenTelemetry tracing for a service: an OTLP exporter
// to the collector named in the configuration, the W3C trace context
// propagator, and the resource that says which service the spans are from.
// kitexx and hertzx start the spans; a service only fills in `otel:` in its
// configuration.
package otelx

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config is the `otel:` section of a service configuration.
type Config struct {
	// Endpoint is the OTLP gRPC collector, "host:port" (Tempo listens on
	// 4317). Empty switches tracing off: no exporter, spans are not recorded,
	// and the trace_id is made the way it was before tracing existed. Usually
	// "${OTEL_EXPORTER_OTLP_ENDPOINT}", so that the deployment decides.
	Endpoint string `yaml:"endpoint"`
	// SampleRatio is the share of new traces that are recorded and exported,
	// 0 to 1. A trace that arrives already sampled (or not) from a caller
	// keeps that decision. Default 1: everything, right for dev; lower it in
	// production when the collector cannot keep up.
	SampleRatio *float64 `yaml:"sample_ratio"`
	// Insecure sends OTLP without TLS. Default true: the collector is inside
	// the cluster. Set false when it is not.
	Insecure *bool `yaml:"insecure"`
}

// Identity is what every span carries about the process that made it; the
// same four things zlog writes on every JSON record.
type Identity struct {
	Service string
	Version string
	Env     string
	Host    string
}

// Enabled says whether cfg switches tracing on.
func (c Config) Enabled() bool { return strings.TrimSpace(c.Endpoint) != "" }

func (c Config) sampleRatio() float64 {
	if c.SampleRatio == nil {
		return 1
	}
	return max(0, min(1, *c.SampleRatio))
}

func (c Config) insecure() bool { return c.Insecure == nil || *c.Insecure }

// tracerName is the instrumentation scope of the spans kit-common starts.
const tracerName = "github.com/sezznaw/devkit-common"

// Tracer is what kitexx and hertzx start their spans with. It follows the
// global provider, so a span started before Init is a no-op.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// Init installs the tracer provider and the propagator for the process and
// returns what to call once the server has stopped, which flushes the spans
// not yet exported. With tracing off it installs a provider that records
// nothing, so the middlewares cost next to nothing, and the propagator still,
// so a trace context that arrives is passed on to the services this one calls.
func Init(cfg Config, id Identity) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	if !cfg.Enabled() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(strings.TrimSpace(cfg.Endpoint))}
	if cfg.insecure() {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	// The exporter connects in the background; a collector that is down at
	// start costs spans, not the start.
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otelx: exporter for %q: %w", cfg.Endpoint, err)
	}

	attrs := []attribute.KeyValue{semconv.ServiceName(id.Service)}
	if id.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(id.Version))
	}
	if id.Env != "" {
		// The key Grafana's service graph and most dashboards group by.
		attrs = append(attrs, attribute.String("deployment.environment", id.Env))
	}
	host := id.Host
	if host == "" {
		host, _ = os.Hostname()
	}
	if host != "" {
		attrs = append(attrs, semconv.HostName(host))
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, fmt.Errorf("otelx: resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.sampleRatio()))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// TraceID returns the trace id of the span in ctx as 32 hex characters, or ""
// when ctx carries no span (tracing off, or a context that did not come
// through a kitexx / hertzx middleware).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// WithRemoteTraceID gives ctx a remote parent whose trace id is id, so that
// the span started next continues that trace: it is how a caller that sends
// only an id (X-Trace-Id, the TRACE_ID metainfo value), not a W3C
// traceparent, still ends up in the same trace. id must be 32 hex characters;
// anything else leaves ctx as it is and returns false. The parent span id is
// made up (the caller did not say), and the trace is marked sampled, because
// a caller that bothers to send an id wants to find it again.
func WithRemoteTraceID(ctx context.Context, id string) (context.Context, bool) {
	tid, err := trace.TraceIDFromHex(id)
	if err != nil || !tid.IsValid() {
		return ctx, false
	}
	var sid trace.SpanID
	copy(sid[:], tid[8:])
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(ctx, sc), true
}
