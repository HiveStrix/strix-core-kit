package authz

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

// agentClaims is an agent token as protocols mints it (contract AI ready
// §1.1): sub == client_id, one client per installation, the agent claims, and
// — the case that must not matter — a scope that would open the service path.
func agentClaims() *auth.Claims {
	return &auth.Claims{
		Subject: "agent-inst-7", ClientID: "agent-inst-7", TenantID: "acme",
		PrincipalTypeClaim: "agent", InstallationID: "inst-7",
		Origin: "marketplace", Tier: "certified", ProviderID: "hivestrix",
		Entitlements: []string{"billing"},
		Scope:        "core.read core.write",
	}
}

func assistantClaims() *auth.Claims {
	return &auth.Claims{
		Subject: "user-1", ClientID: "strix-assistant", TenantID: "acme",
		PrincipalTypeClaim: "user", ViaClaim: "assistant",
		Entitlements: []string{"billing"},
	}
}

func serviceClaims(scope string) *auth.Claims {
	return &auth.Claims{Subject: "jev", ClientID: "jev", TenantID: "acme", PrincipalTypeClaim: "service", Scope: scope}
}

func with(c *auth.Claims, edit func(*auth.Claims)) context.Context {
	cp := *c
	if edit != nil {
		edit(&cp)
	}
	return auth.ContextWithClaims(context.Background(), &cp)
}

// The table every principal type and via combination is judged by. pdp is
// what the fake PDP answers; calls is how many times it must have been asked.
func TestGateByPrincipalType(t *testing.T) {
	catalog := map[string]Effect{
		"billing.invoice.export": EffectRead,  // a read whose verb is not one
		"billing.invoice.list":   EffectWrite, // the catalog beats the verb
		"billing.invoice.send":   EffectExternalSideEffect,
		"billing.invoice.delete": EffectDelete,
	}
	cases := []struct {
		name    string
		ctx     context.Context
		action  string
		effects map[string]Effect
		allow   []string // MachineAllow for the action, if any
		pdp     bool
		want    codes.Code
		calls   int
	}{
		// Agents: entitlement + PDP, never scope.
		{"agent allowed by the PDP", with(agentClaims(), nil), "billing.invoice.read", nil, nil, true, codes.OK, 1},
		{"agent denied by the PDP despite core.write", with(agentClaims(), nil), "billing.invoice.write", nil, nil, false, codes.PermissionDenied, 1},
		{"agent denied by the PDP despite core.read", with(agentClaims(), nil), "billing.invoice.read", nil, nil, false, codes.PermissionDenied, 1},
		{"agent of an unentitled tenant", with(agentClaims(), func(c *auth.Claims) { c.Entitlements = []string{"costing"} }), "billing.invoice.read", nil, nil, true, codes.PermissionDenied, 0},
		{"agent without installation_id", with(agentClaims(), func(c *auth.Claims) { c.InstallationID = "" }), "billing.invoice.read", nil, nil, true, codes.PermissionDenied, 0},
		{"agent ignores the machine allowlist", with(agentClaims(), nil), "billing.invoice.read", nil, []string{"someone-else"}, true, codes.OK, 1},
		{"agent with the allowlist still needs the PDP", with(agentClaims(), nil), "billing.invoice.read", nil, []string{"agent-inst-7"}, false, codes.PermissionDenied, 1},

		// via: read-only before the PDP, then as the person.
		{"assistant reads by verb", with(assistantClaims(), nil), "billing.invoice.list", nil, nil, true, codes.OK, 1},
		{"assistant get by verb", with(assistantClaims(), nil), "billing.invoice.get", nil, nil, true, codes.OK, 1},
		{"assistant read still needs the PDP", with(assistantClaims(), nil), "billing.invoice.read", nil, nil, false, codes.PermissionDenied, 1},
		{"assistant cannot write", with(assistantClaims(), nil), "billing.invoice.write", nil, nil, true, codes.PermissionDenied, 0},
		{"assistant cannot admin", with(assistantClaims(), nil), "billing.taxrate.admin", nil, nil, true, codes.PermissionDenied, 0},
		// ScopeFor counts these as reads; the assistant rule must not.
		{"assistant lookup is not a read", with(assistantClaims(), nil), "billing.client.lookup", nil, nil, true, codes.PermissionDenied, 0},
		{"assistant search is not a read", with(assistantClaims(), nil), "billing.invoice.search", nil, nil, true, codes.PermissionDenied, 0},
		{"assistant view is not a read", with(assistantClaims(), nil), "billing.report.view", nil, nil, true, codes.PermissionDenied, 0},
		// With a catalog, the effect decides.
		{"catalog read with a write-ish verb", with(assistantClaims(), nil), "billing.invoice.export", catalog, nil, true, codes.OK, 1},
		{"catalog write with a read verb", with(assistantClaims(), nil), "billing.invoice.list", catalog, nil, true, codes.PermissionDenied, 0},
		{"catalog external side effect", with(assistantClaims(), nil), "billing.invoice.send", catalog, nil, true, codes.PermissionDenied, 0},
		{"catalog delete", with(assistantClaims(), nil), "billing.invoice.delete", catalog, nil, true, codes.PermissionDenied, 0},
		{"uncatalogued action with a read verb", with(assistantClaims(), nil), "billing.invoice.get", catalog, nil, true, codes.PermissionDenied, 0},
		{"unknown via is held read-only too", with(assistantClaims(), func(c *auth.Claims) { c.ViaClaim = "copilot" }), "billing.invoice.write", nil, nil, true, codes.PermissionDenied, 0},
		{"agent through the assistant cannot write", with(agentClaims(), func(c *auth.Claims) { c.ViaClaim = "assistant" }), "billing.invoice.write", nil, nil, true, codes.PermissionDenied, 0},
		{"service through the assistant cannot write", with(serviceClaims("core.write"), func(c *auth.Claims) { c.ViaClaim = "assistant" }), "billing.invoice.write", nil, nil, true, codes.PermissionDenied, 0},
		{"service through the assistant reads by scope", with(serviceClaims("core.read"), func(c *auth.Claims) { c.ViaClaim = "assistant" }), "billing.invoice.read", nil, nil, false, codes.OK, 0},

		// Services: scope, plus the exact action as a scope (§6.1).
		{"exact action scope", with(serviceClaims("core.read billing.document.classify"), nil), "billing.document.classify", nil, nil, false, codes.OK, 0},
		{"exact action scope does not reach a sibling", with(serviceClaims("billing.document.classify"), nil), "billing.document.delete", nil, nil, true, codes.PermissionDenied, 0},
		{"exact action scope does not reach a prefix", with(serviceClaims("billing.document"), nil), "billing.document.classify", nil, nil, true, codes.PermissionDenied, 0},
		{"exact action scope is not core.read", with(serviceClaims("billing.document.classify"), nil), "billing.document.read", nil, nil, true, codes.PermissionDenied, 0},
		{"exact action scope, not allowlisted", with(serviceClaims("billing.document.classify"), nil), "billing.document.classify", nil, []string{"other"}, true, codes.PermissionDenied, 0},
		{"exact action scope, allowlisted", with(serviceClaims("billing.document.classify"), nil), "billing.document.classify", nil, []string{"jev"}, false, codes.OK, 0},
		{"legacy service, no principal_type", with(serviceClaims("core.read"), func(c *auth.Claims) { c.PrincipalTypeClaim = "" }), "billing.invoice.read", nil, nil, false, codes.OK, 0},

		// People.
		{"person allowed", with(assistantClaims(), func(c *auth.Claims) { c.ViaClaim = "" }), "billing.invoice.write", nil, nil, true, codes.OK, 1},
		{"person stamped user with sub == client_id goes to the PDP", with(serviceClaims("core.write"), func(c *auth.Claims) {
			c.PrincipalTypeClaim = "user"
			c.Entitlements = []string{"billing"}
		}), "billing.invoice.write", nil, nil, false, codes.PermissionDenied, 1},
		{"unknown principal type", with(serviceClaims("core.write"), func(c *auth.Claims) {
			c.PrincipalTypeClaim = "robot"
			c.Entitlements = []string{"billing"}
		}), "billing.invoice.read", nil, nil, true, codes.PermissionDenied, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakePDP{allowed: tc.pdp}
			base := newGate(t, f, "billing")
			var opts []Option
			if tc.effects != nil {
				opts = append(opts, Effects(tc.effects))
			}
			if tc.allow != nil {
				opts = append(opts, MachineAllow(tc.action, tc.allow...))
			}
			g := New(base.pdp, "billing", opts...)
			err := g.Require(tc.ctx, tc.action, "invoice", "inv-1")
			if status.Code(err) != tc.want {
				t.Fatalf("code = %v (%v), want %v", status.Code(err), err, tc.want)
			}
			if _, calls := f.recorded(); calls != tc.calls {
				t.Fatalf("PDP calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}

// What the PDP receives for an agent: judged as the agent, subject = the
// installation, with the attributes of its signed token and nothing else.
func TestAgentReachesThePDPAsAnAgent(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "billing")
	if err := g.Require(with(agentClaims(), nil), "billing.invoice.read", "invoice", "inv-1"); err != nil {
		t.Fatalf("Require: %v", err)
	}
	req, _ := f.recorded()
	if req.GetSubjectType() != "agent" || req.GetSubjectId() != "inst-7" || req.GetTenantId() != "acme" {
		t.Fatalf("subject = (%q, %q, %q), want (agent, inst-7, acme)", req.GetSubjectType(), req.GetSubjectId(), req.GetTenantId())
	}
	want := map[string]string{"origin": "marketplace", "tier": "certified", "provider": "hivestrix", "installation_id": "inst-7"}
	got := req.GetPrincipalAttributes()
	if len(got) != len(want) {
		t.Fatalf("principal_attributes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("principal_attributes = %v, want %v", got, want)
		}
	}
	if len(req.GetContext()) != 0 || req.GetVia() != "" {
		t.Fatalf("context = %v, via = %q; want neither", req.GetContext(), req.GetVia())
	}
	if len(req.GetEntitlements()) != 1 || req.GetEntitlements()[0] != "billing" {
		t.Fatalf("entitlements = %v", req.GetEntitlements())
	}

	// A missing claim is left out, never sent as "".
	if err := g.Require(with(agentClaims(), func(c *auth.Claims) { c.Tier = ""; c.Origin = "" }), "billing.invoice.read", "invoice", "inv-1"); err != nil {
		t.Fatalf("Require: %v", err)
	}
	req, _ = f.recorded()
	if _, has := req.GetPrincipalAttributes()["tier"]; has {
		t.Fatalf("an empty tier was sent: %v", req.GetPrincipalAttributes())
	}
	if _, has := req.GetPrincipalAttributes()["origin"]; has {
		t.Fatalf("an empty origin was sent: %v", req.GetPrincipalAttributes())
	}
}

// What the PDP receives through the assistant: the person, with via.
func TestAssistantReachesThePDPAsThePersonWithVia(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "billing")
	if err := g.Require(with(assistantClaims(), nil), "billing.invoice.list", "invoice", ""); err != nil {
		t.Fatalf("Require: %v", err)
	}
	req, _ := f.recorded()
	if req.GetSubjectId() != "user-1" || req.GetVia() != "assistant" || req.GetSubjectType() != "" {
		t.Fatalf("subject=%q via=%q type=%q, want user-1, assistant, \"\"", req.GetSubjectId(), req.GetVia(), req.GetSubjectType())
	}
	if len(req.GetPrincipalAttributes()) != 0 {
		t.Fatalf("a person carries no principal attributes: %v", req.GetPrincipalAttributes())
	}
}

// A person without via sends exactly what it sent before v0.20: no
// subject_type, no via, no attributes.
func TestPersonRequestIsUnchanged(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "expenses")
	if err := g.Require(ctxWith("expenses"), "expenses.catalog.write", "item", "i-1"); err != nil {
		t.Fatalf("Require: %v", err)
	}
	req, _ := f.recorded()
	if req.GetSubjectType() != "" || req.GetVia() != "" || len(req.GetPrincipalAttributes()) != 0 {
		t.Fatalf("person request changed: type=%q via=%q attrs=%v", req.GetSubjectType(), req.GetVia(), req.GetPrincipalAttributes())
	}
}

// The assistant's denial says why; it is not a secret that the assistant only
// reads.
func TestAssistantDenialSaysWhy(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "billing")
	err := g.Require(with(assistantClaims(), nil), "billing.invoice.write", "invoice", "")
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "assistant may only read") {
		t.Fatalf("message = %q", msg)
	}
}

func TestEffectsRejectsAnUnknownEffect(t *testing.T) {
	if msg := panicMessage(func() { New(nil, "billing", Effects(map[string]Effect{"billing.invoice.list": "reed"})) }); !strings.HasPrefix(msg, "authz: Effects") {
		t.Fatalf("New must panic on an unknown effect, got %q", msg)
	}
	// The map is copied: editing it afterwards does not change the gate.
	m := map[string]Effect{"billing.invoice.export": EffectRead}
	f := &fakePDP{allowed: true}
	base := newGate(t, f, "billing")
	g := New(base.pdp, "billing", Effects(m))
	m["billing.invoice.export"] = EffectWrite
	if err := g.Require(with(assistantClaims(), nil), "billing.invoice.export", "invoice", ""); err != nil {
		t.Fatalf("mutating the caller's map changed the gate: %v", err)
	}
}
