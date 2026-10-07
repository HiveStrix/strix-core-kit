package outbox

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/hs-javierviquez/strix-core-kit/tenancy"
)

// Tenant discovery for the relay.
//
// Without it the relay sweeps the tenants with a live pool plus a fixed list
// (ExtraTenants, the CORE_<X>_RELAY_TENANTS of each spec). That list had to be
// edited in every Core for every new tenant, or the new tenant's events sat in
// its outbox; and a tenant listed without the module failed with 42501 on every
// sweep. With Discover the relay asks Postgres which tenants have the Core
// activated, the same question the migration fan-out asks.

const (
	// defaultDiscoverEvery is how long a new tenant may wait before its
	// events start flowing. Cheap enough to ask often: one query on the
	// maintenance database plus one short connection per activated tenant.
	defaultDiscoverEvery = time.Minute
	// earlyRefreshGap bounds how often a failing tenant may pull the next
	// refresh forward.
	earlyRefreshGap = 10 * time.Second
	// discoverTimeout bounds one discovery, so a database that does not answer
	// cannot stall the sweeps of the tenants already known.
	discoverTimeout = 30 * time.Second
)

// ActiveTenants returns a Config.Discover listing the tenants where the
// Core's schema is activated (tenancy.TenantsWithSchema): the databases of
// template where current_user has CONNECT and the schema exists. A database
// without CONNECT is filtered in the enumeration query and never receives a
// connection, so it leaves nothing in the engine's log either.
func ActiveTenants(template, schema string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		return tenancy.TenantsWithSchema(ctx, template, schema)
	}
}

// discovery is the relay's view of the activated tenants. Only the Run
// goroutine touches it.
type discovery struct {
	active  []string
	ok      bool // a discovery has succeeded at least once
	lastTry time.Time
	soon    bool // a tenant failed: refresh before DiscoverEvery
	warned  bool
	// suspect holds the tenants that failed since the last refresh: they may
	// have just lost the module, so their failures stay silent until a
	// refresh says otherwise. failing holds the ones a refresh confirmed
	// active: their failures are real and logged.
	suspect map[string]bool
	failing map[string]bool
}

func (r *Relay) tenants(ctx context.Context) []string {
	if r.cfg.Discover == nil {
		return r.fallbackTenants()
	}
	r.refresh(ctx)
	if !r.disc.ok {
		return r.fallbackTenants()
	}
	return r.disc.active
}

// fallbackTenants is the sweep without discovery, or before the first
// discovery succeeds: the tenants with a live pool plus ExtraTenants.
func (r *Relay) fallbackTenants() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range append(r.store.KnownTenants(), r.cfg.ExtraTenants...) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (r *Relay) refresh(ctx context.Context) {
	now := time.Now()
	every := r.cfg.DiscoverEvery
	if every <= 0 {
		every = defaultDiscoverEvery
	}
	since := now.Sub(r.disc.lastTry)
	if !r.disc.lastTry.IsZero() && since < every && !(r.disc.soon && since >= earlyRefreshGap) {
		return
	}
	r.disc.lastTry = now
	r.disc.soon = false

	attempt, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	found, err := r.cfg.Discover(attempt)
	if err != nil {
		if !r.disc.warned {
			r.disc.warned = true
			sweeping := "the tenants with a live pool and ExtraTenants"
			if r.disc.ok {
				sweeping = "the last discovered tenants"
			}
			slog.WarnContext(ctx, "relay: tenant discovery failed, sweeping "+sweeping,
				"stream", r.cfg.StreamName, "error", err)
		}
		return
	}
	if r.disc.warned {
		r.disc.warned = false
		slog.InfoContext(ctx, "relay: tenant discovery recovered", "stream", r.cfg.StreamName)
	}
	r.adopt(ctx, found)
}

func (r *Relay) adopt(ctx context.Context, found []string) {
	if !r.disc.ok {
		slog.InfoContext(ctx, "relay: sweeping the activated tenants", "stream", r.cfg.StreamName, "tenants", found)
		if len(r.cfg.ExtraTenants) > 0 {
			slog.InfoContext(ctx, "relay: ExtraTenants ignored, the activated tenants are discovered",
				"stream", r.cfg.StreamName, "extra_tenants", r.cfg.ExtraTenants)
		}
	} else {
		for _, t := range found {
			if !slices.Contains(r.disc.active, t) {
				slog.InfoContext(ctx, "relay: tenant activated, sweeping it", "stream", r.cfg.StreamName, "tenant", t)
			}
		}
		for _, t := range r.disc.active {
			if !slices.Contains(found, t) {
				slog.InfoContext(ctx, "relay: tenant no longer activated, no longer swept", "stream", r.cfg.StreamName, "tenant", t)
			}
		}
	}
	for t := range r.disc.suspect {
		if slices.Contains(found, t) {
			if r.disc.failing == nil {
				r.disc.failing = map[string]bool{}
			}
			r.disc.failing[t] = true
		}
	}
	r.disc.suspect = nil
	for t := range r.disc.failing {
		if !slices.Contains(found, t) {
			delete(r.disc.failing, t)
		}
	}
	r.disc.active = found
	r.disc.ok = true
}

// fetchFailed reports a tenant whose outbox could not be read. With discovery,
// the first failure since the last refresh is not logged: the tenant may have
// just lost the module (its schema dropped, its CONNECT revoked), so the relay
// asks again soon and drops it quietly if so. Only a tenant a refresh still
// finds activated has a real problem, and from then on it is logged every
// sweep, as without discovery.
func (r *Relay) fetchFailed(ctx context.Context, tenantID string, err error) {
	if r.cfg.Discover == nil || !r.disc.ok || r.disc.failing[tenantID] {
		slog.ErrorContext(ctx, "relay: fetch outbox failed", "tenant", tenantID, "error", err)
		return
	}
	if r.disc.suspect == nil {
		r.disc.suspect = map[string]bool{}
	}
	r.disc.suspect[tenantID] = true
	r.disc.soon = true
}

func (r *Relay) fetchOK(tenantID string) {
	delete(r.disc.suspect, tenantID)
	delete(r.disc.failing, tenantID)
}
