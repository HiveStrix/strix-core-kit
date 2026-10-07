package outbox

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hs-javierviquez/strix-core-kit/tenancy"
)

// logRecorder keeps what the relay logs, so the tests can assert on the noise
// as well as on the behaviour.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

func (h *logRecorder) count(level slog.Level, msgPart string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, msgPart) {
			n++
		}
	}
	return n
}

func recordLogs(t *testing.T) *logRecorder {
	t.Helper()
	h := &logRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// newOfflineRelay builds a relay whose store never connects anywhere: enough
// for the tenant bookkeeping, which is all these tests exercise.
func newOfflineRelay(cfg Config) *Relay {
	pools := tenancy.NewPools(tenancy.TemplateResolver{
		Template: "postgres://nobody@127.0.0.1:1/db_tenant_{slug}", Schema: "s",
	}, 4)
	return &Relay{store: New(tenancy.NewBase(pools)), cfg: cfg}
}

// answers is a Discover that returns its answers in order, repeating the
// last one, and counts how often it was asked.
type answers struct {
	calls int
	list  [][]string
	err   error
}

func (a *answers) discover(context.Context) ([]string, error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	i := min(a.calls, len(a.list)) - 1
	return a.list[i], nil
}

// expire makes the next tenants() call refresh, as if DiscoverEvery passed.
func expire(r *Relay) { r.disc.lastTry = time.Now().Add(-time.Hour) }

func TestWithoutDiscoverTheRelaySweepsExtraTenants(t *testing.T) {
	r := newOfflineRelay(Config{ExtraTenants: []string{"demo", "", "demo", "demojavi"}})
	if got := r.tenants(context.Background()); !slices.Equal(got, []string{"demo", "demojavi"}) {
		t.Fatalf("tenants = %v, want [demo demojavi]", got)
	}
}

func TestDiscoveredTenantsReplaceExtraTenants(t *testing.T) {
	logs := recordLogs(t)
	a := &answers{list: [][]string{{"acme", "testhcm"}}}
	r := newOfflineRelay(Config{ExtraTenants: []string{"demo"}, Discover: a.discover})
	if got := r.tenants(context.Background()); !slices.Equal(got, []string{"acme", "testhcm"}) {
		t.Fatalf("tenants = %v, want the discovered ones, not ExtraTenants", got)
	}
	if logs.count(slog.LevelInfo, "ExtraTenants ignored") != 1 {
		t.Fatal("ignoring a configured ExtraTenants must be said once")
	}
}

func TestDiscoveryIsAskedOncePerInterval(t *testing.T) {
	a := &answers{list: [][]string{{"acme"}}}
	r := newOfflineRelay(Config{Discover: a.discover})
	ctx := context.Background()
	r.tenants(ctx)
	r.tenants(ctx)
	if a.calls != 1 {
		t.Fatalf("Discover asked %d times inside one interval, want 1: every sweep would query Postgres", a.calls)
	}
	expire(r)
	r.tenants(ctx)
	if a.calls != 2 {
		t.Fatalf("Discover asked %d times after the interval, want 2", a.calls)
	}
}

// A tenant activated while the pod runs starts being swept at the next
// refresh, without a restart and without editing any spec.
func TestTenantActivatedWhileRunningJoinsOnRefresh(t *testing.T) {
	logs := recordLogs(t)
	a := &answers{list: [][]string{{"demo"}, {"demo", "testhcm"}}}
	r := newOfflineRelay(Config{Discover: a.discover})
	ctx := context.Background()
	r.tenants(ctx)
	expire(r)
	if got := r.tenants(ctx); !slices.Equal(got, []string{"demo", "testhcm"}) {
		t.Fatalf("tenants = %v, want testhcm to join", got)
	}
	if logs.count(slog.LevelInfo, "tenant activated") != 1 {
		t.Fatal("a newly swept tenant must be logged once")
	}
}

// If discovery has never answered, the relay must not be worse than before
// it existed: it sweeps the tenants with a live pool and ExtraTenants, and
// says so once, not every sweep.
func TestFailedDiscoveryBeforeAnySuccessFallsBack(t *testing.T) {
	logs := recordLogs(t)
	a := &answers{err: errors.New("connection refused")}
	r := newOfflineRelay(Config{ExtraTenants: []string{"demo"}, Discover: a.discover})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if got := r.tenants(ctx); !slices.Equal(got, []string{"demo"}) {
			t.Fatalf("tenants = %v, want the fallback [demo]", got)
		}
		expire(r)
	}
	if n := logs.count(slog.LevelWarn, "tenant discovery failed"); n != 1 {
		t.Fatalf("discovery failure logged %d times, want once", n)
	}
}

func TestFailedDiscoveryKeepsTheLastAnswer(t *testing.T) {
	recordLogs(t)
	a := &answers{list: [][]string{{"acme"}}}
	r := newOfflineRelay(Config{ExtraTenants: []string{"demo"}, Discover: a.discover})
	ctx := context.Background()
	r.tenants(ctx)
	a.err = errors.New("timeout")
	expire(r)
	if got := r.tenants(ctx); !slices.Equal(got, []string{"acme"}) {
		t.Fatalf("tenants = %v, want the last discovered set [acme]", got)
	}
}

// A tenant that loses the module fails its sweep once and is dropped at the
// early refresh, without an ERROR every two seconds in between.
func TestDeactivatedTenantIsDroppedQuietly(t *testing.T) {
	logs := recordLogs(t)
	a := &answers{list: [][]string{{"demo", "payroll-less"}, {"demo"}}}
	r := newOfflineRelay(Config{Discover: a.discover})
	ctx := context.Background()
	r.tenants(ctx)

	for i := 0; i < 4; i++ { // several sweeps before the early refresh is allowed
		r.fetchFailed(ctx, "payroll-less", errors.New("permission denied for database (SQLSTATE 42501)"))
		r.tenants(ctx)
	}
	if a.calls != 1 {
		t.Fatalf("Discover asked %d times inside earlyRefreshGap, want 1", a.calls)
	}
	r.disc.lastTry = time.Now().Add(-earlyRefreshGap - time.Second)
	if got := r.tenants(ctx); !slices.Equal(got, []string{"demo"}) {
		t.Fatalf("tenants = %v, want the deactivated tenant dropped", got)
	}
	if n := logs.count(slog.LevelError, "fetch outbox failed"); n != 0 {
		t.Fatalf("%d ERROR lines for a tenant that only lost the module, want 0", n)
	}
	if logs.count(slog.LevelInfo, "no longer activated") != 1 {
		t.Fatal("dropping a tenant must be logged once")
	}
}

// A tenant that keeps failing while still activated has a real problem, and
// it must be as loud as it was without discovery.
func TestFailingTenantStillActivatedIsLogged(t *testing.T) {
	logs := recordLogs(t)
	a := &answers{list: [][]string{{"demo"}}}
	r := newOfflineRelay(Config{Discover: a.discover})
	ctx := context.Background()
	r.tenants(ctx)

	r.fetchFailed(ctx, "demo", errors.New("relation outbox does not exist"))
	if logs.count(slog.LevelError, "fetch outbox failed") != 0 {
		t.Fatal("the first failure must wait for the refresh to tell")
	}
	r.disc.lastTry = time.Now().Add(-earlyRefreshGap - time.Second)
	r.tenants(ctx)
	r.fetchFailed(ctx, "demo", errors.New("relation outbox does not exist"))
	if logs.count(slog.LevelError, "fetch outbox failed") != 1 {
		t.Fatal("a failing tenant the refresh still finds activated must be logged")
	}

	r.fetchOK("demo")
	r.fetchFailed(ctx, "demo", errors.New("blip"))
	if logs.count(slog.LevelError, "fetch outbox failed") != 1 {
		t.Fatal("after recovering, a new failure starts as suspect again")
	}
}
