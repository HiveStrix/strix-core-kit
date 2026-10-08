package tenancy

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Needs a Postgres where the test may connect as superuser:
//
//	docker run --rm -d --name kit-pg -e POSTGRES_PASSWORD=kit -p 55433:5432 postgres:16-alpine
//	TENANCY_TEST_PG='postgres://postgres:kit@127.0.0.1:55433/postgres?sslmode=disable' go test ./tenancy -run Pool -v
func testPG(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TENANCY_TEST_PG")
	if dsn == "" {
		t.Skip("TENANCY_TEST_PG is not set")
	}
	return dsn
}

func testPools(t *testing.T, s PoolSettings) *Pools {
	t.Helper()
	p := NewPoolsWithSettings(fixedDSN(testPG(t)), 8, s)
	t.Cleanup(p.Close)
	return p
}

type fixedDSN string

func (f fixedDSN) Resolve(context.Context, string) (string, error) { return string(f), nil }

// connsOf counts the server-side connections carrying an application_name.
func connsOf(t *testing.T, app string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testPG(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, app).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPoolNeverExceedsMaxConns(t *testing.T) {
	s := DefaultPoolSettings()
	s.MaxConns = 2
	s.AppName = "kit-pool-max"
	p := testPools(t, s)
	ctx := context.Background()

	pool, err := p.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var x int
			_ = pool.QueryRow(ctx, `SELECT 1 FROM pg_sleep(0.2)`).Scan(&x)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if got := connsOf(t, "kit-pool-max"); got > 2 {
		t.Errorf("server connections = %d, want <= 2", got)
	}
	wg.Wait()
}

func TestPoolReleasesIdleConnections(t *testing.T) {
	s := DefaultPoolSettings()
	s.MaxIdle = 300 * time.Millisecond
	s.HealthPeriod = 100 * time.Millisecond
	s.IdleClose = 0
	s.AppName = "kit-pool-idle"
	p := testPools(t, s)

	if _, err := p.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for connsOf(t, "kit-pool-idle") > 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle connections were never closed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestUnusedTenantReleasesItsPoolAndReopens(t *testing.T) {
	s := DefaultPoolSettings()
	s.IdleClose = 400 * time.Millisecond
	s.AppName = "kit-pool-reap"
	p := testPools(t, s)
	ctx := context.Background()

	if _, err := p.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	// The relay polling "a" must not keep it alive; real traffic on "b" must.
	deadline := time.Now().Add(5 * time.Second)
	for len(p.KnownTenants()) > 1 {
		if time.Now().After(deadline) {
			t.Fatalf("tenants still pooled: %v", p.KnownTenants())
		}
		if _, err := p.GetBackground(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Get(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if k := p.KnownTenants(); len(k) != 1 || k[0] != "b" {
		t.Fatalf("KnownTenants = %v, want [b]", k)
	}

	// First request after the release opens it again and works.
	pool, err := p.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("query on the reopened pool: %v (%d)", err, one)
	}
}

func TestPoolSettingsFromEnv(t *testing.T) {
	t.Setenv(EnvPoolMaxConns, "5")
	t.Setenv(EnvPoolMinConns, "9")
	t.Setenv(EnvPoolMaxIdle, "45s")
	t.Setenv(EnvPoolIdleClose, "not-a-duration")
	t.Setenv(EnvAppName, "")
	t.Setenv("NATS_SERVICE_NAME", "core-x")

	s := PoolSettingsFromEnv()
	if s.MaxConns != 5 || s.MinConns != 5 {
		t.Errorf("conns = %d/%d, want min clamped to max 5/5", s.MinConns, s.MaxConns)
	}
	if s.MaxIdle != 45*time.Second {
		t.Errorf("MaxIdle = %v", s.MaxIdle)
	}
	if s.IdleClose != DefaultPoolSettings().IdleClose {
		t.Errorf("an unreadable value must keep the default, got %v", s.IdleClose)
	}
	if s.AppName != "core-x" {
		t.Errorf("AppName = %q, want the NATS_SERVICE_NAME fallback", s.AppName)
	}
}

func TestPodAppName(t *testing.T) {
	cases := map[string]string{
		"strix-people-6d8f9c7b5-abcde":     "strix-people",
		"strix-hcm-rules-5f6c7d8b9c-x2k4m": "strix-hcm-rules",
		"postgres-0":                       "postgres",
		"DESKTOP-ABC123":                   "",
		"laptop":                           "",
	}
	for host, want := range cases {
		if got := podAppName(host); got != want {
			t.Errorf("podAppName(%q) = %q, want %q", host, got, want)
		}
	}
}
