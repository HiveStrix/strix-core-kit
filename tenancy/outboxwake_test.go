package tenancy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// Needs a Postgres, like the pool tests:
//
//	TENANCY_TEST_PG='postgres://postgres:kit@127.0.0.1:55433/postgres?sslmode=disable' go test ./tenancy -run Wake -v
func wakeBase(t *testing.T) (*Base, *[]string, *sync.Mutex) {
	t.Helper()
	dsn := strings.Replace(testPG(t), "/postgres?", "/{slug}?", 1)
	p := NewPoolsWithSettings(TemplateResolver{Template: dsn, Schema: "public"}, 4, DefaultPoolSettings())
	t.Cleanup(p.Close)
	b := NewBase(p)
	var mu sync.Mutex
	var heard []string
	b.OnOutboxCommit(func(tenantID string) {
		mu.Lock()
		heard = append(heard, tenantID)
		mu.Unlock()
	})
	return b, &heard, &mu
}

func marksInFlight() int {
	n := 0
	outboxMarks.Range(func(_, _ any) bool { n++; return true })
	return n
}

// A transaction that wrote an outbox row and committed wakes its tenant, once,
// after the commit.
func TestWakeAfterACommitThatWroteAnEvent(t *testing.T) {
	b, heard, mu := wakeBase(t)
	ctx := tenantctx.WithTenant(context.Background(), "postgres")

	if err := b.InTx(ctx, func(tx pgx.Tx, _ string) error {
		MarkOutbox(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.InTxFor(context.Background(), "postgres", func(tx pgx.Tx) error {
		MarkOutbox(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*heard) != 2 || (*heard)[0] != "postgres" || (*heard)[1] != "postgres" {
		t.Fatalf("heard = %v, want the tenant once per committed transaction (InTx and InTxFor)", *heard)
	}
	if n := marksInFlight(); n != 0 {
		t.Errorf("%d marks left behind", n)
	}
}

// Reads wake nobody, and neither does a transaction that rolls back: the relay
// is woken only for work that exists.
func TestNoWakeForReadsNorForRollbacks(t *testing.T) {
	b, heard, mu := wakeBase(t)
	ctx := tenantctx.WithTenant(context.Background(), "postgres")

	if err := b.InTx(ctx, func(tx pgx.Tx, _ string) error {
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := b.InTx(ctx, func(tx pgx.Tx, _ string) error {
		MarkOutbox(tx)
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*heard) != 0 {
		t.Fatalf("heard = %v, want nobody", *heard)
	}
	if n := marksInFlight(); n != 0 {
		t.Errorf("%d marks left behind after a rollback", n)
	}
}
