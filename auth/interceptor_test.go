package auth

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// invoke runs the interceptor over a handler that records the context it was
// called with, so a test can assert on what the interceptor injected.
func invoke(t *testing.T, v *Verifier, method string, md metadata.MD) (context.Context, error) {
	t.Helper()
	ctx := context.Background()
	if md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	var seen context.Context
	handler := func(ctx context.Context, _ any) (any, error) {
		seen = ctx
		return "ok", nil
	}
	_, err := UnaryServerInterceptor(v)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
	return seen, err
}

func bearer(token string) metadata.MD {
	return metadata.Pairs("authorization", "Bearer "+token)
}

// A valid token is the one path that reaches the handler, and when it does the
// handler must find both the claims and a seeded tenantctx — the store reads
// the latter to pick the tenant's database, so a handler physically cannot
// query without having gone through verification.
func TestInterceptorInjectsTheVerifiedIdentity(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	raw := s.token(t, claimSet{tenantID: "acme", entitles: []string{"expenses"}})
	ctx, err := invoke(t, v, "/expenses.v1.CatalogService/ListItems", bearer(raw))
	if err != nil {
		t.Fatalf("interceptor rejected a valid token: %v", err)
	}

	claims, ok := ClaimsFrom(ctx)
	if !ok {
		t.Fatal("handler context carries no claims")
	}
	if claims.TenantID != "acme" {
		t.Errorf("claims.TenantID = %q, want acme", claims.TenantID)
	}
	if got := tenantctx.Tenant(ctx); got != "acme" {
		t.Errorf("tenantctx.Tenant = %q, want acme", got)
	}
	if got := tenantctx.Subject(ctx); got != "user-1" {
		t.Errorf("tenantctx.Subject = %q, want user-1", got)
	}
	if got := tenantctx.Entitlements(ctx); len(got) != 1 || got[0] != "expenses" {
		t.Errorf("tenantctx.Entitlements = %v, want [expenses]", got)
	}
}

func TestInterceptorRejects(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	cases := []struct {
		name string
		md   metadata.MD
	}{
		{"no metadata at all", nil},
		{"no authorization header", metadata.Pairs("x-other", "value")},
		{"not a Bearer token", metadata.Pairs("authorization", "Basic dXNlcjpwYXNz")},
		{"an invalid token", bearer("not.a.token")},
		{"a token with no tenant_id", bearer(s.token(t, claimSet{}))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := invoke(t, v, "/expenses.v1.CatalogService/ListItems", tc.md)
			if err == nil {
				t.Fatal("interceptor let the call through, want Unauthenticated")
			}
			if code := status.Code(err); code != codes.Unauthenticated {
				t.Errorf("code = %v, want Unauthenticated", code)
			}
			if ctx != nil {
				t.Error("the handler ran despite the rejection")
			}
		})
	}
}

// The health probe carries no user identity, and requiring one would make the
// probe fail whenever the auth service is briefly unreachable — which would
// take the Core out of the load balancer for an outage that is not its own.
func TestInterceptorExemptsTheHealthCheck(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	ctx, err := invoke(t, v, "/grpc.health.v1.Health/Check", nil)
	if err != nil {
		t.Fatalf("health check rejected: %v", err)
	}
	if ctx == nil {
		t.Fatal("health handler did not run")
	}
	if _, ok := ClaimsFrom(ctx); ok {
		t.Error("health context carries claims, want none")
	}
}
