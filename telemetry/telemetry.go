// Package telemetry is the Core's OpenTelemetry wiring (proposal §4.6):
// traces exported by OTLP to the platform's gateway, the gRPC handlers that
// open a span per RPC and carry the trace across Cores, and the four
// attributes every span may carry — tenant_id, principal_type, action and
// decision. Beyla keeps covering whatever does not use the kit.
//
// NEVER BUSINESS CONTENT. A trace is read by whoever operates the platform,
// across every tenant, and kept for days. The package only offers setters for
// those four identifiers; there is deliberately no helper to attach a request
// body, an amount or a name, and a Core that needs one is asking for a log
// line in its own tenant's scope, not a span attribute.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

// The attribute keys the platform standardises (proposal §4.6). They are the
// only ones the package writes.
const (
	AttrTenantID      = "tenant_id"
	AttrPrincipalType = "principal_type"
	AttrAction        = "action"
	AttrDecision      = "decision"
)

// Decisions the gate records.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// propagator is W3C trace context only. Baggage is left out on purpose: it
// travels to every downstream service and into their spans, which is exactly
// where business content must not go.
var propagator = propagation.TraceContext{}

// Setup installs the OpenTelemetry SDK for serviceName and returns the
// function that flushes and stops it on shutdown (call it with a bounded
// context after the gRPC server stops).
//
// The exporter is OTLP over HTTP/protobuf (the OTLP default, what the
// platform's gateway and Beyla speak on :4318) and is configured by the
// standard variables: OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS, and the
// sampler by OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG (default: parent
// based, always on). OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override
// and extend the resource.
//
// With neither endpoint variable set, or OTEL_SDK_DISABLED=true, Setup
// installs nothing but the propagator and returns a no-op shutdown: a Core
// running where no gateway exists (a laptop, CI) must not spend a goroutine
// retrying exports nobody receives. The propagator is installed in both
// cases, so a Core with export off still passes an incoming trace on to the
// Cores it calls.
func Setup(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagator)
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}
	if serviceName == "" {
		return noop, errors.New("telemetry: service name is required")
	}

	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, fmt.Errorf("telemetry: otlp exporter: %w", err)
	}
	res, err := resource.Merge(
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(serviceName)),
		resource.Environment(),
	)
	if err != nil {
		return noop, fmt.Errorf("telemetry: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Enabled reports whether Setup would export: an OTLP endpoint is configured
// and the SDK is not disabled.
func Enabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// ServerOption opens a server span per RPC, continuing the caller's trace
// from the incoming traceparent. otelgrpc instruments through a stats
// handler (its interceptors are gone), so this is a grpc.ServerOption, not
// an interceptor:
//
//	grpc.NewServer(telemetry.ServerOption(),
//		grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(v), telemetry.UnaryServerInterceptor()))
func ServerOption(opts ...otelgrpc.Option) grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler(opts...))
}

// DialOption opens a client span per outgoing RPC and sends the traceparent,
// so the Core called continues this trace.
func DialOption(opts ...otelgrpc.Option) grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler(opts...))
}

// UnaryServerInterceptor stamps tenant_id and principal_type from the
// verified claims on the RPC's span. It goes AFTER auth's interceptor in the
// chain (it reads what that one put in the context); action and decision are
// stamped by the gate.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if c, ok := auth.ClaimsFrom(ctx); ok {
			SetPrincipal(ctx, c.TenantID, c.PrincipalType())
		}
		return handler(ctx, req)
	}
}

// SetPrincipal puts tenant_id and principal_type on the current span. Empty
// values are left out. A no-op when the span is not recording.
func SetPrincipal(ctx context.Context, tenantID, principalType string) {
	set(ctx, AttrTenantID, tenantID, AttrPrincipalType, principalType)
}

// RecordDecision puts action and decision (DecisionAllow, DecisionDeny) on
// the current span. A no-op when the span is not recording.
func RecordDecision(ctx context.Context, action, decision string) {
	set(ctx, AttrAction, action, AttrDecision, decision)
}

func set(ctx context.Context, kv ...string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			attrs = append(attrs, attribute.String(kv[i], kv[i+1]))
		}
	}
	span.SetAttributes(attrs...)
}

// Traceparent is the W3C traceparent of the span in ctx, "" when there is
// none: what an outbox row records (outbox.Meta.Traceparent) so the
// consumer's work joins this trace.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// ContextWithTraceparent makes traceparent the remote parent of the spans
// started from the returned context. A malformed or empty traceparent leaves
// ctx as it was.
func ContextWithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}
