package authz

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
	authorizationv1 "github.com/hs-javierviquez/strix-core-kit/gen/authorization/v1"
	"github.com/hs-javierviquez/strix-core-kit/pdp"
)

// fakePDP answers CheckPermission with a fixed decision and records the last
// request, so a test can assert on what the gate assembled.
type fakePDP struct {
	authorizationv1.UnimplementedAuthorizationServiceServer
	allowed bool
	err     error

	mu      sync.Mutex
	lastReq *authorizationv1.CheckPermissionRequest
	calls   int
}

func (f *fakePDP) CheckPermission(_ context.Context, req *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
	f.mu.Lock()
	f.lastReq = req
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &authorizationv1.CheckPermissionResponse{Allowed: f.allowed, Reason: "fake"}, nil
}

func (f *fakePDP) recorded() (*authorizationv1.CheckPermissionRequest, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq, f.calls
}

func newGate(t *testing.T, f *fakePDP, module string) *Gate {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	authorizationv1.RegisterAuthorizationServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := pdp.Dial(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(c, module)
}

// ctxWith builds the context the interceptor would have produced.
func ctxWith(entitlements ...string) context.Context {
	return auth.ContextWithClaims(context.Background(), &auth.Claims{
		Subject:      "user-1",
		TenantID:     "acme",
		Entitlements: entitlements,
	})
}

func TestRequireAllowsWhatThePDPAllows(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "expenses")

	if err := g.Require(ctxWith("expenses"), "expenses.catalog.read", "item", "item-9"); err != nil {
		t.Fatalf("Require denied an allowed action: %v", err)
	}

	req, _ := f.recorded()
	if req.GetTenantId() != "acme" || req.GetSubjectId() != "user-1" {
		t.Errorf("identity = (%q, %q), want (acme, user-1)", req.GetTenantId(), req.GetSubjectId())
	}
	if len(req.GetContext()) != 0 {
		t.Errorf("context = %v, want empty — the PDP derives those keys", req.GetContext())
	}
}

func TestRequireDeniesWhatThePDPDenies(t *testing.T) {
	f := &fakePDP{allowed: false}
	g := newGate(t, f, "expenses")

	err := g.Require(ctxWith("expenses"), "expenses.catalog.write", "item", "")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", status.Code(err))
	}
}

// FAIL-CLOSED: an unreachable or erroring PDP denies.
func TestRequireDeniesWhenThePDPErrors(t *testing.T) {
	f := &fakePDP{err: status.Error(codes.Unavailable, "down")}
	g := newGate(t, f, "expenses")

	err := g.Require(ctxWith("expenses"), "expenses.catalog.write", "item", "")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", status.Code(err))
	}
}

// The entitlement check runs BEFORE the PDP: a tenant that does not own the
// module gets the same answer whatever the policy would say, and asking first
// would leak that the resource exists.
func TestRequireChecksEntitlementBeforeCallingThePDP(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "expenses")

	err := g.Require(ctxWith("costing"), "expenses.catalog.read", "item", "")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", status.Code(err))
	}
	if _, calls := f.recorded(); calls != 0 {
		t.Errorf("the PDP was called %d times for an unentitled tenant, want 0", calls)
	}
}

// An RPC wired outside the interceptor chain reaches the gate with no claims.
// That is a deployment mistake, and it must not pass.
func TestRequireRejectsAContextWithNoClaims(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "expenses")

	err := g.Require(context.Background(), "expenses.catalog.read", "item", "")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", status.Code(err))
	}
	if _, calls := f.recorded(); calls != 0 {
		t.Errorf("the PDP was called %d times with no claims, want 0", calls)
	}
}

// An action whose module segment is not this Core's is a naming mistake, and
// the kind that hides: the PDP would happily judge it against another module's
// rules. It fails loudly rather than denying for a reason nobody can see.
func TestRequireRejectsAnActionFromAnotherModule(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := newGate(t, f, "expenses")

	err := g.Require(ctxWith("expenses"), "clients.delete", "client", "")
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
	if _, calls := f.recorded(); calls != 0 {
		t.Errorf("the PDP was called %d times for a foreign action, want 0", calls)
	}
}
