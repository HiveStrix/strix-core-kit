// Package outbox is the transactional outbox and the relay that drains it
// (SCC §9).
//
// The outbox is what makes publication reliable: the event is written in the
// SAME transaction as the data change, so either both happen or neither does.
// Publishing outside the transaction — "publish and pray" — is how a consumer
// ends up believing a figure that was never committed, or missing one that was.
package outbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hs-javierviquez/strix-core-kit/tenancy"
)

// ErrAlreadyProcessed is returned when an event was already applied.
var ErrAlreadyProcessed = errors.New("outbox: event already processed")

// Row is an unpublished outbox event.
type Row struct {
	ID       int64
	EventID  string
	TenantID string
	Subject  string
	Payload  []byte
}

// Store reads and writes the outbox and processed_events tables. Both are part
// of the ecosystem contract rather than any Core's domain, so they live here and
// every Core gets the same semantics.
type Store struct {
	base *tenancy.Base
}

// New builds the outbox store over a Core's tenancy base.
func New(base *tenancy.Base) *Store {
	return &Store{base: base}
}

// KnownTenants lists the tenants with a live pool, which is what the relay
// sweeps.
func (s *Store) KnownTenants() []string {
	return s.base.Pools().KnownTenants()
}

// Insert appends an event to the outbox INSIDE the caller's transaction.
//
// This is the whole point of the outbox: the event and the data change commit
// together or neither does.
func (s *Store) Insert(ctx context.Context, tx pgx.Tx, tenantID, subject string, payload []byte) (string, error) {
	const q = `
		INSERT INTO outbox (tenant_id, subject, payload)
		VALUES ($1, $2, $3::jsonb)
		RETURNING event_id`
	var eventID string
	if err := tx.QueryRow(ctx, q, tenantID, subject, payload).Scan(&eventID); err != nil {
		return "", fmt.Errorf("outbox: insert: %w", err)
	}
	return eventID, nil
}

// FetchUnpublished returns up to limit unpublished events of one tenant, oldest
// first.
//
// Ordered by id and drained in order: the events describing one entity's
// history are a sequence, and delivering the second before the first would
// leave a consumer believing the older figure.
func (s *Store) FetchUnpublished(ctx context.Context, tenantID string, limit int) ([]Row, error) {
	pool, err := s.base.PoolFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	const q = `
		SELECT id, event_id, tenant_id, subject, payload::text
		FROM outbox
		WHERE published_at IS NULL AND tenant_id = $1
		ORDER BY id
		LIMIT $2`
	rows, err := pool.Query(ctx, q, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: fetch: %w", err)
	}
	defer rows.Close()

	out := []Row{}
	for rows.Next() {
		var row Row
		var payload string
		if err := rows.Scan(&row.ID, &row.EventID, &row.TenantID, &row.Subject, &payload); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		row.Payload = []byte(payload)
		out = append(out, row)
	}
	return out, rows.Err()
}

// MarkPublished flags an event as delivered.
func (s *Store) MarkPublished(ctx context.Context, tenantID string, id int64) error {
	pool, err := s.base.PoolFor(ctx, tenantID)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = $1 AND tenant_id = $2`, id, tenantID); err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}

// BumpAttempts increments the retry counter of an event.
//
// The row is never dropped, however many attempts it accumulates. An event that
// cannot be delivered is a problem to look at, not a row to discard: discarding
// it is how a downstream figure silently stops being recalculated.
func (s *Store) BumpAttempts(ctx context.Context, tenantID string, id int64) error {
	pool, err := s.base.PoolFor(ctx, tenantID)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET attempts = attempts + 1 WHERE id = $1 AND tenant_id = $2`, id, tenantID); err != nil {
		return fmt.Errorf("outbox: bump attempts: %w", err)
	}
	return nil
}

// MarkProcessed records that an event was applied, INSIDE the caller's
// transaction.
//
// Returns ErrAlreadyProcessed when the event_id is already there, which is the
// signal to roll back the effect rather than apply it twice. The uniqueness of
// the primary key is what makes this race-free: two concurrent deliveries of
// the same event cannot both insert.
func (s *Store) MarkProcessed(ctx context.Context, tx pgx.Tx, tenantID, eventID, subject string) error {
	const q = `
		INSERT INTO processed_events (event_id, tenant_id, subject)
		VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING event_id`
	var got string
	err := tx.QueryRow(ctx, q, eventID, tenantID, subject).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAlreadyProcessed
	}
	if err != nil {
		return fmt.Errorf("outbox: mark processed: %w", err)
	}
	return nil
}
