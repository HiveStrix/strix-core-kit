// Package tenantctx carries the authenticated tenant and subject through the
// request context.
//
// The values here are written in ONE place — the PEP interceptor, after
// verifying the access token — and read everywhere else. They are never taken
// from a request body: the tenant_id field that appears in some proto messages
// is legacy and informative, and the claim is the source of truth
// (SCC Appendix D §A.7).
package tenantctx

import "context"

type contextKey int

const (
	tenantKey contextKey = iota
	subjectKey
	entitlementsKey
)

// WithTenant returns a context carrying the authenticated tenant id.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey, tenantID)
}

// Tenant returns the authenticated tenant id, or "" when there is none.
func Tenant(ctx context.Context) string {
	v, _ := ctx.Value(tenantKey).(string)
	return v
}

// WithSubject returns a context carrying the authenticated subject (the `sub`
// claim), which is what audit trails record as the actor.
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectKey, subject)
}

// Subject returns the authenticated subject, or "" when there is none.
func Subject(ctx context.Context) string {
	v, _ := ctx.Value(subjectKey).(string)
	return v
}

// WithEntitlements returns a context carrying the modules the tenant is
// entitled to (SCC Appendix E.4).
func WithEntitlements(ctx context.Context, entitlements []string) context.Context {
	return context.WithValue(ctx, entitlementsKey, entitlements)
}

// Entitlements returns the tenant's entitlements, or nil when there are none.
func Entitlements(ctx context.Context) []string {
	v, _ := ctx.Value(entitlementsKey).([]string)
	return v
}
