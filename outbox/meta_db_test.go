package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/hs-javierviquez/strix-core-kit/tenancy"
)

// The DB tests run against a real Postgres when KIT_TEST_DSN is set (a
// database of their own; each test makes and drops a schema in it):
//
//	createdb kit_test_aiready
//	KIT_TEST_DSN='postgres://postgres:test@localhost:5439/kit_test_aiready?sslmode=disable' go test ./outbox ./idempotency
const legacyOutbox = `-- +goose Up
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    tenant_id    text NOT NULL,
    event_id     uuid NOT NULL DEFAULT gen_random_uuid(),
    subject      text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts     integer NOT NULL DEFAULT 0
);
-- +goose Down
DROP TABLE outbox;
`

type schemaResolver struct{ dsn string }

func (r schemaResolver) Resolve(context.Context, string) (string, error) { return r.dsn, nil }

// testSchema creates a fresh schema with the legacy outbox and returns a DSN
// pinned to it.
func testSchema(t *testing.T) string {
	t.Helper()
	base := os.Getenv("KIT_TEST_DSN")
	if base == "" {
		t.Skip("KIT_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// migrate applies the given goose files in order (Up) against dsn.
func migrate(t *testing.T, dsn string, files ...string) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	for i, body := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d_m.sql", i+1)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	return db
}

func storeFor(t *testing.T, dsn string) *Store {
	pools := tenancy.NewPools(schemaResolver{dsn}, 2)
	t.Cleanup(pools.Close)
	return New(tenancy.NewBase(pools))
}

func insert(t *testing.T, s *Store, write func(context.Context, pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := s.base.InTxFor(ctx, "acme", func(tx pgx.Tx) error { return write(ctx, tx) }); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestTraceColumnsRoundTrip(t *testing.T) {
	dsn := testSchema(t)
	migrate(t, dsn, legacyOutbox, TraceColumnsMigration)
	s := storeFor(t, dsn)
	ctx := context.Background()

	cause := &Causation{EventID: "11111111-1111-4111-8111-111111111111", PrincipalType: "agent", PrincipalID: "inst-7", Chain: []string{"u-1", "inst-7"}}
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	insert(t, s, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.Insert(ctx, tx, "acme", "x.plain.v1", []byte(`{"n":1}`)); err != nil {
			return err
		}
		_, err := s.InsertWith(ctx, tx, "acme", "x.meta.v1", []byte(`{"n":2}`), Meta{Traceparent: tp, Causation: cause})
		return err
	})

	rows, err := s.FetchUnpublishedWithMeta(ctx, "acme", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].Traceparent != "" || rows[0].Causation != nil {
		t.Fatalf("a plain Insert has no meta: %+v", rows[0])
	}
	if rows[1].Traceparent != tp || !strings.Contains(string(rows[1].Causation), `"inst-7"`) {
		t.Fatalf("meta lost: %+v", rows[1])
	}
	raw, err := envelopeFor(rows[1])
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ParseEnvelope(raw)
	if env.Traceparent != tp || env.Causation == nil || env.Causation.PrincipalID != "inst-7" || len(env.Causation.Chain) != 2 {
		t.Fatalf("envelope = %+v", env)
	}
	// The relay reads them only when told the columns exist.
	withCols := &Relay{store: s, cfg: Config{Batch: 10, TraceColumns: true}}
	if got, err := withCols.pending(ctx, "acme"); err != nil || got[1].Traceparent != tp || got[1].Causation == nil {
		t.Fatalf("relay with TraceColumns = %+v, %v", got, err)
	}
	without := &Relay{store: s, cfg: Config{Batch: 10}}
	if got, err := without.pending(ctx, "acme"); err != nil || got[1].Traceparent != "" || got[1].Causation != nil {
		t.Fatalf("relay without TraceColumns = %+v, %v", got, err)
	}
	// Without the option the relay reads what it always read.
	plain, err := s.FetchUnpublished(ctx, "acme", 10)
	if err != nil || len(plain) != 2 || plain[1].Traceparent != "" || plain[1].Causation != nil {
		t.Fatalf("FetchUnpublished = %+v, %v", plain, err)
	}
}

// A Core that never ran the migration keeps working exactly as before, and
// the two new calls fail loudly instead of dropping the metadata.
func TestLegacyOutboxWithoutTheColumns(t *testing.T) {
	dsn := testSchema(t)
	migrate(t, dsn, legacyOutbox)
	s := storeFor(t, dsn)
	ctx := context.Background()

	insert(t, s, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.Insert(ctx, tx, "acme", "x.plain.v1", []byte(`{}`))
		return err
	})
	if rows, err := s.FetchUnpublished(ctx, "acme", 10); err != nil || len(rows) != 1 {
		t.Fatalf("FetchUnpublished on a legacy outbox = %d, %v", len(rows), err)
	}
	if _, err := s.FetchUnpublishedWithMeta(ctx, "acme", 10); err == nil {
		t.Fatal("reading meta from an outbox without the columns must fail")
	}
	err := s.base.InTxFor(ctx, "acme", func(tx pgx.Tx) error {
		_, err := s.InsertWith(ctx, tx, "acme", "x.meta.v1", []byte(`{}`), Meta{Traceparent: "00-a-b-01"})
		return err
	})
	if err == nil {
		t.Fatal("InsertWith on an outbox without the columns must fail")
	}
}

// The migration is idempotent and reversible.
func TestTraceColumnsMigrationUpDownUp(t *testing.T) {
	dsn := testSchema(t)
	db := migrate(t, dsn, legacyOutbox, TraceColumnsMigration)
	ctx := context.Background()
	count := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'outbox' AND column_name IN ('traceparent','causation')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 2 {
		t.Fatal("columns missing after Up")
	}
	up := TraceColumnsMigration[:strings.Index(TraceColumnsMigration, "-- +goose Down")]
	if _, err := db.ExecContext(ctx, up); err != nil {
		t.Fatalf("re-running Up: %v", err)
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "00001_m.sql"), []byte(legacyOutbox), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "00002_m.sql"), []byte(TraceColumnsMigration), 0o600)
	if err := goose.Down(db, dir); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if count() != 0 {
		t.Fatal("columns still there after Down")
	}
}
