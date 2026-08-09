// Package authz is the authorization gate every handler goes through
// (SCC Appendix D §B).
//
// DENY BY DEFAULT. A handler that forgets to call Require simply has no gate,
// so the rule is: every RPC calls it, first thing, before touching anything.
package authz

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
	"github.com/hs-javierviquez/strix-core-kit/pdp"
)

// Gate checks entitlement locally and delegates the permission decision to the
// PDP.
type Gate struct {
	pdp    *pdp.Client
	module string
}

// New builds the gate over a PDP client for a Core's entitlement key — what the
// tenant bought (SCC Appendix E.4), e.g. "expenses" or "clients".
func New(pdpClient *pdp.Client, module string) *Gate {
	return &Gate{pdp: pdpClient, module: module}
}

// Require authorizes an action, returning a gRPC status error when it is not
// allowed. It runs two checks, in order:
//
//  1. ENTITLEMENT — does the tenant own this module at all? Local, from the
//     verified claim. A tenant without the module gets the same answer whatever
//     the PDP would say, and asking the PDP first would leak that the resource
//     exists.
//  2. PDP — is this subject allowed this action? Remote, fail-closed.
//
// action is "<module>[.<group>...].<verb>". The Core does not get to say which
// verb its action is judged by: the PDP derives that from the last segment, so
// naming the action IS choosing the tier. An action that needs tenant-admin
// ends in a verb that says so ("expenses.taxrate.admin") instead of borrowing
// the name of a write.
//
// On scopes: the ecosystem contract lists "entitlement + scopes" for this gate.
// The scope names are declared when a module is registered in the marketplace;
// until then, checking against invented names would deny every request. The
// entitlement and the PDP carry the decision meanwhile.
func (g *Gate) Require(ctx context.Context, action, resourceType, resourceID string) error {
	claims, ok := auth.ClaimsFrom(ctx)
	if !ok {
		// The interceptor guarantees claims are present, so reaching here means
		// an RPC was registered outside the interceptor chain.
		return status.Error(codes.Unauthenticated, "authz: missing verified claims")
	}

	// A misnamed action is how the PDP ends up evaluating a request against a
	// module the Core did not mean, and it is invisible: the request just gets
	// judged by someone else's rules. Cheap to catch, so catch it loudly rather
	// than let it deny — or worse, allow — for reasons nobody can see.
	if mod, _, _ := strings.Cut(action, "."); mod != g.module {
		slog.ErrorContext(ctx, "authz: action does not belong to this module",
			"action", action, "module", g.module)
		return status.Errorf(codes.Internal, "authz: action %q does not belong to module %q", action, g.module)
	}

	if !hasEntitlement(claims.Entitlements, g.module) {
		return status.Errorf(codes.PermissionDenied, "authz: tenant is not entitled to the %q module", g.module)
	}

	allowed, reason, err := g.pdp.Allowed(ctx, pdp.Request{
		TenantID:     claims.TenantID,
		SubjectID:    claims.Subject,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Entitlements: claims.Entitlements,
	})
	if err != nil {
		// Logged, not returned to the caller: whether the PDP was unreachable
		// or the policy said no is not the caller's business, and the
		// distinction is exactly what an attacker probes for.
		slog.ErrorContext(ctx, "authz: PDP unreachable, denying", "action", action, "error", err)
		return status.Error(codes.PermissionDenied, "authz: denied")
	}
	if !allowed {
		slog.InfoContext(ctx, "authz: denied by policy", "action", action, "reason", reason)
		return status.Error(codes.PermissionDenied, "authz: denied")
	}
	return nil
}

// Claims returns the verified claims, for handlers that need the subject after
// passing the gate.
func Claims(ctx context.Context) (*auth.Claims, bool) {
	return auth.ClaimsFrom(ctx)
}

func hasEntitlement(entitlements []string, key string) bool {
	for _, e := range entitlements {
		if e == key {
			return true
		}
	}
	return false
}
