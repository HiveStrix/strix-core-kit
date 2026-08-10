package tenancy

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// The resolver is what pins search_path to the Core's schema. Without it every
// unqualified table name would resolve wherever the connection's default search
// path points — which is how one Core ends up reading another's tables in the
// same tenant database (SCC §3).
func TestTemplateResolverPinsTheSearchPath(t *testing.T) {
	r := TemplateResolver{Template: "postgres://user:pw@host:5432/tenant_{slug}", Schema: "expenses"}

	dsn, err := r.Resolve(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("the resolved DSN does not parse: %v", err)
	}
	if !strings.HasSuffix(u.Path, "tenant_acme") {
		t.Errorf("database = %q, want it to carry the tenant slug", u.Path)
	}
	if got := u.Query().Get("options"); got != "-csearch_path=expenses" {
		t.Errorf("options = %q, want -csearch_path=expenses", got)
	}
}

func TestTemplateResolverRejects(t *testing.T) {
	cases := []struct {
		name     string
		template string
		tenant   string
		wantErr  error
	}{
		{"no tenant", "postgres://host/db_{slug}", "", ErrNoTenant},
		{"template without the slug placeholder", "postgres://host/db", "acme", nil},
		{"template that is not a URL", "://nope{slug}", "acme", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TemplateResolver{Template: tc.template, Schema: "expenses"}.Resolve(context.Background(), tc.tenant)
			if err == nil {
				t.Fatal("Resolve succeeded, want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// A tenant id must not be able to smuggle connection parameters into the DSN.
// It arrives from a verified token, so it is not attacker-chosen, but it is
// caller-supplied, and the rule is that the connection string is not influenced
// by the caller. "acme?sslmode=disable" substituted into the template appends a
// real parameter: the tenant id turning off TLS to the database.
func TestTemplateResolverRejectsASlugThatCanAlterTheDSN(t *testing.T) {
	r := TemplateResolver{Template: "postgres://host:5432/tenant_{slug}", Schema: "expenses"}

	for _, slug := range []string{
		"acme?sslmode=disable",
		"acme?options=-csearch_path=clients",
		"acme/../other",
		"acme with spaces",
		"acme#fragment",
	} {
		if dsn, err := r.Resolve(context.Background(), slug); err == nil {
			t.Errorf("slug %q was accepted, producing %q", slug, dsn)
		}
	}
}

// A missing tenant is a programming error — the PEP guarantees one is present —
// so it must surface as such rather than reaching the database.
func TestPoolsRejectAnEmptyTenant(t *testing.T) {
	p := NewPools(TemplateResolver{Template: "postgres://host/db_{slug}", Schema: "expenses"}, 4)
	t.Cleanup(p.Close)

	if _, err := p.Get(context.Background(), ""); !errors.Is(err, ErrNoTenant) {
		t.Errorf("error = %v, want ErrNoTenant", err)
	}
	if got := p.KnownTenants(); len(got) != 0 {
		t.Errorf("KnownTenants = %v, want empty", got)
	}
}

// The limit is what keeps a deployment with hundreds of tenants from holding
// hundreds of idle pools; a zero or negative one would mean no pools at all.
func TestNewPoolsFloorsTheLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		if got := NewPools(nil, limit).limit; got != 1 {
			t.Errorf("NewPools(_, %d).limit = %d, want 1", limit, got)
		}
	}
}
