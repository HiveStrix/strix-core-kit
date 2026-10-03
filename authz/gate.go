// Package authz is the authorization gate every handler goes through
// (SCC Appendix D §B).
//
// DENY BY DEFAULT. A handler that forgets to call Require simply has no gate,
// so the rule is: every RPC calls it, first thing, before touching anything.
package authz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

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

	// machines is the per-action client_id allowlist for service principals.
	// nil means the gate was built without one and a machine is decided by
	// scope alone, exactly as before v0.18.0.
	machines map[string]map[string]struct{}
}

// Option configures a Gate when it is built.
type Option struct {
	apply func(*Gate) error
}

// New builds the gate over a PDP client for a Core's entitlement key — what the
// tenant bought (SCC Appendix E.4), e.g. "expenses" or "clients".
//
// New panics when an option is malformed (see MachineAllow). That is a
// programming error in a literal the Core wrote, so it stops the process at
// startup, like regexp.MustCompile, instead of shipping a gate that quietly
// lets through — or locks out — someone nobody meant to.
func New(pdpClient *pdp.Client, module string, opts ...Option) *Gate {
	g := &Gate{pdp: pdpClient, module: module}
	for _, o := range opts {
		if err := o.apply(g); err != nil {
			panic(err)
		}
	}
	return g
}

// MachineAllow lists the service principals (by client_id, which on a
// client_credentials token is also its sub) that may perform action. It
// closes the gap a scope alone leaves open: every Core that holds core.read
// for an audience could read EVERYTHING that Core serves — salaries and
// national ids included — because which RPC it needed was never part of the
// decision.
//
// Passing at least one MachineAllow turns the gate into allowlist mode, and
// then, for a machine:
//
//   - it passes only when its client_id is listed for THAT exact action AND
//     its scope still covers the verb (ScopeFor) — the allowlist narrows the
//     scope, it never replaces it;
//   - an action no MachineAllow names is denied to every machine
//     (deny-by-default). To keep a machine out of an action, leave the action
//     out.
//
// Every machine denial is then the same "authz: denied" the PDP's denial
// reads, a missing scope included: the answer never tells the caller whether
// it is listed. Why it was denied goes to the log.
//
// People are not affected: they keep going through entitlement and the PDP.
// A gate built without MachineAllow behaves exactly as before.
//
// The allowlist is per action, not per RPC: two RPCs gated by the same action
// name share one entry. Where different machines need different RPCs, give
// those RPCs different action names (the middle segments are free).
//
// Calls for the same action add up. New panics on an action that is not
// "<module>.<...>.<verb>" of this gate's module, or on an empty or padded
// client_id, or on an empty list.
func MachineAllow(action string, clientIDs ...string) Option {
	ids := append([]string(nil), clientIDs...)
	return Option{apply: func(g *Gate) error {
		if err := checkAction(action, g.module); err != nil {
			return fmt.Errorf("authz: MachineAllow: %w", err)
		}
		if len(ids) == 0 {
			return fmt.Errorf("authz: MachineAllow(%q) lists no client_id; an action no machine may perform is left out instead", action)
		}
		for _, id := range ids {
			if id == "" || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
				return fmt.Errorf("authz: MachineAllow(%q): client_id %q is empty or has whitespace and would never match a token", action, id)
			}
		}
		if g.machines == nil {
			g.machines = map[string]map[string]struct{}{}
		}
		set := g.machines[action]
		if set == nil {
			set = map[string]struct{}{}
			g.machines[action] = set
		}
		for _, id := range ids {
			set[id] = struct{}{}
		}
		return nil
	}}
}

// checkAction validates an action name written into the Core's code: it must
// belong to module and have no empty or space-carrying segment, since such a
// name never matches what a handler passes to Require.
func checkAction(action, module string) error {
	segments := strings.Split(action, ".")
	if len(segments) < 2 {
		return fmt.Errorf("action %q has no verb; want \"<module>[.<group>...].<verb>\"", action)
	}
	if segments[0] != module {
		return fmt.Errorf("action %q does not belong to module %q", action, module)
	}
	for _, s := range segments {
		if s == "" || strings.IndexFunc(s, unicode.IsSpace) >= 0 {
			return fmt.Errorf("action %q has an empty or space-carrying segment", action)
		}
	}
	return nil
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
// On scopes: for a PERSON the scope names are declared when a module is
// registered in the marketplace; until then, checking against invented names
// would deny every request, so the entitlement and the PDP carry the decision.
// For a MACHINE (a client_credentials token, security-contract §5.6, whose
// sub is its client_id) the scope decides: core.read for reads, core.write
// for everything else. A machine has no entitlements and no ReBAC groups, so
// neither stage applies to it, and which Cores it may reach was fixed by its
// client's audience allowlist when the token was minted. A gate built with
// MachineAllow first asks whether this client_id is listed for this action,
// and only then looks at the scope.
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

	if claims.IsService() {
		if g.machines != nil {
			if _, listed := g.machines[action][claims.ClientID]; !listed {
				// The caller gets the same answer whether the action has other
				// clients listed, none, or does not exist: which machines may
				// do what is not the caller's business.
				slog.InfoContext(ctx, "authz: service principal not allowlisted for action",
					"action", action, "client", claims.ClientID)
				return status.Error(codes.PermissionDenied, "authz: denied")
			}
		}
		need := ScopeFor(action)
		if !claims.HasScope(need) && !(need == ScopeRead && claims.HasScope(ScopeWrite)) {
			slog.InfoContext(ctx, "authz: service principal lacks scope",
				"action", action, "client", claims.ClientID, "scope", claims.Scope, "need", need)
			if g.machines != nil {
				// Naming the missing scope here would tell a listed machine
				// apart from an unlisted one, i.e. tell the caller whether it
				// is on the allowlist. The detail stays in the log.
				return status.Error(codes.PermissionDenied, "authz: denied")
			}
			return status.Errorf(codes.PermissionDenied, "authz: service principal lacks scope %q", need)
		}
		return nil
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

// Scopes a machine principal may hold (security-contract §8.4).
const (
	ScopeRead  = "core.read"
	ScopeWrite = "core.write"
)

// ScopeFor maps an action to the scope a service principal needs for it:
// the verb is the last segment of the action name, and only reads are
// reads. Everything else — write, admin, update, delete — is core.write.
func ScopeFor(action string) string {
	verb := action[strings.LastIndex(action, ".")+1:]
	switch verb {
	case "read", "list", "get", "lookup", "search", "view":
		return ScopeRead
	}
	return ScopeWrite
}

func hasEntitlement(entitlements []string, key string) bool {
	for _, e := range entitlements {
		if e == key {
			return true
		}
	}
	return false
}
