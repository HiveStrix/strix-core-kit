package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

func tokenServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id, secret, ok := r.BasicAuth()
		if !ok || id != "core-costing" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("form: %v", err)
		}
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "core.read" || r.Form.Get("audience") != "core-expenses" {
			t.Errorf("unexpected form: %v", r.Form)
		}
		tenant := r.Form.Get("tenant_id")
		if tenant == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "error_description": "tenant_id is required"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + tenant, "token_type": "Bearer", "expires_in": 300})
	}))
}

func TestServiceTokensMintPerTenantAndCache(t *testing.T) {
	var calls atomic.Int64
	srv := tokenServer(t, &calls)
	defer srv.Close()

	ts := NewServiceTokens(ServiceCredentials{TokenURL: srv.URL, ClientID: "core-costing", ClientSecret: "s3cret", Scope: "core.read"})
	if ts == nil {
		t.Fatal("configured credentials must build a source")
	}
	now := time.Now()
	ts.now = func() time.Time { return now }

	tok, err := ts.Token(context.Background(), "acme", "core-expenses")
	if err != nil || tok != "tok-acme" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	if tok, _ := ts.Token(context.Background(), "acme", "core-expenses"); tok != "tok-acme" || calls.Load() != 1 {
		t.Fatalf("second call must come from the cache: calls = %d", calls.Load())
	}
	if tok, _ := ts.Token(context.Background(), "globex", "core-expenses"); tok != "tok-globex" || calls.Load() != 2 {
		t.Fatalf("another tenant is another token: tok = %q, calls = %d", tok, calls.Load())
	}
	// 30 s before expiry the token is minted again.
	now = now.Add(4*time.Minute + 45*time.Second)
	if _, err := ts.Token(context.Background(), "acme", "core-expenses"); err != nil || calls.Load() != 3 {
		t.Fatalf("expired token must be re-minted: err = %v, calls = %d", err, calls.Load())
	}
	if _, err := ts.Token(context.Background(), "", "core-expenses"); err == nil {
		t.Fatal("a service token without tenant must be refused before calling the server")
	}
}

func TestServiceTokensRefusedIsAnError(t *testing.T) {
	var calls atomic.Int64
	srv := tokenServer(t, &calls)
	defer srv.Close()
	ts := NewServiceTokens(ServiceCredentials{TokenURL: srv.URL, ClientID: "core-costing", ClientSecret: "wrong", Scope: "core.read"})
	if _, err := ts.Token(context.Background(), "acme", "core-expenses"); err == nil {
		t.Fatal("an invalid_client answer must surface as an error")
	}
	if NewServiceTokens(ServiceCredentials{}) != nil {
		t.Fatal("unconfigured credentials must yield a nil source")
	}
}

func TestOutgoingForwardsTheCallerAndFallsBackToTheService(t *testing.T) {
	var calls atomic.Int64
	srv := tokenServer(t, &calls)
	defer srv.Close()
	ts := NewServiceTokens(ServiceCredentials{TokenURL: srv.URL, ClientID: "core-costing", ClientSecret: "s3cret", Scope: "core.read"})

	// A person's request: her bearer travels, no service token is minted.
	in := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer user-token"))
	out, err := Outgoing(in, ts, "core-expenses")
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(out)
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer user-token" || calls.Load() != 0 {
		t.Fatalf("outgoing = %v, calls = %d; the caller's bearer must be forwarded untouched", got, calls.Load())
	}

	// Nobody behind the call (an event consumer): the service token, for the tenant in context.
	ctx := tenantctx.WithTenant(context.Background(), "acme")
	out, err = Outgoing(ctx, ts, "core-expenses")
	if err != nil {
		t.Fatal(err)
	}
	md, _ = metadata.FromOutgoingContext(out)
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer tok-acme" {
		t.Fatalf("outgoing = %v, want the service token", got)
	}

	// No credentials: the context goes out as it came; the callee will say so.
	out, err = Outgoing(ctx, nil, "core-expenses")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata.FromOutgoingContext(out); ok {
		t.Fatal("without credentials nothing must be appended")
	}
	// No tenant either: an error, never a token for nobody.
	if _, err := Outgoing(context.Background(), ts, "core-expenses"); err == nil {
		t.Fatal("a consumer without tenant in context cannot get a token")
	}
}

func TestClaimsServiceHelpers(t *testing.T) {
	machine := &Claims{Subject: "core-costing", ClientID: "core-costing", TenantID: "acme", Scope: "core.read"}
	person := &Claims{Subject: "u-1", ClientID: "strix-shell", TenantID: "acme", Scope: "openid"}
	if !machine.IsService() || person.IsService() {
		t.Fatal("a token is a service token exactly when sub equals client_id")
	}
	if !machine.HasScope("core.read") || machine.HasScope("core.write") || person.HasScope("core.read") {
		t.Fatal("HasScope must match whole scope words")
	}
	var nilClaims *Claims
	if nilClaims.IsService() || nilClaims.HasScope("core.read") {
		t.Fatal("nil claims are nobody")
	}
}
