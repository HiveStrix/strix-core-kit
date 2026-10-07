package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/propagation"
)

// TraceColumnsMigration adds the optional traceparent and causation columns
// to a Core's outbox (contract AI ready, proposal §2.5). The outbox table is
// each Core's own migration, so the kit cannot add them itself: the Core
// copies this into its next goose file and, once it is applied everywhere,
// sets Config.TraceColumns and writes with InsertWith.
//
// Both columns are nullable and the statement is idempotent: rows written
// before it simply carry neither, and the relay leaves them out of the
// envelope. The table name is unqualified, resolved by the migration's
// search_path; a Core whose migrations qualify the schema
// ("billing.outbox") qualifies it here too.
const TraceColumnsMigration = `-- +goose Up
ALTER TABLE outbox
    ADD COLUMN IF NOT EXISTS traceparent text,
    ADD COLUMN IF NOT EXISTS causation   jsonb;

-- +goose Down
ALTER TABLE outbox
    DROP COLUMN IF EXISTS causation,
    DROP COLUMN IF EXISTS traceparent;
`

// Causation is why an event exists: the event that led to it, who acted on
// that event, and the chain of principals along the way, oldest first.
type Causation struct {
	// EventID is the event whose handling produced this one.
	EventID string `json:"event_id"`
	// PrincipalType and PrincipalID name who acted ("agent", "inst-7").
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	// Chain is every principal on the causal path, oldest first, the acting
	// one last.
	Chain []string `json:"chain"`
}

// Meta is what InsertWith writes besides the event itself.
type Meta struct {
	// Traceparent is the W3C trace context of the current request
	// (telemetry.Traceparent(ctx)). Empty is NULL.
	Traceparent string
	// Causation is nil for an event no other event caused (a person's
	// request).
	Causation *Causation
}

// DefaultMaxCausationDepth bounds a causal chain when the caller passes no
// bound of its own: eight hops of event -> reaction -> event is far beyond
// any saga the platform runs, and well short of a runaway loop's damage.
const DefaultMaxCausationDepth = 8

// The two ways NextCausation refuses to extend a chain.
var (
	ErrCausationTooDeep = errors.New("outbox: causation chain too deep")
	ErrCausationCycle   = errors.New("outbox: causation chain has a cycle")
)

// NextCausation is the Causation of an event a principal emits while handling
// trigger (the event it reacts to, whose own causation is triggerCausation,
// nil for a root event).
//
// It is the agent loop guard. It refuses — returns an error and no Causation
// — when principalID is already in the chain (the principal would be reacting,
// directly or through others, to its own doing) or when the new chain would
// be longer than maxDepth (<= 0 means DefaultMaxCausationDepth). The caller
// then does NOT act: it acks the trigger and logs, which ends the loop.
func NextCausation(triggerEventID string, triggerCausation *Causation, principalType, principalID string, maxDepth int) (*Causation, error) {
	if maxDepth <= 0 {
		maxDepth = DefaultMaxCausationDepth
	}
	if principalID == "" {
		return nil, errors.New("outbox: causation needs a principal id")
	}
	var prev []string
	if triggerCausation != nil {
		if err := triggerCausation.Validate(maxDepth); err != nil {
			return nil, err
		}
		prev = triggerCausation.Chain
	}
	for _, p := range prev {
		if p == principalID {
			return nil, fmt.Errorf("%w: %q already acted on this chain %v", ErrCausationCycle, principalID, prev)
		}
	}
	if len(prev)+1 > maxDepth {
		return nil, fmt.Errorf("%w: %d hops, the limit is %d", ErrCausationTooDeep, len(prev)+1, maxDepth)
	}
	chain := append(append(make([]string, 0, len(prev)+1), prev...), principalID)
	return &Causation{EventID: triggerEventID, PrincipalType: principalType, PrincipalID: principalID, Chain: chain}, nil
}

// Validate reports whether an incoming chain is already longer than maxDepth
// (<= 0 means DefaultMaxCausationDepth) or repeats a principal. A consumer
// may refuse to act on such an event even when it would not extend it.
func (c *Causation) Validate(maxDepth int) error {
	if c == nil {
		return nil
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxCausationDepth
	}
	if len(c.Chain) > maxDepth {
		return fmt.Errorf("%w: %d hops, the limit is %d", ErrCausationTooDeep, len(c.Chain), maxDepth)
	}
	seen := make(map[string]bool, len(c.Chain))
	for _, p := range c.Chain {
		if seen[p] {
			return fmt.Errorf("%w: %q appears twice in %v", ErrCausationCycle, p, c.Chain)
		}
		seen[p] = true
	}
	return nil
}

// InsertWith is Insert that also writes traceparent and causation, INSIDE the
// caller's transaction. It needs the columns of TraceColumnsMigration; on an
// outbox without them the insert fails, and so does the caller's transaction,
// which is the point: a Core that switched to InsertWith before migrating
// finds out at once instead of losing the metadata quietly.
func (s *Store) InsertWith(ctx context.Context, tx pgx.Tx, tenantID, subject string, payload []byte, meta Meta) (string, error) {
	var traceparent, causation *string
	if meta.Traceparent != "" {
		traceparent = &meta.Traceparent
	}
	if meta.Causation != nil {
		b, err := json.Marshal(meta.Causation)
		if err != nil {
			return "", fmt.Errorf("outbox: marshal causation: %w", err)
		}
		c := string(b)
		causation = &c
	}
	const q = `
		INSERT INTO outbox (tenant_id, subject, payload, traceparent, causation)
		VALUES ($1, $2, $3::jsonb, $4, $5::jsonb)
		RETURNING event_id`
	var eventID string
	if err := tx.QueryRow(ctx, q, tenantID, subject, payload, traceparent, causation).Scan(&eventID); err != nil {
		return "", fmt.Errorf("outbox: insert: %w", err)
	}
	return eventID, nil
}

type envelopeKey struct{}

// ParseEnvelope decodes what a consumer received.
func ParseEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("outbox: parse envelope: %w", err)
	}
	return env, nil
}

// ContextWithEnvelope carries a received envelope's traceparent and causation
// through the handler, so whatever it writes can name what caused it
// (CausationFrom, TraceparentFrom) and NextCausation can see the chain.
//
// The traceparent also becomes the remote parent of the spans the handler
// starts, so the consumer's work shows up in the producer's trace.
func ContextWithEnvelope(ctx context.Context, env Envelope) context.Context {
	if env.Traceparent != "" {
		ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": env.Traceparent})
	}
	return context.WithValue(ctx, envelopeKey{}, env)
}

// EnvelopeFrom returns the envelope ContextWithEnvelope stored.
func EnvelopeFrom(ctx context.Context) (Envelope, bool) {
	env, ok := ctx.Value(envelopeKey{}).(Envelope)
	return env, ok
}

// TraceparentFrom returns the traceparent of the event being handled, "" when
// there is none.
func TraceparentFrom(ctx context.Context) string {
	env, _ := EnvelopeFrom(ctx)
	return env.Traceparent
}

// CausationFrom returns the causation of the event being handled and its
// event id, for NextCausation.
func CausationFrom(ctx context.Context) (eventID string, c *Causation, ok bool) {
	env, ok := EnvelopeFrom(ctx)
	if !ok {
		return "", nil, false
	}
	return env.EventID, env.Causation, true
}
