// Package pdp is the client side of the PEP<->PDP interface (SCC Appendix D
// §B, E.2): the Core validates the token itself (the PEP) and delegates the
// permission decision to the central authorization service (the PDP) over
// gRPC. The PDP never parses tokens; it only sees claims the PEP verified.
//
// Roles are NOT in the token: a role is a ReBAC Group the PDP resolves (ADR
// roles-rebac-reconciliation, SEC-auth §8.4). Anything in a Core that branches
// on a role read from a claim is a bug.
package pdp

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	authorizationv1 "github.com/hs-javierviquez/strix-core-kit/gen/authorization/v1"
)

// Request is the already-verified information the PEP hands to the PDP.
// TenantID, SubjectID and Entitlements come from the JWT claims the
// interceptor validated — never from a request body.
type Request struct {
	TenantID     string
	SubjectID    string
	Action       string
	ResourceType string
	ResourceID   string
	Entitlements []string

	// SubjectType is "" (or "user") for a person and "agent" for an AI agent,
	// whose SubjectID is then its installation_id (contract AI ready §1.2).
	SubjectType string
	// PrincipalAttributes are the agent's attributes taken from the verified
	// token (origin, tier, provider, installation_id). The PDP hands them to
	// Cedar as attributes of the Agent entity, never as free context, so they
	// cannot be used to restate a key the PDP derives.
	PrincipalAttributes map[string]string
	// Via is the token's `via` claim ("assistant" or ""). The PDP exposes it
	// as context.via so a policy can say what the assistant may not do.
	Via string
	// ConsistencyToken asks for a decision at least as fresh as the relation
	// change that returned it. Opaque: store it and send it back, never parse
	// it. Empty means "the freshest the PDP has".
	ConsistencyToken string
}

// Decision is the PDP's answer to one Request.
type Decision struct {
	Allowed bool
	Reason  string
	// DecidedAt is the token of the relation revision the decision saw, or ""
	// when it read no relations. Send it back as ConsistencyToken to get a
	// decision at least this fresh.
	DecidedAt string
}

// MaxBatch is the most items one BatchCheckPermission may carry (contract AI
// ready §1.2). BatchAllowed refuses more instead of splitting them: a caller
// checking hundreds of items one by one is a design question, not something
// to hide behind several round trips.
const MaxBatch = 100

// DefaultTimeout bounds a CheckPermission call. With WaitForReady set, a PDP
// that is down makes calls queue rather than fail, so without a deadline an
// outage would hold requests open instead of denying them.
const DefaultTimeout = 5 * time.Second

// Client calls the central authorization service's CheckPermission RPC.
type Client struct {
	conn    *grpc.ClientConn
	rpc     authorizationv1.AuthorizationServiceClient
	timeout time.Duration
}

// Dial connects to the PDP intra-cluster: plaintext (it never leaves the
// cluster), WaitForReady so a PDP restart queues calls instead of failing them,
// and keepalive to notice a dead peer. Calls are bounded by DefaultTimeout.
func Dial(addr string) (*Client, error) {
	return DialWithTimeout(addr, DefaultTimeout)
}

// DialWithTimeout is Dial with an explicit per-call deadline.
func DialWithTimeout(addr string, timeout time.Duration) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("pdp: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, rpc: authorizationv1.NewAuthorizationServiceClient(conn), timeout: timeout}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Allowed calls CheckPermission and reports whether the action is allowed.
//
// DEFAULT DENY / FAIL-CLOSED (SCC Appendix D §B.3): any transport or RPC error
// is not-allowed, never allowed. A PDP that cannot be reached denies; there is
// no fallback that lets the request through, because that fallback is exactly
// how an authorization system stops being one.
//
// A timeout bounds the call so a hung PDP degrades into a denial instead of
// holding the request open forever.
//
// No context map is sent. The verb, module, tenant and entitlements the policy
// evaluates are all derived by the PDP from what is already here, and it
// reserves those keys precisely so a Core cannot restate them and pick the tier
// it is judged at (gitops#38).
func (c *Client) Allowed(ctx context.Context, req Request) (allowed bool, reason string, err error) {
	d, err := c.Check(ctx, req)
	return d.Allowed, d.Reason, err
}

// Check is Allowed returning the whole Decision, DecidedAt included. Same
// fail-closed contract: on error the Decision is the zero value (denied).
func (c *Client) Check(ctx context.Context, req Request) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.CheckPermission(ctx, toProto(req))
	if err != nil {
		return Decision{}, fmt.Errorf("pdp: check permission: %w", err)
	}
	return fromProto(resp), nil
}

// BatchAllowed checks several items in one round trip (BatchCheckPermission)
// and returns one Decision per item, in the same order. It exists for the AI
// Core, which must verify item by item what a person may see before showing
// a list it assembled, without a call per row.
//
// FAIL-CLOSED, all or nothing: any error — transport, a PDP that does not
// implement the RPC yet, or an answer with a different number of results
// than items — returns no decisions and an error, and the caller denies
// every item. A partial answer is never padded or trusted by position.
//
// The PDP requires every item to share tenant_id and subject; BatchAllowed
// checks that, and the MaxBatch bound, before sending anything, so a mixed
// batch is a programming error the caller sees at once instead of a remote
// denial it has to interpret. An empty batch returns no decisions and no
// error without calling the PDP.
func (c *Client) BatchAllowed(ctx context.Context, reqs []Request) ([]Decision, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if len(reqs) > MaxBatch {
		return nil, fmt.Errorf("pdp: batch of %d items exceeds the maximum of %d", len(reqs), MaxBatch)
	}
	items := make([]*authorizationv1.CheckPermissionRequest, len(reqs))
	for i, r := range reqs {
		if r.TenantID != reqs[0].TenantID || r.SubjectID != reqs[0].SubjectID || r.SubjectType != reqs[0].SubjectType {
			return nil, fmt.Errorf("pdp: batch item %d has a different tenant or subject than item 0", i)
		}
		items[i] = toProto(r)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.BatchCheckPermission(ctx, &authorizationv1.BatchCheckPermissionRequest{Items: items})
	if err != nil {
		return nil, fmt.Errorf("pdp: batch check permission: %w", err)
	}
	results := resp.GetResults()
	if len(results) != len(reqs) {
		return nil, fmt.Errorf("pdp: batch check permission returned %d results for %d items", len(results), len(reqs))
	}
	out := make([]Decision, len(results))
	for i, r := range results {
		out[i] = fromProto(r)
	}
	return out, nil
}

func toProto(req Request) *authorizationv1.CheckPermissionRequest {
	return &authorizationv1.CheckPermissionRequest{
		TenantId:            req.TenantID,
		SubjectId:           req.SubjectID,
		Action:              req.Action,
		ResourceType:        req.ResourceType,
		ResourceId:          req.ResourceID,
		Entitlements:        req.Entitlements,
		ConsistencyToken:    req.ConsistencyToken,
		SubjectType:         req.SubjectType,
		PrincipalAttributes: req.PrincipalAttributes,
		Via:                 req.Via,
	}
}

func fromProto(resp *authorizationv1.CheckPermissionResponse) Decision {
	return Decision{Allowed: resp.GetAllowed(), Reason: resp.GetReason(), DecidedAt: resp.GetDecidedAt()}
}
