package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/hs-javierviquez/strix-core-kit/auth"
	"github.com/hs-javierviquez/strix-core-kit/capabilities"
	authorizationv1 "github.com/hs-javierviquez/strix-core-kit/gen/authorization/v1"
)

// Runs against a real Postgres when KIT_TEST_DSN is set (see outbox's DB
// tests); each test gets a schema of its own.
func testPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("idem_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()
	dsn := u.String()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00001_idem.sql"), []byte(Migration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetLogger(goose.NopLogger())
	_ = goose.SetDialect("postgres")
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

const (
	writeMethod = "/billing.v1.InvoiceService/VoidInvoice"
	readMethod  = "/billing.v1.InvoiceService/ListInvoices"
)

var catalog = map[string]capabilities.Effect{
	writeMethod:                              capabilities.EffectWrite,
	readMethod:                               capabilities.EffectRead,
	"/billing.v1.InvoiceService/SendInvoice": capabilities.EffectExternalSideEffect,
}

func person(tenant string) *auth.Claims {
	return &auth.Claims{Subject: "u-1", ClientID: "strix-shell", TenantID: tenant, PrincipalTypeClaim: "user"}
}

func service() *auth.Claims {
	return &auth.Claims{Subject: "jev", ClientID: "jev", TenantID: "acme", PrincipalTypeClaim: "service"}
}

func agent() *auth.Claims {
	return &auth.Claims{Subject: "a-7", ClientID: "a-7", TenantID: "acme", PrincipalTypeClaim: "agent", InstallationID: "inst-7"}
}

func call(t *testing.T, i *Interceptor, c *auth.Claims, key, method string, req proto.Message, h grpc.UnaryHandler) (any, error) {
	t.Helper()
	ctx := context.Background()
	if c != nil {
		ctx = auth.ContextWithClaims(ctx, c)
	}
	if key != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(MetadataKey, key))
	}
	return i.Unary()(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, h)
}

// counting is a handler that answers with a fresh response each time, so a
// replay is distinguishable from a second execution.
func counting(n *atomic.Int64) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) {
		v := n.Add(1)
		return &authorizationv1.CheckPermissionResponse{Allowed: true, Reason: fmt.Sprintf("run-%d", v)}, nil
	}
}

func req(id string) proto.Message {
	return &authorizationv1.CheckPermissionRequest{TenantId: "acme", Action: "billing.invoice.void", ResourceId: id}
}

func TestMachinesMustSendAKeyOnWrites(t *testing.T) {
	i := New(SinglePool(nil), catalog) // never reaches the store
	var n atomic.Int64
	for name, c := range map[string]*auth.Claims{"service": service(), "agent": agent(),
		"legacy service": {Subject: "x", ClientID: "x", TenantID: "acme"},
		"unknown type":   {Subject: "x", TenantID: "acme", PrincipalTypeClaim: "robot"}} {
		for _, m := range []string{writeMethod, "/billing.v1.InvoiceService/SendInvoice", "/billing.v1.InvoiceService/Uncatalogued"} {
			_, err := call(t, i, c, "", m, req("1"), counting(&n))
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s %s without key: %v, want InvalidArgument", name, m, err)
			}
		}
		// Reads never need one.
		if _, err := call(t, i, c, "", readMethod, req("1"), counting(&n)); err != nil {
			t.Errorf("%s read without key: %v", name, err)
		}
	}
	if n.Load() != 4 {
		t.Fatalf("handler ran %d times, want only the 4 reads", n.Load())
	}
	// A person may write without a key, and nothing is recorded.
	if _, err := call(t, i, person("acme"), "", writeMethod, req("1"), counting(&n)); err != nil {
		t.Fatalf("person without key: %v", err)
	}
}

func TestMalformedKeys(t *testing.T) {
	i := New(SinglePool(nil), catalog)
	var n atomic.Int64
	for name, key := range map[string]string{"too long": strings.Repeat("k", MaxKeyLength+1), "space": "a b", "non ascii": "llavé"} {
		if _, err := call(t, i, person("acme"), key, writeMethod, req("1"), counting(&n)); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	ctx := metadata.NewIncomingContext(auth.ContextWithClaims(context.Background(), person("acme")), metadata.Pairs(MetadataKey, "a", MetadataKey, "b"))
	if _, err := i.Unary()(ctx, req("1"), &grpc.UnaryServerInfo{FullMethod: writeMethod}, counting(&n)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("two keys: %v", err)
	}
	if n.Load() != 0 {
		t.Fatal("the handler ran on a malformed key")
	}
}

func TestClassification(t *testing.T) {
	with := New(nil, catalog)
	if with.IsWrite(readMethod) || !with.IsWrite(writeMethod) || !with.IsWrite("/billing.v1.InvoiceService/SendInvoice") {
		t.Fatal("catalog classification wrong")
	}
	if !with.IsWrite("/billing.v1.InvoiceService/GetInvoice") {
		t.Fatal("with a catalog, an uncatalogued method is a write even if named like a read")
	}
	without := New(nil, nil)
	for m, write := range map[string]bool{
		"/a.v1.S/GetThing": false, "/a.v1.S/ListThings": false, "/a.v1.S/ReadThing": false,
		"/a.v1.S/CreateThing": true, "/a.v1.S/SearchThings": true, "/a.v1.S/LookupThings": true, "/a.v1.S/DeleteThing": true,
	} {
		if without.IsWrite(m) != write {
			t.Errorf("IsWrite(%q) without catalog = %v, want %v", m, !write, write)
		}
	}
	// The map is copied.
	m := map[string]capabilities.Effect{readMethod: capabilities.EffectRead}
	i := New(nil, m)
	m[readMethod] = capabilities.EffectWrite
	if i.IsWrite(readMethod) {
		t.Fatal("mutating the caller's map changed the interceptor")
	}
}

func TestSameKeyReplaysTheFirstResponse(t *testing.T) {
	i := New(SinglePool(testPool(t)), catalog)
	var n atomic.Int64
	first, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), counting(&n))
	if err != nil {
		t.Fatal(err)
	}
	second, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), counting(&n))
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", n.Load())
	}
	if !proto.Equal(first.(proto.Message), second.(proto.Message)) {
		t.Fatalf("replay = %v, want %v", second, first)
	}
	if second.(*authorizationv1.CheckPermissionResponse).GetReason() != "run-1" {
		t.Fatalf("replay is not the first response: %v", second)
	}
	// A person's key works the same.
	if _, err := call(t, i, person("acme"), "k-2", writeMethod, req("inv-1"), counting(&n)); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, i, person("acme"), "k-2", writeMethod, req("inv-1"), counting(&n)); err != nil || n.Load() != 2 {
		t.Fatalf("person replay: %v, runs %d", err, n.Load())
	}
}

func TestSameKeyForAnotherRequestIsRefused(t *testing.T) {
	i := New(SinglePool(testPool(t)), catalog)
	var n atomic.Int64
	if _, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), counting(&n)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() (any, error){
		"other body": func() (any, error) { return call(t, i, service(), "k-1", writeMethod, req("inv-2"), counting(&n)) },
		"other method": func() (any, error) {
			return call(t, i, service(), "k-1", "/billing.v1.InvoiceService/SendInvoice", req("inv-1"), counting(&n))
		},
		"other principal": func() (any, error) { return call(t, i, agent(), "k-1", writeMethod, req("inv-1"), counting(&n)) },
	}
	for name, f := range cases {
		if _, err := f(); status.Code(err) != codes.AlreadyExists {
			t.Errorf("%s: %v, want AlreadyExists", name, err)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", n.Load())
	}
}

func TestKeysAreScopedByTenant(t *testing.T) {
	i := New(SinglePool(testPool(t)), catalog)
	var n atomic.Int64
	for _, tenant := range []string{"acme", "globex"} {
		c := service()
		c.TenantID = tenant
		if _, err := call(t, i, c, "k-1", writeMethod, req("inv-1"), counting(&n)); err != nil {
			t.Fatalf("%s: %v", tenant, err)
		}
	}
	if n.Load() != 2 {
		t.Fatalf("handler ran %d times, want once per tenant", n.Load())
	}
}

func TestFailedHandlerReleasesTheKey(t *testing.T) {
	i := New(SinglePool(testPool(t)), catalog)
	boom := func(context.Context, any) (any, error) { return nil, status.Error(codes.FailedPrecondition, "nope") }
	if _, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), boom); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("the handler's error must reach the caller: %v", err)
	}
	var n atomic.Int64
	if _, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), counting(&n)); err != nil || n.Load() != 1 {
		t.Fatalf("retry after a failure must run: %v, runs %d", err, n.Load())
	}
}

func TestInFlightAndStaleKeys(t *testing.T) {
	pool := testPool(t)
	i := New(SinglePool(pool), catalog, StaleAfter(time.Minute))
	ctx := context.Background()

	// A concurrent duplicate while the first attempt runs: Aborted.
	release := make(chan struct{})
	started := make(chan struct{})
	var n atomic.Int64
	slow := func(c context.Context, r any) (any, error) {
		close(started)
		<-release
		return counting(&n)(c, r)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() {
		defer wg.Done()
		_, firstErr = call(t, i, service(), "k-1", writeMethod, req("inv-1"), slow)
	}()
	<-started
	if _, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), counting(&n)); status.Code(err) != codes.Aborted {
		t.Fatalf("duplicate in flight: %v, want Aborted", err)
	}
	close(release)
	wg.Wait()
	if firstErr != nil || n.Load() != 1 {
		t.Fatalf("first attempt: %v, runs %d", firstErr, n.Load())
	}

	// A completed key older than the stale window, but within the TTL,
	// still replays: only an in-progress key goes stale.
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '2 minutes' WHERE key = 'k-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, i, service(), "k-1", writeMethod, req("inv-1"), counting(&n)); err != nil || n.Load() != 1 {
		t.Fatalf("completed key past the stale window: %v, runs %d", err, n.Load())
	}

	// A key left in progress by a process that died is taken over once stale.
	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, key, method, request_hash, status, created_at)
		VALUES ('acme', 'k-dead', $1, '\x00', 'in_progress', now() - interval '2 minutes')`, writeMethod); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, i, service(), "k-dead", writeMethod, req("inv-1"), counting(&n)); err != nil || n.Load() != 2 {
		t.Fatalf("stale key: %v, runs %d", err, n.Load())
	}
	// But not while fresh.
	if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, key, method, request_hash, status)
		VALUES ('acme', 'k-fresh', $1, '\x00', 'in_progress')`, writeMethod); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, i, service(), "k-fresh", writeMethod, req("inv-1"), counting(&n)); status.Code(err) != codes.AlreadyExists && status.Code(err) != codes.Aborted {
		t.Fatalf("fresh foreign key: %v", err)
	}
	if n.Load() != 2 {
		t.Fatalf("a fresh in-progress key must not run: runs %d", n.Load())
	}
}

func TestExpiredKeysRunAgainAndAreCleanedUp(t *testing.T) {
	pool := testPool(t)
	i := New(SinglePool(pool), catalog, TTL(time.Hour))
	ctx := context.Background()
	var n atomic.Int64
	for _, k := range []string{"old", "new"} {
		if _, err := call(t, i, service(), k, writeMethod, req("inv-1"), counting(&n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '2 hours' WHERE key = 'old'`); err != nil {
		t.Fatal(err)
	}
	// Past the TTL the key is new again: even another request may use it.
	if _, err := call(t, i, service(), "old", writeMethod, req("inv-9"), counting(&n)); err != nil || n.Load() != 3 {
		t.Fatalf("expired key: %v, runs %d", err, n.Load())
	}
	if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '2 hours' WHERE key = 'old'`); err != nil {
		t.Fatal(err)
	}
	deleted, err := i.Cleanup(ctx, "acme")
	if err != nil || deleted != 1 {
		t.Fatalf("Cleanup = %d, %v; want 1", deleted, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&left)
	if left != 1 {
		t.Fatalf("keys left = %d, want 1", left)
	}
}

type brokenPools struct{}

func (brokenPools) PoolFor(context.Context, string) (*pgxpool.Pool, error) {
	return nil, errors.New("tenant db down")
}

// A write that cannot be made idempotent is not made.
func TestStoreUnavailableRefusesTheWrite(t *testing.T) {
	var n atomic.Int64
	if _, err := call(t, New(brokenPools{}, catalog), service(), "k-1", writeMethod, req("1"), counting(&n)); status.Code(err) != codes.Unavailable {
		t.Fatalf("broken store: %v, want Unavailable", err)
	}
	// A pool whose table does not exist fails the same way.
	pool := testPool(t)
	if _, err := pool.Exec(context.Background(), `DROP TABLE idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, New(SinglePool(pool), catalog), service(), "k-1", writeMethod, req("1"), counting(&n)); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing table: %v, want Unavailable", err)
	}
	if n.Load() != 0 {
		t.Fatal("the handler ran without a key store")
	}
}

func TestNoClaimsPassesThrough(t *testing.T) {
	var n atomic.Int64
	if _, err := call(t, New(brokenPools{}, catalog), nil, "", writeMethod, req("1"), counting(&n)); err != nil || n.Load() != 1 {
		t.Fatalf("no claims: %v", err)
	}
}
