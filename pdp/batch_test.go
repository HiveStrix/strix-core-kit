package pdp

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	authorizationv1 "github.com/hs-javierviquez/strix-core-kit/gen/authorization/v1"
)

// The AI ready fields reach the PDP exactly as the gate set them: an agent is
// judged as an agent, with the attributes its signed token carries, and the
// assistant's via travels so a policy can see it.
func TestAllowedForwardsTheAIReadyFields(t *testing.T) {
	f := &fakePDP{handler: allow(true, "")}
	c := serve(t, f, DefaultTimeout)

	_, _, err := c.Allowed(context.Background(), Request{
		TenantID: "acme", SubjectID: "inst-7", Action: "billing.invoice.list",
		SubjectType:         "agent",
		PrincipalAttributes: map[string]string{"origin": "marketplace", "tier": "certified"},
		Via:                 "assistant",
		ConsistencyToken:    "zk-42",
	})
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	got := f.recorded()
	if got.GetSubjectType() != "agent" || got.GetVia() != "assistant" || got.GetConsistencyToken() != "zk-42" {
		t.Errorf("subject_type=%q via=%q consistency_token=%q", got.GetSubjectType(), got.GetVia(), got.GetConsistencyToken())
	}
	if a := got.GetPrincipalAttributes(); a["origin"] != "marketplace" || a["tier"] != "certified" || len(a) != 2 {
		t.Errorf("principal_attributes = %v", a)
	}
	// The attributes are not smuggled into the free context.
	if len(got.GetContext()) != 0 {
		t.Errorf("context = %v, want empty", got.GetContext())
	}
}

func TestCheckReturnsDecidedAt(t *testing.T) {
	f := &fakePDP{handler: func(context.Context, *authorizationv1.CheckPermissionRequest) (*authorizationv1.CheckPermissionResponse, error) {
		return &authorizationv1.CheckPermissionResponse{Allowed: true, Reason: "r", DecidedAt: "zk-9"}, nil
	}}
	c := serve(t, f, DefaultTimeout)
	d, err := c.Check(context.Background(), Request{Action: "clients.read"})
	if err != nil || !d.Allowed || d.Reason != "r" || d.DecidedAt != "zk-9" {
		t.Fatalf("Check = %+v, %v", d, err)
	}
}

func batchOf(n int) []Request {
	out := make([]Request, n)
	for i := range out {
		out[i] = Request{TenantID: "acme", SubjectID: "u-1", Action: "billing.invoice.read", ResourceType: "invoice", ResourceID: string(rune('a' + i%26))}
	}
	return out
}

// Decisions come back in item order, one per item.
func TestBatchAllowedKeepsTheOrder(t *testing.T) {
	f := &fakePDP{handler: allow(true, ""), batch: func(_ context.Context, req *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error) {
		out := &authorizationv1.BatchCheckPermissionResponse{}
		for _, it := range req.GetItems() {
			out.Results = append(out.Results, &authorizationv1.CheckPermissionResponse{Allowed: it.GetResourceId() == "b", Reason: it.GetResourceId()})
		}
		return out, nil
	}}
	c := serve(t, f, DefaultTimeout)
	got, err := c.BatchAllowed(context.Background(), batchOf(3))
	if err != nil {
		t.Fatalf("BatchAllowed: %v", err)
	}
	if len(got) != 3 || got[0].Allowed || !got[1].Allowed || got[2].Allowed || got[0].Reason != "a" || got[2].Reason != "c" {
		t.Fatalf("decisions = %+v, want only b allowed, in order", got)
	}
}

// FAIL-CLOSED: anything but a complete answer is an error and no decisions.
func TestBatchAllowedFailsClosed(t *testing.T) {
	cases := map[string]func(context.Context, *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error){
		"rpc error": func(context.Context, *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error) {
			return nil, status.Error(codes.Internal, "down")
		},
		"fewer results than items": func(context.Context, *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error) {
			return &authorizationv1.BatchCheckPermissionResponse{Results: []*authorizationv1.CheckPermissionResponse{{Allowed: true}, {Allowed: true}}}, nil
		},
		"more results than items": func(context.Context, *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error) {
			r := []*authorizationv1.CheckPermissionResponse{{Allowed: true}, {Allowed: true}, {Allowed: true}, {Allowed: true}}
			return &authorizationv1.BatchCheckPermissionResponse{Results: r}, nil
		},
		"unimplemented (old PDP)": nil,
	}
	for name, h := range cases {
		f := &fakePDP{handler: allow(true, ""), batch: h}
		c := serve(t, f, DefaultTimeout)
		got, err := c.BatchAllowed(context.Background(), batchOf(3))
		if err == nil || got != nil {
			t.Errorf("%s: BatchAllowed = %+v, %v; want no decisions and an error", name, got, err)
		}
	}
}

// Misuse is refused before anything leaves the process.
func TestBatchAllowedRefusesMalformedBatchesLocally(t *testing.T) {
	f := &fakePDP{handler: allow(true, ""), batch: func(context.Context, *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error) {
		t.Error("the PDP was called for a malformed batch")
		return nil, nil
	}}
	c := serve(t, f, DefaultTimeout)

	mixedTenant := batchOf(2)
	mixedTenant[1].TenantID = "globex"
	mixedSubject := batchOf(2)
	mixedSubject[1].SubjectID = "u-2"
	mixedType := batchOf(2)
	mixedType[1].SubjectType = "agent"

	for name, reqs := range map[string][]Request{
		"too many":      batchOf(MaxBatch + 1),
		"mixed tenant":  mixedTenant,
		"mixed subject": mixedSubject,
		"mixed type":    mixedType,
	} {
		got, err := c.BatchAllowed(context.Background(), reqs)
		if err == nil || got != nil {
			t.Errorf("%s: BatchAllowed = %+v, %v; want an error", name, got, err)
		}
	}
	if got, err := c.BatchAllowed(context.Background(), nil); got != nil || err != nil {
		t.Errorf("empty batch = %+v, %v; want nil, nil", got, err)
	}
	if n := f.batches(); n != 0 {
		t.Errorf("BatchCheckPermission called %d times, want 0", n)
	}
	// Exactly MaxBatch is fine.
	f.batch = func(_ context.Context, req *authorizationv1.BatchCheckPermissionRequest) (*authorizationv1.BatchCheckPermissionResponse, error) {
		out := &authorizationv1.BatchCheckPermissionResponse{}
		for range req.GetItems() {
			out.Results = append(out.Results, &authorizationv1.CheckPermissionResponse{})
		}
		return out, nil
	}
	if got, err := c.BatchAllowed(context.Background(), batchOf(MaxBatch)); err != nil || len(got) != MaxBatch {
		t.Errorf("a batch of exactly MaxBatch = %d decisions, %v", len(got), err)
	}
}
