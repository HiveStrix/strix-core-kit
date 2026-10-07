package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Envelope is what actually travels on the wire.
//
// The tenant rides in the envelope, not in the payload: a consumer has to know
// whose data this is BEFORE it parses anything domain-specific, and a Core has
// one outbox per tenant database, so the subject alone cannot say.
//
// EventID is what makes consumers idempotent. It is stable across
// redeliveries — that is the whole contract.
type Envelope struct {
	EventID    string          `json:"event_id"`
	TenantID   string          `json:"tenant_id"`
	Subject    string          `json:"subject"`
	OccurredAt string          `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// Config names the Core's stream and the tenants to sweep.
type Config struct {
	// StreamName is the Core's JetStream stream, e.g. "EXPENSES".
	StreamName string
	// SubjectPrefix is every subject the Core publishes, e.g. "expenses.>".
	SubjectPrefix string
	// ExtraTenants are swept on top of those with a live pool. After a restart
	// a tenant with pending events but no traffic would otherwise never be
	// visited. With Discover they only matter until the first discovery
	// succeeds.
	ExtraTenants []string
	// Discover lists the tenants where this Core is activated (ActiveTenants
	// builds it from the tenant DSN template). When set, the relay sweeps
	// exactly those, re-asked every DiscoverEvery, instead of the tenants with
	// a live pool plus ExtraTenants.
	Discover func(ctx context.Context) ([]string, error)
	// DiscoverEvery is how often Discover is asked again. Zero means 1 minute.
	DiscoverEvery time.Duration
	// Interval between sweeps. Zero means 2s.
	Interval time.Duration
	// Batch is how many events one tenant yields per sweep. Zero means 100.
	Batch int
}

// Relay polls each tenant's outbox and publishes pending events to JetStream,
// then marks them published.
//
// It tolerates broker outages by design: rows stay in the table until delivery
// succeeds, and readiness is never coupled to the broker. A Core that refused
// to serve because NATS is down would take its writes with it, and capturing
// what the user did is the one thing that cannot wait.
type Relay struct {
	store *Store
	nc    *nats.Conn
	js    jetstream.JetStream
	cfg   Config

	// streamReady and warned are touched by Connect and then only by the Run
	// goroutine, never concurrently.
	streamReady bool
	warned      bool
	disc        discovery
}

// streamAttemptTimeout bounds one attempt at creating the stream, so a broker
// that accepts the TCP connection but does not answer cannot stall a sweep.
const streamAttemptTimeout = 5 * time.Second

// Connect dials NATS and ensures the stream exists.
//
// A nil Relay with a nil error is returned when natsURL is empty: events simply
// accumulate in the outbox and drain once a broker is configured. Nothing is
// lost, and nothing blocks.
//
// A broker that is not reachable YET is not an error either. The first dial
// retries in the background (RetryOnFailedConnect) and the stream is created
// by Run as soon as the broker answers. Without this, a DNS or NATS hiccup in
// the seconds a pod takes to start left that pod without a relay until its
// next restart, with every event parked in the outbox. The error return is
// kept for what retrying cannot fix: a malformed URL or invalid options.
func Connect(ctx context.Context, natsURL string, store *Store, cfg Config) (*Relay, error) {
	if natsURL == "" {
		return nil, nil
	}
	if cfg.Interval == 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Batch == 0 {
		cfg.Batch = 100
	}
	nc, js, err := Dial(natsURL)
	if err != nil {
		return nil, err
	}
	r := &Relay{store: store, nc: nc, js: js, cfg: cfg}
	// Best effort now, so a healthy broker is ready from the first sweep.
	r.ensureStream(ctx)
	return r, nil
}

// ensureStream creates the Core's stream once the broker is reachable and
// reports whether it exists. Until it does, sweeps publish nothing: the events
// stay in the outbox, which is exactly where they are safe.
func (r *Relay) ensureStream(ctx context.Context) bool {
	if r.streamReady {
		return true
	}
	if !r.nc.IsConnected() {
		r.warnOnce(ctx, "broker not reachable yet")
		return false
	}
	attempt, cancel := context.WithTimeout(ctx, streamAttemptTimeout)
	defer cancel()
	if _, err := r.js.CreateOrUpdateStream(attempt, jetstream.StreamConfig{
		Name:      r.cfg.StreamName,
		Subjects:  []string{r.cfg.SubjectPrefix},
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		r.warnOnce(ctx, err.Error())
		return false
	}
	if r.warned {
		slog.InfoContext(ctx, "relay: broker reachable, stream ready", "stream", r.cfg.StreamName)
	}
	r.streamReady = true
	return true
}

// warnOnce logs the first failure only: a broker that is down for an hour
// would otherwise write a line every sweep.
func (r *Relay) warnOnce(ctx context.Context, reason string) {
	if r.warned {
		return
	}
	r.warned = true
	slog.WarnContext(ctx, "relay: stream not ready, events stay in the outbox and are published once the broker answers",
		"stream", r.cfg.StreamName, "reason", reason)
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.ensureStream(ctx) {
				continue
			}
			for _, tenantID := range r.tenants(ctx) {
				r.drain(ctx, tenantID)
			}
		}
	}
}

// Close drains the NATS connection cleanly on SIGTERM, so in-flight publishes
// finish instead of being cut.
func (r *Relay) Close() {
	if r.nc != nil {
		_ = r.nc.Drain()
	}
}

func (r *Relay) drain(ctx context.Context, tenantID string) {
	rows, err := r.store.FetchUnpublished(ctx, tenantID, r.cfg.Batch)
	if err != nil {
		r.fetchFailed(ctx, tenantID, err)
		return
	}
	r.fetchOK(tenantID)
	for _, row := range rows {
		if err := publish(ctx, r.js, row); err != nil {
			slog.WarnContext(ctx, "relay: publish failed, will retry", "subject", row.Subject, "error", err)
			if bumpErr := r.store.BumpAttempts(ctx, tenantID, row.ID); bumpErr != nil {
				slog.ErrorContext(ctx, "relay: bump attempts failed", "error", bumpErr)
			}
			// Stop at the first failure instead of skipping ahead. The events of
			// one entity are a sequence, and delivering a later one first would
			// leave a consumer holding the older figure.
			return
		}
		if err := r.store.MarkPublished(ctx, tenantID, row.ID); err != nil {
			// The event went out but the mark did not stick, so it will be
			// republished next tick. That is why consumers must be idempotent:
			// this is the exact duplicate they are guarding against.
			slog.ErrorContext(ctx, "relay: mark published failed, event will be redelivered", "event", row.EventID, "error", err)
			return
		}
	}
}

// envelopeFor builds the wire envelope of an outbox row. occurred_at is the
// row's creation time, when the change committed, not the publish time: a
// relay that was down for an hour must not date an hour-old change "now".
func envelopeFor(row Row) ([]byte, error) {
	return json.Marshal(Envelope{
		EventID:    row.EventID,
		TenantID:   row.TenantID,
		Subject:    row.Subject,
		OccurredAt: row.CreatedAt.UTC().Format(time.RFC3339),
		Data:       row.Payload,
	})
}

// publish sends one outbox row. The event id travels as Nats-Msg-Id, so a
// republish inside JetStream's duplicate window (MarkPublished failed, or two
// replicas swept the same row) is dropped by the broker instead of reaching
// every consumer twice. Consumers stay idempotent regardless: the window is
// finite.
func publish(ctx context.Context, js jetstream.JetStream, row Row) error {
	envelope, err := envelopeFor(row)
	if err != nil {
		return fmt.Errorf("outbox: marshal envelope of %s: %w", row.EventID, err)
	}
	if _, err := js.Publish(ctx, row.Subject, envelope, jetstream.WithMsgID(row.EventID)); err != nil {
		return err
	}
	return nil
}
