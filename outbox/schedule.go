package outbox

import (
	"sync"
	"time"
)

// Adaptive sweeping.
//
// The relay used to poll every activated tenant every Interval (2 s). That
// keeps one database connection alive per Core, tenant and replica forever,
// even for a tenant nobody has touched in weeks, and the total grows with
// Cores x replicas x tenants (the 53300 of 2026-10-08).
//
// Now each tenant has its own cadence. While it has events it is swept every
// Interval; every empty sweep doubles its wait, up to IdleInterval. And a
// commit that wrote an event wakes that tenant at once, so a quiet tenant does
// not delay the first event after the quiet. Between two sweeps of a quiet
// tenant no connection of its own is in use, which is what lets a pooler or the
// pool's idle timeout give it back.

// defaultIdleInterval is the longest a tenant with no events waits between
// sweeps, when Config.IdleInterval and STRIX_RELAY_IDLE_INTERVAL say nothing.
const defaultIdleInterval = 30 * time.Second

type tenantSweep struct {
	interval time.Duration
	next     time.Time
}

// sweepSchedule decides which tenants are due. All its methods may be called
// from any goroutine: the relay's loop reads and writes it, request goroutines
// wake tenants through it.
type sweepSchedule struct {
	base    time.Duration // sweep cadence while a tenant has events
	idleMax time.Duration // longest wait for a tenant with none

	mu    sync.Mutex
	state map[string]*tenantSweep
	woken map[string]struct{}
}

func newSweepSchedule(base, idleMax time.Duration) *sweepSchedule {
	return &sweepSchedule{
		base:    base,
		idleMax: idleMax,
		state:   map[string]*tenantSweep{},
		woken:   map[string]struct{}{},
	}
}

// due reports whether tenantID should be swept now. A tenant never swept is due.
func (s *sweepSchedule) due(tenantID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[tenantID]
	return !ok || !now.Before(st.next)
}

// done records the outcome of a sweep. busy means the tenant had events, or the
// sweep failed and must be retried soon: both keep the base cadence. An empty
// sweep doubles the wait up to idleMax. With idleMax <= base the backoff is
// off and every tenant is swept every base, as before.
func (s *sweepSchedule) done(tenantID string, busy bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[tenantID]
	if !ok {
		st = &tenantSweep{interval: s.base}
		s.state[tenantID] = st
	}
	if busy || s.idleMax <= s.base {
		st.interval = s.base
	} else {
		st.interval = min(st.interval*2, s.idleMax)
	}
	st.next = now.Add(st.interval)
}

// wake flags a tenant to be swept right away, whatever its cadence.
func (s *sweepSchedule) wake(tenantID string) {
	s.mu.Lock()
	s.woken[tenantID] = struct{}{}
	s.mu.Unlock()
}

// takeWoken returns the tenants flagged since the last call and clears them.
func (s *sweepSchedule) takeWoken() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.woken) == 0 {
		return nil
	}
	out := s.woken
	s.woken = map[string]struct{}{}
	return out
}

// prune forgets the tenants that are no longer swept, so the state does not
// grow with every tenant the process ever saw.
func (s *sweepSchedule) prune(active []string) {
	keep := make(map[string]struct{}, len(active))
	for _, t := range active {
		keep[t] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.state {
		if _, ok := keep[t]; !ok {
			delete(s.state, t)
		}
	}
}

// intervalOf is the current wait of a tenant, for tests and diagnostics.
func (s *sweepSchedule) intervalOf(tenantID string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.state[tenantID]; ok {
		return st.interval
	}
	return 0
}
