// Package ratelimit bounds how fast one tenant's principals of one type may
// call a Core (proposal §5.6): a token bucket per (tenant, principal type),
// in memory, per replica. Agents get a lower limit than people: an agent can
// loop, and a loop must hit a wall long before it hits the database.
//
// Per replica, deliberately: the limit is a guard against runaways, not a
// billing meter, and sharing it would put a network round trip in front of
// every call. With N replicas the effective ceiling is N times the bucket.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

// Limit is one bucket's shape: Rate tokens per second, up to Burst at once.
// A Rate <= 0 means no limit for that principal type.
type Limit struct {
	Rate  float64
	Burst int
}

// Config holds the bucket of each principal type. A principal type the kit
// does not know gets Agent's: the strictest is the safe default.
type Config struct {
	User    Limit
	Service Limit
	Agent   Limit
}

// DefaultConfig: people at 50 requests per second per tenant (burst 100),
// services at 200 (burst 400, they are the platform's own plumbing: relays,
// consumers, the Shell's BFF), agents at 5 (burst 10).
func DefaultConfig() Config {
	return Config{
		User:    Limit{Rate: 50, Burst: 100},
		Service: Limit{Rate: 200, Burst: 400},
		Agent:   Limit{Rate: 5, Burst: 10},
	}
}

// Limiter is the interceptor's state.
type Limiter struct {
	cfg Config
	now func() time.Time

	mu      sync.Mutex
	buckets map[bucketKey]*rate.Limiter
}

type bucketKey struct{ tenant, principalType string }

// New builds a limiter.
func New(cfg Config) *Limiter {
	return &Limiter{cfg: cfg, now: time.Now, buckets: map[bucketKey]*rate.Limiter{}}
}

func (l *Limiter) limitFor(principalType string) Limit {
	switch principalType {
	case auth.PrincipalUser:
		return l.cfg.User
	case auth.PrincipalService:
		return l.cfg.Service
	default:
		return l.cfg.Agent
	}
}

// Allow takes one token from the bucket of (tenantID, principalType).
func (l *Limiter) Allow(tenantID, principalType string) bool {
	lim := l.limitFor(principalType)
	if lim.Rate <= 0 {
		return true
	}
	k := bucketKey{tenantID, principalType}
	l.mu.Lock()
	b := l.buckets[k]
	if b == nil {
		burst := lim.Burst
		if burst < 1 {
			burst = 1
		}
		b = rate.NewLimiter(rate.Limit(lim.Rate), burst)
		l.buckets[k] = b
	}
	l.mu.Unlock()
	return b.AllowN(l.now(), 1)
}

// Unary is the interceptor. It goes after auth's (it reads the verified
// tenant and principal type); a call without claims (the health check) is
// never limited. Over the limit the answer is ResourceExhausted, which a
// well-behaved client backs off from.
//
// The buckets live as long as the process, one per tenant and principal
// type that has called: bounded by the tenants the Core serves times three.
func (l *Limiter) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		c, ok := auth.ClaimsFrom(ctx)
		if !ok {
			return handler(ctx, req)
		}
		if !l.Allow(c.TenantID, c.PrincipalType()) {
			return nil, status.Errorf(codes.ResourceExhausted, "ratelimit: too many requests from this tenant's %s principals; retry later", c.PrincipalType())
		}
		return handler(ctx, req)
	}
}
