package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/hs-javierviquez/strix-core-kit/natsconn"
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

	// Traceparent is the W3C trace context of the request that wrote the
	// event, so the consumer's work joins the same trace. Optional: absent
	// on an outbox without the columns, and on events written without it.
	Traceparent string `json:"traceparent,omitempty"`
	// Causation says which event and which principal led to this one, and
	// the chain of principals before it. Optional, like Traceparent. It is
	// what lets an agent that reacts to events notice it is reacting to its
	// own (NextCausation).
	Causation *Causation `json:"causation,omitempty"`
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
	// Interval between sweeps of a tenant that has events. Zero means 2s.
	Interval time.Duration
	// IdleInterval is the longest a tenant with no events waits between
	// sweeps: every empty sweep doubles its wait, from Interval up to this.
	// A commit that writes an event wakes its tenant at once, so a quiet
	// tenant does not delay the first event after the quiet. Zero means the
	// STRIX_RELAY_IDLE_INTERVAL environment variable, or 30s if unset. A value
	// <= Interval, or a negative one, turns the backoff off and sweeps every
	// tenant every Interval.
	IdleInterval time.Duration
	// Batch is how many events one tenant yields per sweep. Zero means 100.
	Batch int
	// TraceColumns says the outbox has the traceparent and causation columns
	// (TraceColumnsMigration applied), so the relay reads them and puts them
	// in the envelope. Off by default: a Core's outbox table is its own, and
	// reading columns it does not have would stop the relay cold.
	TraceColumns bool
	// Service is the Core's NATS identity (natsconn.WithService): the
	// connection's name and its own reply inbox, _INBOX.<Service>. Empty
	// falls back to NATS_SERVICE_NAME.
	Service string
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

	// sched and wakeCh are created by start, once, when Run begins.
	startOnce sync.Once
	sched     *sweepSchedule
	wakeCh    chan struct{}
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
	if cfg.IdleInterval == 0 {
		cfg.IdleInterval = idleIntervalFromEnv()
	}
	var opts []natsconn.Option
	if cfg.Service != "" {
		opts = append(opts, natsconn.WithService(cfg.Service))
	}
	nc, js, err := DialWith(natsURL, opts...)
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

// idleIntervalFromEnv reads STRIX_RELAY_IDLE_INTERVAL. A value <= 0 means "no
// backoff". An unreadable or missing one means the default.
func idleIntervalFromEnv() time.Duration {
	v := os.Getenv("STRIX_RELAY_IDLE_INTERVAL")
	if v == "" {
		return defaultIdleInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultIdleInterval
	}
	return d
}

// start builds the sweep schedule and hooks the relay to the Base, so a commit
// that wrote an outbox event wakes its tenant. It runs once, from Run.
func (r *Relay) start() {
	r.startOnce.Do(func() {
		idle := r.cfg.IdleInterval
		if idle == 0 {
			idle = idleIntervalFromEnv()
		}
		r.sched = newSweepSchedule(r.cfg.Interval, idle)
		r.wakeCh = make(chan struct{}, 1)
		if r.store != nil && r.store.base != nil {
			r.store.base.OnOutboxCommit(r.wake)
		}
	})
}

// wake is called, on the committing goroutine, after a transaction that wrote an
// event commits. It only flags the tenant and signals; the sweep happens in Run.
func (r *Relay) wake(tenantID string) {
	r.sched.wake(tenantID)
	select {
	case r.wakeCh <- struct{}{}:
	default: // a sweep is already pending: it will take this tenant too
	}
}

// Run sweeps until ctx is cancelled: every Interval for the tenants that are
// due, and at once for a tenant woken by a commit.
func (r *Relay) Run(ctx context.Context) {
	r.start()
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx, false)
		case <-r.wakeCh:
			r.sweep(ctx, true)
		}
	}
}

// sweep drains the tenants that are due (all of them on a tick) or, when woken
// is set, only the ones a commit just flagged.
func (r *Relay) sweep(ctx context.Context, woken bool) {
	if !r.ensureStream(ctx) {
		return
	}
	flagged := r.sched.takeWoken()
	if woken {
		for tenantID := range flagged {
			r.sweepTenant(ctx, tenantID)
		}
		return
	}
	tenants := r.tenants(ctx)
	r.sched.prune(tenants)
	now := time.Now()
	for _, tenantID := range tenants {
		_, wasWoken := flagged[tenantID]
		if wasWoken || r.sched.due(tenantID, now) {
			r.sweepTenant(ctx, tenantID)
		}
	}
}

func (r *Relay) sweepTenant(ctx context.Context, tenantID string) {
	busy := r.drain(ctx, tenantID)
	r.sched.done(tenantID, busy, time.Now())
}

// Close drains the NATS connection cleanly on SIGTERM, so in-flight publishes
// finish instead of being cut.
func (r *Relay) Close() {
	if r.nc != nil {
		_ = r.nc.Drain()
	}
}

// drain publishes one tenant's pending events and reports whether the tenant is
// busy: it had events, or something failed and must be retried at the base
// cadence. Only an empty, clean sweep reports false and lets the wait grow.
func (r *Relay) drain(ctx context.Context, tenantID string) bool {
	rows, err := r.pending(ctx, tenantID)
	if err != nil {
		r.fetchFailed(ctx, tenantID, err)
		return true
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
			return true
		}
		if err := r.store.MarkPublished(ctx, tenantID, row.ID); err != nil {
			// The event went out but the mark did not stick, so it will be
			// republished next tick. That is why consumers must be idempotent:
			// this is the exact duplicate they are guarding against.
			slog.ErrorContext(ctx, "relay: mark published failed, event will be redelivered", "event", row.EventID, "error", err)
			return true
		}
	}
	return len(rows) > 0
}

// pending is the next batch of one tenant, with traceparent and causation
// when the outbox has the columns.
func (r *Relay) pending(ctx context.Context, tenantID string) ([]Row, error) {
	return r.store.fetch(ctx, tenantID, r.cfg.Batch, r.cfg.TraceColumns)
}

// envelopeFor builds the wire envelope of an outbox row. occurred_at is the
// row's creation time, when the change committed, not the publish time: a
// relay that was down for an hour must not date an hour-old change "now".
//
// traceparent and causation go out only when the row has them, so an
// envelope from a Core without the columns is byte for byte what it was.
func envelopeFor(row Row) ([]byte, error) {
	env := Envelope{
		EventID:     row.EventID,
		TenantID:    row.TenantID,
		Subject:     row.Subject,
		OccurredAt:  row.CreatedAt.UTC().Format(time.RFC3339),
		Data:        row.Payload,
		Traceparent: row.Traceparent,
	}
	if len(row.Causation) > 0 {
		var c Causation
		if err := json.Unmarshal(row.Causation, &c); err != nil {
			return nil, fmt.Errorf("causation: %w", err)
		}
		env.Causation = &c
	}
	return json.Marshal(env)
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
