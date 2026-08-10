package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// Querier is what both a pool and a transaction satisfy.
//
// It exists so a read can happen either standalone or INSIDE the transaction
// that is about to write. Reading outside the transaction would leave a window
// where a concurrent write changes the state between the read and the write,
// and the result would be computed against a state that no longer exists.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Base is what a Core's repository embeds to get tenant-scoped connections and
// transactions. The domain methods stay in the Core — this only answers "which
// database, and in what transaction".
type Base struct {
	pools *Pools
}

// NewBase builds a Base over a per-tenant pool registry.
func NewBase(pools *Pools) *Base {
	return &Base{pools: pools}
}

// Pools exposes the pool registry, for the relay that sweeps every tenant.
func (b *Base) Pools() *Pools {
	return b.pools
}

// Conn resolves the calling tenant's pool and returns it alongside the tenant
// id, which every query then carries explicitly.
//
// EVERY query carries tenant_id even though the tenant database already
// isolates. Defence in depth: the day a connection is resolved wrong, the query
// still cannot read another tenant's rows.
func (b *Base) Conn(ctx context.Context) (*pgxpool.Pool, string, error) {
	tenantID := tenantctx.Tenant(ctx)
	if tenantID == "" {
		return nil, "", ErrNoTenant
	}
	pool, err := b.pools.Get(ctx, tenantID)
	if err != nil {
		return nil, "", err
	}
	return pool, tenantID, nil
}

// PoolFor returns a specific tenant's pool, for callers working outside a
// request context. See InTxFor for when that is legitimate.
func (b *Base) PoolFor(ctx context.Context, tenantID string) (*pgxpool.Pool, error) {
	if tenantID == "" {
		return nil, ErrNoTenant
	}
	return b.pools.Get(ctx, tenantID)
}

// InTx runs fn inside a transaction, rolling back on error or panic.
//
// Multi-table mutations WITHIN this schema go in a single transaction. Nothing
// ever spans two Cores: there is no transaction that embraces two of them, and
// what needs cross-core atomicity is a saga with compensation (SCC §8).
func (b *Base) InTx(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	pool, tenantID, err := b.Conn(ctx)
	if err != nil {
		return err
	}
	return runTx(ctx, pool, func(tx pgx.Tx) error { return fn(tx, tenantID) })
}

// InTxFor runs fn in a transaction against an EXPLICIT tenant, for callers that
// have no request context to read it from.
//
// The event consumer is the case: the tenant arrives inside the message
// envelope, not in a JWT. It is the one legitimate way past tenantctx, and it
// stays a separate method precisely so it cannot be reached by accident from a
// handler.
func (b *Base) InTxFor(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	pool, err := b.PoolFor(ctx, tenantID)
	if err != nil {
		return err
	}
	return runTx(ctx, pool, fn)
}

func runTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenancy: begin: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe on
		// every path, including a panic unwinding through it.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenancy: commit: %w", err)
	}
	return nil
}

// Migrate applies a Core's embedded migrations idempotently against dsn.
//
// This is what the Shell's provisioner runs as a Job with the Core's image when
// a tenant activates the module, and what the PostSync fan-out re-runs across
// every active tenant database on redeploy (SCC §3, §13). The migrations belong
// to the Core, so it passes its own embedded FS.
func Migrate(dsn string, migrations fs.FS) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("tenancy: open for migrate: %w", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("tenancy: goose dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("tenancy: goose up: %w", err)
	}
	return nil
}
