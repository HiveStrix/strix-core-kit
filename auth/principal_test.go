package auth

import (
	"context"
	"testing"
)

// An agent token, as protocols mints it (contract AI ready §1.1): sub ==
// client_id (one client per installation) plus the agent claims. Every claim
// must reach Claims, and the agent must NOT read as a service, or the gate
// would authorize it by scope.
func TestVerifyReadsTheAgentClaims(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	raw := s.token(t, claimSet{subject: "agent-inst-7", tenantID: "acme", extra: map[string]any{
		"client_id":       "agent-inst-7",
		"principal_type":  "agent",
		"installation_id": "inst-7",
		"agent_id":        "collections",
		"agent_version":   "1.4.0",
		"provider_id":     "hivestrix",
		"origin":          "marketplace",
		"tier":            "certified",
		"act":             map[string]any{"sub": "user-9"},
	}})
	c, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.PrincipalType() != PrincipalAgent || !c.IsAgent() {
		t.Fatalf("PrincipalType = %q, want agent", c.PrincipalType())
	}
	if c.IsService() {
		t.Fatal("an agent token has sub == client_id but must not be a service")
	}
	got := []string{c.InstallationID, c.AgentID, c.AgentVersion, c.ProviderID, c.Origin, c.Tier, c.ActSubject}
	want := []string{"inst-7", "collections", "1.4.0", "hivestrix", "marketplace", "certified", "user-9"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agent claims = %v, want %v", got, want)
		}
	}
}

func TestVerifyReadsVia(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	raw := s.token(t, claimSet{tenantID: "acme", extra: map[string]any{
		"client_id": "strix-assistant", "principal_type": "user", "via": "assistant",
	}})
	c, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Via() != ViaAssistant {
		t.Fatalf("Via() = %q, want assistant", c.Via())
	}
	if c.PrincipalType() != PrincipalUser || c.IsService() || c.IsAgent() {
		t.Fatalf("an assistant token is still the person: type %q", c.PrincipalType())
	}
}

// The claim wins when present; without it the old heuristic stands, so a
// token minted before protocols stamped principal_type keeps working.
func TestPrincipalType(t *testing.T) {
	cases := []struct {
		name    string
		c       *Claims
		want    string
		service bool
	}{
		{"legacy machine", &Claims{Subject: "core-costing", ClientID: "core-costing"}, PrincipalService, true},
		{"legacy person", &Claims{Subject: "u-1", ClientID: "strix-shell"}, PrincipalUser, false},
		{"legacy no client_id", &Claims{Subject: "u-1"}, PrincipalUser, false},
		{"stamped service", &Claims{Subject: "core-costing", ClientID: "core-costing", PrincipalTypeClaim: "service"}, PrincipalService, true},
		{"stamped user", &Claims{Subject: "u-1", ClientID: "strix-shell", PrincipalTypeClaim: "user"}, PrincipalUser, false},
		{"agent with sub == client_id", &Claims{Subject: "agent-1", ClientID: "agent-1", PrincipalTypeClaim: "agent"}, PrincipalAgent, false},
		{"user claim beats the heuristic", &Claims{Subject: "x", ClientID: "x", PrincipalTypeClaim: "user"}, PrincipalUser, false},
		{"unknown type stays unknown", &Claims{Subject: "x", ClientID: "x", PrincipalTypeClaim: "robot"}, "robot", false},
	}
	for _, tc := range cases {
		if got := tc.c.PrincipalType(); got != tc.want {
			t.Errorf("%s: PrincipalType = %q, want %q", tc.name, got, tc.want)
		}
		if got := tc.c.IsService(); got != tc.service {
			t.Errorf("%s: IsService = %v, want %v", tc.name, got, tc.service)
		}
	}
	var nilClaims *Claims
	if nilClaims.PrincipalType() != "" || nilClaims.IsAgent() || nilClaims.Via() != "" {
		t.Fatal("nil claims are nobody")
	}
}
