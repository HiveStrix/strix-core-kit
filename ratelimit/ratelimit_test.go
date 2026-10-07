package ratelimit

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func limiter(cfg Config) (*Limiter, *clock) {
	l := New(cfg)
	c := &clock{t: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	l.now = c.now
	return l, c
}

func burst(l *Limiter, tenant, pt string, n int) int {
	ok := 0
	for i := 0; i < n; i++ {
		if l.Allow(tenant, pt) {
			ok++
		}
	}
	return ok
}

func TestBucketsArePerTenantAndPrincipalType(t *testing.T) {
	l, _ := limiter(Config{User: Limit{Rate: 1, Burst: 3}, Service: Limit{Rate: 1, Burst: 5}, Agent: Limit{Rate: 1, Burst: 2}})
	if got := burst(l, "acme", "user", 10); got != 3 {
		t.Fatalf("acme users = %d, want 3", got)
	}
	// Another tenant, and another principal type of the same tenant, have
	// buckets of their own.
	if got := burst(l, "globex", "user", 10); got != 3 {
		t.Fatalf("globex users = %d, want 3", got)
	}
	if got := burst(l, "acme", "service", 10); got != 5 {
		t.Fatalf("acme services = %d, want 5", got)
	}
	if got := burst(l, "acme", "agent", 10); got != 2 {
		t.Fatalf("acme agents = %d, want 2", got)
	}
	// An unknown principal type gets the agent limit, in its own bucket.
	if got := burst(l, "acme", "robot", 10); got != 2 {
		t.Fatalf("unknown type = %d, want the agent limit 2", got)
	}
}

func TestBucketRefills(t *testing.T) {
	l, c := limiter(Config{User: Limit{Rate: 2, Burst: 2}})
	burst(l, "acme", "user", 2)
	if l.Allow("acme", "user") {
		t.Fatal("bucket should be empty")
	}
	c.t = c.t.Add(time.Second)
	if got := burst(l, "acme", "user", 5); got != 2 {
		t.Fatalf("after 1s at 2/s = %d, want 2", got)
	}
}

func TestDefaultsKeepAgentsBelowPeople(t *testing.T) {
	d := DefaultConfig()
	if !(d.Agent.Rate < d.User.Rate && d.Agent.Burst < d.User.Burst) {
		t.Fatalf("agents must be limited below people: %+v", d)
	}
	if d.Agent.Rate <= 0 || d.User.Rate <= 0 || d.Service.Rate <= 0 {
		t.Fatalf("defaults must limit every type: %+v", d)
	}
}

func TestZeroRateIsUnlimited(t *testing.T) {
	l, _ := limiter(Config{Agent: Limit{Rate: 1, Burst: 1}})
	if got := burst(l, "acme", "user", 1000); got != 1000 {
		t.Fatalf("unlimited users = %d", got)
	}
}

func TestInterceptor(t *testing.T) {
	l, _ := limiter(Config{Agent: Limit{Rate: 1, Burst: 1}})
	ran := 0
	h := func(context.Context, any) (any, error) { ran++; return nil, nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/a.B/C"}
	agent := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "a", ClientID: "a", TenantID: "acme", PrincipalTypeClaim: "agent"})
	if _, err := l.Unary()(agent, nil, info, h); err != nil {
		t.Fatal(err)
	}
	_, err := l.Unary()(agent, nil, info, h)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second agent call = %v, want ResourceExhausted", err)
	}
	// An agent whose token looks like a service's (sub == client_id) is still
	// limited as an agent.
	if ran != 1 {
		t.Fatalf("handler ran %d times", ran)
	}
	// No claims (health): never limited.
	for i := 0; i < 5; i++ {
		if _, err := l.Unary()(context.Background(), nil, info, h); err != nil {
			t.Fatal(err)
		}
	}
}
