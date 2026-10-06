package kitexx

import (
	"context"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/sezznaw/devkit-common/otelx"
)

// ServerTracing starts a server span for every RPC, continuing the trace the
// caller sent (W3C traceparent in the TTHeader, or only a TRACE_ID), and puts
// it into the context, so that LoggingMiddleware logs its trace id and the
// clients a handler calls with that context make child spans of it. It is the
// first middleware in Options; with tracing off (otel.endpoint empty) the span
// records nothing.
func ServerTracing() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			ctx = otel.GetTextMapPropagator().Extract(ctx, metainfoCarrier{ctx: &ctx})
			if !trace.SpanContextFromContext(ctx).IsValid() {
				if id, ok := metainfo.GetPersistentValue(ctx, TraceIDKey); ok {
					ctx, _ = otelx.WithRemoteTraceID(ctx, id)
				}
			}
			service, method, from := rpcNames(ctx)
			attrs := []attribute.KeyValue{
				attribute.String("rpc.system", "kitex"),
				attribute.String("rpc.service", service),
				semconv.RPCMethod(method),
			}
			if from != "" {
				attrs = append(attrs, attribute.String("peer.service", from))
			}
			ctx, span := otelx.Tracer().Start(ctx, service+"/"+method,
				trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
			defer span.End()

			err := next(ctx, req, resp)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return err
		}
	}
}

// ClientTracing starts a client span around every call and puts the W3C
// traceparent into the TTHeader of the request, which is how the service
// called continues the trace. It is part of ClientOptions.
func ClientTracing() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			service, method, _ := rpcNames(ctx)
			ctx, span := otelx.Tracer().Start(ctx, service+"/"+method,
				trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
					attribute.String("rpc.system", "kitex"),
					attribute.String("rpc.service", service),
					semconv.RPCMethod(method)))
			defer span.End()
			otel.GetTextMapPropagator().Inject(ctx, metainfoCarrier{ctx: &ctx})

			err := next(ctx, req, resp)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return err
		}
	}
}

// rpcNames reads what the span is named after from the RPCInfo: the service
// handling the call, the method, and who is calling ("" when not known).
func rpcNames(ctx context.Context) (service, method, from string) {
	service, method = "unknown", "unknown"
	ri := rpcinfo.GetRPCInfo(ctx)
	if ri == nil {
		return
	}
	if ri.To() != nil && ri.To().ServiceName() != "" {
		service = ri.To().ServiceName()
	}
	if ri.Invocation() != nil && ri.Invocation().MethodName() != "" {
		method = ri.Invocation().MethodName()
	}
	if ri.From() != nil {
		from = ri.From().ServiceName()
	}
	return
}

// metainfoCarrier lets the OpenTelemetry propagator read and write the
// transient metainfo values of a context, which the TTHeader transport carries
// for one hop: exactly the life of a traceparent. Set has to replace the
// context (metainfo is immutable), hence the pointer.
type metainfoCarrier struct{ ctx *context.Context }

func (c metainfoCarrier) Get(key string) string {
	v, _ := metainfo.GetValue(*c.ctx, key)
	return v
}

func (c metainfoCarrier) Set(key, value string) {
	*c.ctx = metainfo.WithValue(*c.ctx, key, value)
}

func (c metainfoCarrier) Keys() []string {
	var keys []string
	metainfo.RangeValues(*c.ctx, func(k, _ string) bool {
		keys = append(keys, k)
		return true
	})
	return keys
}
