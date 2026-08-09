package pdp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	authorizationv1 "github.com/hs-javierviquez/strix-core-kit/gen/authorization/v1"
)

// fakePDP stands in for the authorization service. handler decides what each
// CheckPermission call does, including hanging.
type fakePDP struct {
	authorizationv1.UnimplementedAuthorizationServiceServer
	handler func(context.Context, *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error)

	// The server runs on its own goroutine, so the recorded request crosses a
	// goroutine boundary to reach the assertions.
	mu      sync.Mutex
	lastReq *authorizationv1.CheckPermissionRequest
}

func (f *fakePDP) CheckPermission(ctx context.Context, req *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()
	return f.handler(ctx, req)
}

func (f *fakePDP) recorded() *authorizationv1.CheckPermissionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

// serve starts the fake on a local port and returns a client pointed at it.
func serve(t *testing.T, f *fakePDP, timeout time.Duration) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	authorizationv1.RegisterAuthorizationServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := DialWithTimeout(lis.Addr().String(), timeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func allow(allowed bool, reason string) func(context.Context, *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
	return func(context.Context, *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
		return &authorizationv1.CheckPermissionResponse{Allowed: allowed, Reason: reason}, nil
	}
}

func TestAllowedPassesThroughTheDecision(t *testing.T) {
	for _, want := range []bool{true, false} {
		f := &fakePDP{handler: allow(want, "because")}
		c := serve(t, f, DefaultTimeout)

		got, reason, err := c.Allowed(context.Background(), Request{Action: "clients.read"})
		if err != nil {
			t.Fatalf("Allowed returned error: %v", err)
		}
		if got != want {
			t.Errorf("allowed = %v, want %v", got, want)
		}
		if reason != "because" {
			t.Errorf("reason = %q, want %q", reason, "because")
		}
	}
}

// FAIL-CLOSED. The whole point of the gate: an RPC error is a denial, never a
// pass. A "fix" that let errors through would be invisible until someone
// noticed the PDP had been down and nothing was refused.
func TestAllowedDeniesOnAnRPCError(t *testing.T) {
	f := &fakePDP{handler: func(context.Context, *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
		return nil, status.Error(codes.Internal, "policy store is down")
	}}
	c := serve(t, f, DefaultTimeout)

	allowed, _, err := c.Allowed(context.Background(), Request{Action: "clients.delete"})
	if allowed {
		t.Error("allowed = true on an RPC error, want false")
	}
	if err == nil {
		t.Error("err = nil on an RPC error, want an error so the caller logs the outage")
	}
}

// A hung PDP must degrade into a denial rather than hold the request open.
// WaitForReady means the call would otherwise queue indefinitely, so the
// deadline is the only thing bounding it.
func TestAllowedDeniesWhenThePDPHangs(t *testing.T) {
	f := &fakePDP{handler: func(ctx context.Context, _ *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c := serve(t, f, 200*time.Millisecond)

	start := time.Now()
	allowed, _, err := c.Allowed(context.Background(), Request{Action: "clients.delete"})
	elapsed := time.Since(start)

	if allowed {
		t.Error("allowed = true against a hung PDP, want false")
	}
	if err == nil {
		t.Error("err = nil against a hung PDP, want an error")
	}
	if elapsed > time.Second {
		t.Errorf("call took %v, want it bounded near the 200ms timeout", elapsed)
	}
}

// An unreachable PDP is the case the fail-closed rule is really about: nothing
// is listening, WaitForReady queues the call, and only the deadline turns it
// into the denial the contract promises.
func TestAllowedDeniesWhenThePDPIsUnreachable(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close() // free the port so nothing answers

	c, err := DialWithTimeout(addr, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	allowed, _, err := c.Allowed(context.Background(), Request{Action: "clients.delete"})
	if allowed {
		t.Error("allowed = true against an unreachable PDP, want false")
	}
	if err == nil {
		t.Error("err = nil against an unreachable PDP, want an error")
	}
}

// The verified claims must reach the PDP exactly as the gate assembled them.
func TestAllowedForwardsTheVerifiedRequest(t *testing.T) {
	f := &fakePDP{handler: allow(true, "")}
	c := serve(t, f, DefaultTimeout)

	_, _, err := c.Allowed(context.Background(), Request{
		TenantID:     "acme",
		SubjectID:    "user-1",
		Action:       "expenses.catalog.read",
		ResourceType: "item",
		ResourceID:   "item-9",
		Entitlements: []string{"expenses"},
	})
	if err != nil {
		t.Fatalf("Allowed returned error: %v", err)
	}

	got := f.recorded()
	if got.GetTenantId() != "acme" || got.GetSubjectId() != "user-1" {
		t.Errorf("identity = (%q, %q), want (acme, user-1)", got.GetTenantId(), got.GetSubjectId())
	}
	if got.GetAction() != "expenses.catalog.read" {
		t.Errorf("action = %q", got.GetAction())
	}
	if got.GetResourceType() != "item" || got.GetResourceId() != "item-9" {
		t.Errorf("resource = (%q, %q), want (item, item-9)", got.GetResourceType(), got.GetResourceId())
	}
}

// The Core sends no context at all. The verb, module, tenant and entitlements
// the policy reads are the PDP's to derive; a Core that could restate them
// would be choosing the tier it is judged at.
func TestAllowedSendsNoContext(t *testing.T) {
	f := &fakePDP{handler: allow(true, "")}
	c := serve(t, f, DefaultTimeout)

	if _, _, err := c.Allowed(context.Background(), Request{Action: "expenses.catalog.write"}); err != nil {
		t.Fatalf("Allowed returned error: %v", err)
	}
	if got := f.recorded().GetContext(); len(got) != 0 {
		t.Errorf("context = %v, want empty", got)
	}
}
