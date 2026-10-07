package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

func clearEnv(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_SDK_DISABLED", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(k, "")
	}
}

// restoreGlobals puts the global provider back after a test swaps it.
func restoreGlobals(t *testing.T) {
	tp, prop := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(prop)
	})
}

// Without an endpoint nothing is installed: no SDK, no exporter retrying
// into the void. The propagator is, so traces still pass through.
func TestSetupWithoutEndpointIsANoop(t *testing.T) {
	clearEnv(t)
	restoreGlobals(t)
	before := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), "core-test")
	if err != nil || shutdown == nil {
		t.Fatalf("Setup = %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Fatal("Setup installed a provider with no endpoint configured")
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Fatal("SDK provider installed with no endpoint")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// OTEL_SDK_DISABLED wins over an endpoint.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if Enabled() {
		t.Fatal("OTEL_SDK_DISABLED=true must disable export")
	}
}

// With an endpoint, spans reach it over OTLP/HTTP with the service name.
func TestSetupExportsToTheEndpoint(t *testing.T) {
	clearEnv(t)
	restoreGlobals(t)
	var mu sync.Mutex
	var got []*collectortrace.ExportTraceServiceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &collectortrace.ExportTraceServiceRequest{}
		if r.URL.Path == "/v1/traces" && proto.Unmarshal(body, req) == nil {
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	shutdown, err := Setup(context.Background(), "core-test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("t").Start(context.Background(), "op")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("nothing was exported")
	}
	rs := got[0].GetResourceSpans()[0]
	var service string
	for _, a := range rs.GetResource().GetAttributes() {
		if a.GetKey() == "service.name" {
			service = a.GetValue().GetStringValue()
		}
	}
	if service != "core-test" {
		t.Fatalf("service.name = %q", service)
	}
	if name := rs.GetScopeSpans()[0].GetSpans()[0].GetName(); name != "op" {
		t.Fatalf("span = %q", name)
	}
}

func recorder(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp
}

func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, a := range s.Attributes() {
		out[string(a.Key)] = a.Value.Emit()
	}
	return out
}

// The four attributes, only when set, and nothing else.
func TestAttributeSetters(t *testing.T) {
	rec, tp := recorder(t)
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	SetPrincipal(ctx, "acme", "agent")
	RecordDecision(ctx, "billing.invoice.list", DecisionDeny)
	SetPrincipal(ctx, "", "")
	span.End()
	got := attrs(rec.Ended()[0])
	want := map[string]string{"tenant_id": "acme", "principal_type": "agent", "action": "billing.invoice.list", "decision": "deny"}
	if len(got) != len(want) {
		t.Fatalf("attributes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("attributes = %v, want %v", got, want)
		}
	}
	// A context with no span is fine.
	RecordDecision(context.Background(), "x.read", DecisionAllow)
}

func TestUnaryServerInterceptorStampsThePrincipal(t *testing.T) {
	rec, tp := recorder(t)
	ctx, span := tp.Tracer("t").Start(context.Background(), "rpc")
	ctx = auth.ContextWithClaims(ctx, &auth.Claims{Subject: "x", ClientID: "x", TenantID: "acme"})
	_, err := UnaryServerInterceptor()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/a.B/C"}, func(context.Context, any) (any, error) { return nil, nil })
	span.End()
	if err != nil {
		t.Fatal(err)
	}
	got := attrs(rec.Ended()[0])
	if got["tenant_id"] != "acme" || got["principal_type"] != "service" {
		t.Fatalf("attributes = %v", got)
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := ContextWithTraceparent(context.Background(), tp)
	if got := Traceparent(ctx); got != tp {
		t.Fatalf("Traceparent = %q, want %q", got, tp)
	}
	if Traceparent(context.Background()) != "" {
		t.Fatal("no span, no traceparent")
	}
	if Traceparent(ContextWithTraceparent(context.Background(), "garbage")) != "" {
		t.Fatal("a malformed traceparent must not become a parent")
	}
}

// Across a real gRPC hop the server span continues the client's trace.
func TestGRPCPropagatesTheTrace(t *testing.T) {
	restoreGlobals(t)
	rec, tp := recorder(t)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagator)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(ServerOption())
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), DialOption())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, root := tp.Tracer("t").Start(context.Background(), "root")
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	root.End()
	time.Sleep(50 * time.Millisecond)

	var server sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind().String() == "server" {
			server = s
		}
	}
	if server == nil {
		t.Fatalf("no server span among %d", len(rec.Ended()))
	}
	if server.SpanContext().TraceID() != root.SpanContext().TraceID() {
		t.Fatal("the server span is not in the client's trace")
	}
}
