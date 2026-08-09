package auth

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// healthFullMethod is exempt from token validation: a gRPC health check carries
// no user identity, and requiring one would make the probe fail whenever the
// auth service is briefly unreachable.
const healthFullMethod = "/grpc.health.v1.Health/Check"

type claimsContextKey struct{}

// withClaims stores the verified claims in ctx.
func withClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsContextKey{}, c)
}

// ContextWithClaims seeds a context the way the interceptor would, for tests
// that exercise a handler or the gate without standing up a gRPC server and
// signing a token. Production code has no reason to call it: the interceptor is
// the one place identity enters the process.
func ContextWithClaims(ctx context.Context, c *Claims) context.Context {
	ctx = withClaims(ctx, c)
	ctx = tenantctx.WithTenant(ctx, c.TenantID)
	ctx = tenantctx.WithSubject(ctx, c.Subject)
	return tenantctx.WithEntitlements(ctx, c.Entitlements)
}

// ClaimsFrom returns the verified claims a handler should use for tenant and
// subject identity.
func ClaimsFrom(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsContextKey{}).(*Claims)
	return c, ok
}

// UnaryServerInterceptor validates the bearer token on every unary RPC (except
// health) and injects the verified identity into the request context.
//
// It is the ONE place the tenant enters the process. Besides the claims, it
// seeds tenantctx, which is what the store reads to resolve the tenant's
// database — so a handler physically cannot query without having gone through
// verification first.
//
// Any failure — missing metadata, malformed header, or a Verify error —
// rejects with codes.Unauthenticated.
func UnaryServerInterceptor(verifier *Verifier) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == healthFullMethod {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "auth: missing metadata")
		}
		values := md.Get("authorization")
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "auth: missing authorization metadata")
		}
		token, ok := strings.CutPrefix(values[0], "Bearer ")
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "auth: authorization metadata must be a Bearer token")
		}

		claims, err := verifier.Verify(ctx, token)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "auth: %v", err)
		}

		return handler(ContextWithClaims(ctx, claims), req)
	}
}
