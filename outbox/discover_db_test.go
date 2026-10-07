package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/hs-javierviquez/strix-core-kit/tenancy"
)

// Discovery against a real Postgres, the way the provisioner lays tenants
// out: one database per tenant, CONNECT revoked from PUBLIC and granted to a
// Core's role only where its module is activated, and the Core's schema inside.
//
// Needs a Postgres the test may create roles and databases in (it only
// touches db_kitrelay_* and the role kitrelay_core):
//
//	docker run --rm -d --name kit-pg -e POSTGRES_PASSWORD=kit -p 55433:5432 postgres:16-alpine
//	OUTBOX_TEST_PG='postgres://postgres:kit@127.0.0.1:55433/postgres?sslmode=disable' go test ./outbox -run AgainstPostgres -v
//
// With OUTBOX_TEST_NATS set as well, TestRelayPublishesDiscoveredTenantsAgainstPostgres
// runs the whole relay against both.
const (
	kitRole   = "kitrelay_core"
	kitPass   = "kitrelay"
	kitSchema = "kitrelay"
)

type pgFixture struct {
	t        *testing.T
	admin    string // superuser DSN on the maintenance database
	template string // the Core's tenant DSN template
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	admin := os.Getenv("OUTBOX_TEST_PG")
	if admin == "" {
		t.Skip("OUTBOX_TEST_PG not set")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	// By hand: url.String() would escape the {slug} placeholder.
	template := fmt.Sprintf("postgres://%s:%s@%s/db_kitrelay_{slug}?%s", kitRole, kitPass, u.Host, u.RawQuery)
	f := &pgFixture{t: t, admin: admin, template: template}

	f.drop()
	t.Cleanup(f.drop)
	f.exec("postgres", fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, kitRole, kitPass))
	return f
}

func (f *pgFixture) dsnFor(db string) string {
	u, _ := url.Parse(f.admin)
	u.Path = "/" + db
	return u.String()
}

func (f *pgFixture) exec(db, sql string) {
	f.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, f.dsnFor(db))
	if err != nil {
		f.t.Fatalf("connect %s: %v", db, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		f.t.Fatalf("%s on %s: %v", sql, db, err)
	}
}

func (f *pgFixture) drop() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, f.admin)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	for _, slug := range []string{"on", "off", "late"} {
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS db_kitrelay_`+slug+` WITH (FORCE)`)
	}
	_, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+kitRole)
}

// tenant creates db_kitrelay_<slug> as the provisioner does: CONNECT revoked
// from PUBLIC, the Core's schema and outbox inside, owned by the Core's role.
func (f *pgFixture) tenant(slug string) {
	db := "db_kitrelay_" + slug
	f.exec("postgres", `CREATE DATABASE `+db)
	f.exec("postgres", `REVOKE CONNECT ON DATABASE `+db+` FROM PUBLIC`)
	f.exec(db, `CREATE SCHEMA `+kitSchema+` AUTHORIZATION `+kitRole)
	f.exec(db, `CREATE TABLE `+kitSchema+`.outbox (
		id bigserial PRIMARY KEY, tenant_id text NOT NULL,
		event_id uuid NOT NULL DEFAULT gen_random_uuid(), subject text NOT NULL,
		payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
		published_at timestamptz, attempts integer NOT NULL DEFAULT 0)`)
	f.exec(db, `ALTER TABLE `+kitSchema+`.outbox OWNER TO `+kitRole)
}

func (f *pgFixture) activate(slug string) {
	f.exec("postgres", `GRANT CONNECT ON DATABASE db_kitrelay_`+slug+` TO `+kitRole)
}

func (f *pgFixture) insert(slug, subject string) {
	f.exec("db_kitrelay_"+slug, fmt.Sprintf(
		`INSERT INTO %s.outbox (tenant_id, subject, payload) VALUES ('%s', '%s', '{}')`, kitSchema, slug, subject))
}

func (f *pgFixture) relay(cfg Config) *Relay {
	pools := tenancy.NewPools(tenancy.TemplateResolver{Template: f.template, Schema: kitSchema}, 8)
	f.t.Cleanup(pools.Close)
	cfg.Discover = ActiveTenants(f.template, kitSchema)
	return &Relay{store: New(tenancy.NewBase(pools)), cfg: cfg}
}

func TestDiscoveryAgainstPostgres(t *testing.T) {
	f := newPGFixture(t)
	logs := recordLogs(t)
	ctx := context.Background()

	f.tenant("on")
	f.activate("on")
	f.tenant("off") // the schema is there, the module is not: no CONNECT
	f.tenant("late")

	r := f.relay(Config{})
	if got := r.tenants(ctx); !slices.Equal(got, []string{"on"}) {
		t.Fatalf("tenants = %v, want only the activated one", got)
	}

	f.insert("on", "kitrelay.thing.happened.v1")
	rows, err := r.store.FetchUnpublished(ctx, "on", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("FetchUnpublished(on) = %d rows, %v; want the inserted row", len(rows), err)
	}

	// Activated while the relay runs: swept at the next refresh, no restart.
	f.activate("late")
	expire(r)
	if got := r.tenants(ctx); !slices.Equal(got, []string{"late", "on"}) {
		t.Fatalf("tenants = %v, want late to join", got)
	}

	// Deactivated by dropping the schema: its sweep fails once, quietly, and
	// the early refresh lets it go.
	f.exec("db_kitrelay_on", `DROP SCHEMA `+kitSchema+` CASCADE`)
	if _, err := r.store.FetchUnpublished(ctx, "on", 10); err == nil {
		t.Fatal("reading a dropped schema should fail")
	} else {
		r.fetchFailed(ctx, "on", err)
	}
	r.disc.lastTry = time.Now().Add(-earlyRefreshGap - time.Second)
	if got := r.tenants(ctx); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("tenants = %v, want on dropped", got)
	}

	// Deactivated by revoking CONNECT: gone at the next refresh, even though
	// the relay's pool for it is still open.
	f.exec("postgres", `REVOKE CONNECT ON DATABASE db_kitrelay_late FROM `+kitRole)
	expire(r)
	if got := r.tenants(ctx); len(got) != 0 {
		t.Fatalf("tenants = %v, want none", got)
	}

	if n := logs.count(slog.LevelError, ""); n != 0 {
		t.Fatalf("%d ERROR lines for tenants that only gained or lost the module, want 0", n)
	}
}

// The whole relay: events written in each activated tenant reach JetStream
// with their tenant in the envelope, and nothing is published for a tenant
// without the module.
func TestRelayPublishesDiscoveredTenantsAgainstPostgres(t *testing.T) {
	natsURL := os.Getenv("OUTBOX_TEST_NATS")
	if natsURL == "" {
		t.Skip("OUTBOX_TEST_NATS not set")
	}
	f := newPGFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.tenant("on")
	f.activate("on")
	f.tenant("off")
	f.insert("on", "kitrelay.thing.happened.v1")
	f.insert("off", "kitrelay.thing.happened.v1")

	pools := tenancy.NewPools(tenancy.TemplateResolver{Template: f.template, Schema: kitSchema}, 8)
	defer pools.Close()
	r, err := Connect(ctx, natsURL, New(tenancy.NewBase(pools)), Config{
		StreamName:    "KITRELAY_TEST",
		SubjectPrefix: "kitrelay.>",
		Interval:      200 * time.Millisecond,
		Discover:      ActiveTenants(f.template, kitSchema),
		DiscoverEvery: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = r.js.DeleteStream(ctx, "KITRELAY_TEST")
	t.Cleanup(func() { _ = r.js.DeleteStream(context.Background(), "KITRELAY_TEST") })
	r.streamReady = false
	go r.Run(ctx)

	// Activated after the relay started: its event must flow too.
	time.Sleep(time.Second)
	f.tenant("late")
	f.insert("late", "kitrelay.thing.happened.v1")
	f.activate("late")

	got := map[string]int{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && (got["on"] == 0 || got["late"] == 0) {
		time.Sleep(300 * time.Millisecond)
		stream, err := r.js.Stream(ctx, "KITRELAY_TEST")
		if err != nil {
			continue
		}
		cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{})
		if err != nil {
			continue
		}
		batch, err := cons.FetchNoWait(100)
		if err != nil {
			continue
		}
		got = map[string]int{}
		for msg := range batch.Messages() {
			var env Envelope
			if err := json.Unmarshal(msg.Data(), &env); err == nil {
				got[env.TenantID]++
			}
		}
	}
	if got["on"] != 1 || got["late"] != 1 || got["off"] != 0 {
		t.Fatalf("published per tenant = %v, want on:1 late:1 and nothing for off", got)
	}
}
