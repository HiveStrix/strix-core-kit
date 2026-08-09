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
}

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
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.CheckPermission(ctx, &authorizationv1.CheckPermissionRequest{
		TenantId:     req.TenantID,
		SubjectId:    req.SubjectID,
		Action:       req.Action,
		ResourceType: req.ResourceType,
		ResourceId:   req.ResourceID,
		Entitlements: req.Entitlements,
	})
	if err != nil {
		return false, "", fmt.Errorf("pdp: check permission: %w", err)
	}
	return resp.GetAllowed(), resp.GetReason(), nil
}
