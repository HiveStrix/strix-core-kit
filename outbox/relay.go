package outbox

import (
	"context"
	"encoding/json"
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
	// visited.
	ExtraTenants []string
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
}

// Connect dials NATS and ensures the stream exists.
//
// A nil Relay with a nil error is returned when natsURL is empty: events simply
// accumulate in the outbox and drain once a broker is configured. Nothing is
// lost, and nothing blocks.
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
	nc, err := nats.Connect(natsURL,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.ReconnectJitter(500*time.Millisecond, time.Second),
	)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cfg.StreamName,
		Subjects:  []string{cfg.SubjectPrefix},
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		nc.Close()
		return nil, err
	}
	return &Relay{store: store, nc: nc, js: js, cfg: cfg}, nil
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
			for _, tenantID := range r.tenants() {
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

func (r *Relay) tenants() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range append(r.store.KnownTenants(), r.cfg.ExtraTenants...) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (r *Relay) drain(ctx context.Context, tenantID string) {
	rows, err := r.store.FetchUnpublished(ctx, tenantID, r.cfg.Batch)
	if err != nil {
		slog.ErrorContext(ctx, "relay: fetch outbox failed", "tenant", tenantID, "error", err)
		return
	}
	for _, row := range rows {
		envelope, err := json.Marshal(Envelope{
			EventID:    row.EventID,
			TenantID:   row.TenantID,
			Subject:    row.Subject,
			OccurredAt: time.Now().UTC().Format(time.RFC3339),
			Data:       row.Payload,
		})
		if err != nil {
			slog.ErrorContext(ctx, "relay: marshal envelope failed", "event", row.EventID, "error", err)
			return
		}

		if _, err := r.js.Publish(ctx, row.Subject, envelope); err != nil {
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
