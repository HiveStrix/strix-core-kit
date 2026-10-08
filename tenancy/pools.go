// Package tenancy resolves each tenant's database and hands out connections
// scoped to it.
//
// Two rules it exists to enforce (SCC §3, §14):
//
//   - A Core touches ONLY its own schema. Data owned by another Core is reached
//     by opaque ID through its API or an event — never a cross-schema JOIN or
//     foreign key.
//   - The DSN NEVER travels over the network from a caller (SCC §4, §14.2). It
//     is resolved here, from the tenant registry and Vault. A request that
//     carried its own connection string would let any caller point a Core at any
//     database.
package tenancy

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound is returned when a key-based lookup finds no row.
	ErrNotFound = errors.New("tenancy: not found")
	// ErrNoTenant is returned when a call reaches the store without a tenant in
	// context. It is a programming error, not a user error: the PEP guarantees
	// the tenant is present, so reaching here without one means a code path
	// bypassed it.
	ErrNoTenant = errors.New("tenancy: no tenant in context")
)

// DSNResolver turns a tenant id into the DSN of that tenant's database.
type DSNResolver interface {
	Resolve(ctx context.Context, tenantID string) (string, error)
}

// TemplateResolver builds a DSN from a template, substituting the tenant id for
// the literal "{slug}", and pins search_path to the Core's schema.
//
// For development and single-tenant runs. In the cluster the DSN comes from the
// tenant registry the Shell owns, with credentials from Vault.
type TemplateResolver struct {
	Template string
	// Schema is the Core's schema inside every tenant database
	// (schema-per-core, SCC §3).
	Schema string
}

// validSlug is what a tenant id may contain to be substituted into a DSN.
//
// The tenant id arrives from a verified token, so it is not attacker-chosen,
// but it IS caller-supplied, and the rule is that the connection string is not
// influenced by the caller. Unrestricted, a slug like "acme?sslmode=disable"
// substitutes into the template and appends a real connection parameter — the
// tenant id silently turning off TLS to the database. Restricting the alphabet
// removes the whole class instead of chasing the parameters one by one.
var validSlug = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Resolve implements DSNResolver.
func (r TemplateResolver) Resolve(_ context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", ErrNoTenant
	}
	if !validSlug.MatchString(tenantID) {
		return "", fmt.Errorf("tenancy: tenant id %q is not a valid slug", tenantID)
	}
	if !strings.Contains(r.Template, "{slug}") {
		return "", fmt.Errorf("tenancy: DSN template must contain {slug}")
	}
	return withSearchPath(strings.ReplaceAll(r.Template, "{slug}", tenantID), r.Schema)
}

// withSearchPath pins search_path to the Core's schema, so every unqualified
// table name resolves inside it and nowhere else.
func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// NUNCA repetir el DSN en el error: lleva la contraseña del rol del
		// core, y este error viaja a los logs — el relay del outbox lo escribe
		// en cada barrido. Un *url.Error embebe la URL COMPLETA; se propaga
		// solo su motivo (fuga real: core-divisions, 2026-08-25 — una
		// contraseña con % acabó en Loki cada 2 segundos).
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return "", fmt.Errorf("tenancy: invalid DSN: %v", uerr.Err)
		}
		return "", errors.New("tenancy: invalid DSN")
	}
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Pools keeps one connection pool per tenant, opened lazily and evicted least
// recently used first (SCC §4, connection routing).
//
// A pool per tenant rather than one shared pool: the tenant boundary is the
// database, so a shared pool would have nothing to share. The LRU cap keeps a
// deployment with hundreds of tenants from holding hundreds of idle pools.
type Pools struct {
	resolver DSNResolver
	limit    int

	settings PoolSettings

	mu      sync.Mutex
	order   *list.List               // front = most recently used
	entries map[string]*list.Element // tenantID -> element holding *poolEntry

	stopOnce sync.Once
	stop     chan struct{}
}

type poolEntry struct {
	tenantID string
	pool     *pgxpool.Pool
	lastUsed time.Time
}

// NewPools builds a pool registry holding at most limit pools, each sized by
// PoolSettingsFromEnv (STRIX_DB_POOL_*).
func NewPools(resolver DSNResolver, limit int) *Pools {
	return NewPoolsWithSettings(resolver, limit, PoolSettingsFromEnv())
}

// NewPoolsWithSettings is NewPools with explicit pool settings. When
// settings.IdleClose > 0 a background reaper closes the pool of any tenant no
// request used for that long; the next request reopens it.
func NewPoolsWithSettings(resolver DSNResolver, limit int, settings PoolSettings) *Pools {
	if limit < 1 {
		limit = 1
	}
	p := &Pools{
		resolver: resolver,
		limit:    limit,
		settings: settings,
		order:    list.New(),
		entries:  make(map[string]*list.Element, limit),
		stop:     make(chan struct{}),
	}
	if settings.IdleClose > 0 {
		go p.reap()
	}
	return p
}

// Get returns the pool for a tenant, opening it on first use. It counts as use:
// it keeps the pool from being closed for inactivity.
func (p *Pools) Get(ctx context.Context, tenantID string) (*pgxpool.Pool, error) {
	return p.get(ctx, tenantID, true)
}

// GetBackground is Get for background sweeps (the outbox relay). It does not
// count as use, so a tenant with no real traffic still releases its pool even
// though the relay polls it every few seconds.
func (p *Pools) GetBackground(ctx context.Context, tenantID string) (*pgxpool.Pool, error) {
	return p.get(ctx, tenantID, false)
}

func (p *Pools) get(ctx context.Context, tenantID string, touch bool) (*pgxpool.Pool, error) {
	if tenantID == "" {
		return nil, ErrNoTenant
	}

	p.mu.Lock()
	if el, ok := p.entries[tenantID]; ok {
		e := el.Value.(*poolEntry)
		if touch {
			p.order.MoveToFront(el)
			e.lastUsed = time.Now()
		}
		pool := e.pool
		p.mu.Unlock()
		return pool, nil
	}
	p.mu.Unlock()

	// Resolve and dial outside the lock: opening a pool round-trips to the
	// database, and holding the mutex would serialize every other tenant behind
	// it.
	dsn, err := p.resolver.Resolve(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("tenancy: resolve tenant %q: %w", tenantID, err)
	}
	pool, err := connect(ctx, dsn, p.settings)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Another goroutine may have opened it while this one was dialing. Keep
	// theirs and discard this one, so a tenant never has two live pools.
	if el, ok := p.entries[tenantID]; ok {
		e := el.Value.(*poolEntry)
		if touch {
			p.order.MoveToFront(el)
			e.lastUsed = time.Now()
		}
		pool.Close()
		return e.pool, nil
	}

	el := p.order.PushFront(&poolEntry{tenantID: tenantID, pool: pool, lastUsed: time.Now()})
	p.entries[tenantID] = el
	p.evictLocked()
	return pool, nil
}

// evictLocked closes least-recently-used pools past the limit. Caller holds mu.
func (p *Pools) evictLocked() {
	for p.order.Len() > p.limit {
		oldest := p.order.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*poolEntry)
		p.order.Remove(oldest)
		delete(p.entries, entry.tenantID)
		// Close is asynchronous with respect to in-flight queries: pgxpool
		// waits for borrowed connections to be returned before tearing down.
		go entry.pool.Close()
	}
}

// KnownTenants lists the tenants with a live pool.
//
// It is what the outbox relay sweeps. A tenant only gets a pool once it has had
// traffic, and events only exist as a result of traffic, so in a running
// process the two sets coincide. They do NOT coincide right after a restart: a
// tenant with pending events that receives no requests would not be swept,
// which is why the relay also takes a configured list. Both go away when the
// tenant registry exists and can simply be asked.
func (p *Pools) KnownTenants() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.entries))
	for tenantID := range p.entries {
		out = append(out, tenantID)
	}
	return out
}

// reap closes the pools nobody used for IdleClose. A pool with borrowed
// connections is left alone.
func (p *Pools) reap() {
	every := p.settings.IdleClose / 4
	if every < 100*time.Millisecond {
		every = 100 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.reapIdle(time.Now())
		}
	}
}

func (p *Pools) reapIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for el := p.order.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*poolEntry)
		if now.Sub(e.lastUsed) >= p.settings.IdleClose && e.pool.Stat().AcquiredConns() == 0 {
			p.order.Remove(el)
			delete(p.entries, e.tenantID)
			go e.pool.Close()
		}
		el = prev
	}
}

// Close releases every pool.
func (p *Pools) Close() {
	p.stopOnce.Do(func() { close(p.stop) })
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, el := range p.entries {
		el.Value.(*poolEntry).pool.Close()
	}
	p.entries = make(map[string]*list.Element)
	p.order.Init()
}

// connect opens a pool and verifies it answers.
func connect(ctx context.Context, dsn string, settings PoolSettings) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// Never repeat the DSN: it carries the core role's password.
		return nil, errors.New("tenancy: invalid DSN")
	}
	settings.apply(cfg)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tenancy: pgxpool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("tenancy: ping: %w", err)
	}
	return pool, nil
}
