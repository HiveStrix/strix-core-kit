package outbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/hs-javierviquez/strix-core-kit/tenancy"
)

// streamCount is how many messages the stream holds.
func streamCount(ctx context.Context, r *Relay, name string) int {
	stream, err := r.js.Stream(ctx, name)
	if err != nil {
		return 0
	}
	cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{})
	if err != nil {
		return 0
	}
	batch, err := cons.FetchNoWait(100)
	if err != nil {
		return 0
	}
	n := 0
	for range batch.Messages() {
		n++
	}
	return n
}

// The reason for the adaptive wait to be safe: a tenant that has been quiet long
// enough to be swept only every IdleInterval still gets its first event out at
// once, because the commit that wrote it wakes the relay.
//
// Needs Postgres and NATS:
//
//	docker run --rm -d --name kit-pg -e POSTGRES_PASSWORD=kit -p 55433:5432 postgres:16-alpine
//	docker run --rm -d --name kit-nats -p 14222:4222 nats:2.12-alpine -js
//	OUTBOX_TEST_PG='postgres://postgres:kit@127.0.0.1:55433/postgres?sslmode=disable' OUTBOX_TEST_NATS=nats://127.0.0.1:14222 go test ./outbox -run Wakes -v
func TestRelayWakesAQuietTenantOnCommit(t *testing.T) {
	natsURL := os.Getenv("OUTBOX_TEST_NATS")
	if natsURL == "" {
		t.Skip("OUTBOX_TEST_NATS not set")
	}
	f := newPGFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.tenant("on")
	f.activate("on")

	const idle = 1600 * time.Millisecond
	pools := tenancy.NewPools(tenancy.TemplateResolver{Template: f.template, Schema: kitSchema}, 8)
	defer pools.Close()
	base := tenancy.NewBase(pools)
	store := New(base)
	r, err := Connect(ctx, natsURL, store, Config{
		StreamName:    "KITRELAY_WAKE",
		SubjectPrefix: "kitrelay.>",
		Interval:      100 * time.Millisecond,
		IdleInterval:  idle,
		Discover:      ActiveTenants(f.template, kitSchema),
		DiscoverEvery: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = r.js.DeleteStream(ctx, "KITRELAY_WAKE")
	t.Cleanup(func() { _ = r.js.DeleteStream(context.Background(), "KITRELAY_WAKE") })
	r.streamReady = false
	r.start() // before Run, so the test can read the schedule without a race
	go r.Run(ctx)

	// Let the tenant go quiet: every empty sweep doubles its wait up to idle.
	deadline := time.Now().Add(15 * time.Second)
	for r.sched.intervalOf("on") < idle {
		if time.Now().After(deadline) {
			t.Fatalf("the quiet tenant never reached its idle wait, got %v", r.sched.intervalOf("on"))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An event written through the Base, the way a Core writes it.
	start := time.Now()
	if err := base.InTxFor(ctx, "on", func(tx pgx.Tx) error {
		_, err := store.Insert(ctx, tx, "on", "kitrelay.thing.happened.v1", []byte(`{}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for streamCount(ctx, r, "KITRELAY_WAKE") == 0 {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the event was never published")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Well under the wait the tenant had: it was the wake-up, not the sweep.
	if got := time.Since(start); got > idle/2 {
		t.Fatalf("published after %v; with the tenant waiting %v it must be woken by the commit (< %v)", got, idle, idle/2)
	}
	// And the busy sweep brought it back to the base cadence.
	deadline = time.Now().Add(2 * time.Second)
	for r.sched.intervalOf("on") > 400*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatalf("after publishing, the tenant's wait is still %v", r.sched.intervalOf("on"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
