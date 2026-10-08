package tenancy

import (
	"sync"

	"github.com/jackc/pgx/v5"
)

// outboxMarks holds the transactions that wrote an outbox row and have not
// finished yet. A transaction enters with MarkOutbox and always leaves in
// runTx, committed or not, so the map stays as small as the number of
// transactions in flight.
var outboxMarks sync.Map // pgx.Tx -> struct{}

// MarkOutbox records that tx wrote an outbox row. If tx then commits through
// Base.InTx or Base.InTxFor, the listeners registered with OnOutboxCommit hear
// the tenant's name right after the commit.
//
// It is the outbox's Insert that calls it, so a Core gets the wake-up without
// touching its own code. A transaction that only reads never marks itself, and
// one that rolls back never notifies: the relay is woken for work that exists.
func MarkOutbox(tx pgx.Tx) {
	if tx != nil {
		outboxMarks.Store(tx, struct{}{})
	}
}

// OnOutboxCommit registers fn to be called, after the commit, with the tenant
// of every transaction that wrote an outbox row through this Base. fn runs on
// the committing goroutine, so it must be quick and must not block: the relay
// only flags the tenant and signals a channel.
func (b *Base) OnOutboxCommit(fn func(tenantID string)) {
	b.mu.Lock()
	b.onOutbox = append(b.onOutbox, fn)
	b.mu.Unlock()
}

func (b *Base) notifyOutbox(tenantID string) {
	b.mu.RLock()
	hooks := b.onOutbox
	b.mu.RUnlock()
	for _, fn := range hooks {
		fn(tenantID)
	}
}
