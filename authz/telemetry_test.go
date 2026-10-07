package authz

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

// The gate stamps the four standard attributes on the RPC's span, the
// decision included, and nothing else.
func TestRequireRecordsTheDecisionOnTheSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	f := &fakePDP{allowed: true}
	g := newGate(t, f, "billing")
	run := func(c *auth.Claims, action string) map[string]string {
		ctx, span := tp.Tracer("t").Start(context.Background(), "rpc")
		_ = g.Require(auth.ContextWithClaims(ctx, c), action, "invoice", "inv-1")
		span.End()
		ended := rec.Ended()
		out := map[string]string{}
		for _, a := range ended[len(ended)-1].Attributes() {
			out[string(a.Key)] = a.Value.Emit()
		}
		return out
	}

	got := run(agentClaims(), "billing.invoice.read")
	want := map[string]string{"tenant_id": "acme", "principal_type": "agent", "action": "billing.invoice.read", "decision": "allow"}
	if len(got) != 4 {
		t.Fatalf("attributes = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("attributes = %v, want %v", got, want)
		}
	}
	// A legacy service token (no principal_type) is recorded as what the
	// gate judged it, not as the missing claim.
	legacy := &auth.Claims{Subject: "core-x", ClientID: "core-x", TenantID: "acme", Scope: "core.read"}
	if got := run(legacy, "billing.invoice.read"); got["principal_type"] != "service" || got["decision"] != "allow" {
		t.Fatalf("legacy service: %v", got)
	}
	if got := run(assistantClaims(), "billing.invoice.write"); got["decision"] != "deny" || got["principal_type"] != "user" {
		t.Fatalf("denied assistant write: %v", got)
	}
}
