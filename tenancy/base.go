package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

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

	// El schema que fija el search_path del DSN puede no existir todavía: el
	// fan-out PostSync corre sobre TODA base de tenant, incluidas aquellas donde
	// el módulo nunca se activó y por tanto el provisioner jamás lo creó. Sin
	// schema, goose no tiene dónde poner su tabla de versión y Postgres responde
	// "no schema has been selected to create in".
	// Se COMPRUEBA antes de crear, y no basta con `IF NOT EXISTS`: ese
	// modificador evita el error de "ya existe", pero Postgres verifica el
	// privilegio ANTES, y `CREATE SCHEMA` lo exige sobre la BASE DE DATOS, no
	// sobre el schema. El rol de un Core tiene CREATE en su propio schema y
	// nada sobre la base — como debe ser: crear schemas es activar un módulo,
	// y eso lo decide el provisioner.
	//
	// Sin esta comprobación, migrar un schema que ya existe fallaba con
	// "permission denied for database" aunque no hubiera nada que crear.
	if schema := schemaFromDSN(dsn); schema != "" {
		var exists bool
		if err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
			schema).Scan(&exists); err != nil {
			return fmt.Errorf("tenancy: check schema %q: %w", schema, err)
		}
		if !exists {
			if _, err := db.Exec("CREATE SCHEMA IF NOT EXISTS " + pgx.Identifier{schema}.Sanitize()); err != nil {
				return fmt.Errorf("tenancy: create schema %q: %w", schema, err)
			}
		}
	}

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("tenancy: goose dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("tenancy: goose up: %w", err)
	}
	return nil
}

// schemaFromDSN lee el schema que el DSN fija en options=-csearch_path=...
//
// Devuelve cadena vacía cuando el DSN no fija ninguno, que es la señal de "no
// crees nada": una migración sin search_path va al schema por defecto y ahí no
// hay nada que preparar.
func schemaFromDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	_, after, found := strings.Cut(u.Query().Get("options"), "-csearch_path=")
	if !found {
		return ""
	}
	// search_path admite una lista; el schema del Core es siempre el primero.
	first, _, _ := strings.Cut(after, ",")
	return strings.TrimSpace(first)
}
