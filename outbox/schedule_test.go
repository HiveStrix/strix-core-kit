package outbox

import (
	"sort"
	"testing"
	"time"
)

func TestScheduleSweepsANewTenantAtOnce(t *testing.T) {
	s := newSweepSchedule(2*time.Second, 30*time.Second)
	if !s.due("a", time.Now()) {
		t.Fatal("a tenant never swept must be due")
	}
}

// Every empty sweep doubles the wait, and it stops at the cap.
func TestScheduleBacksOffWhileEmptyAndCaps(t *testing.T) {
	s := newSweepSchedule(2*time.Second, 30*time.Second)
	now := time.Unix(1000, 0)
	want := []time.Duration{4, 8, 16, 30, 30}
	for i, w := range want {
		s.done("a", false, now)
		if got := s.intervalOf("a"); got != w*time.Second {
			t.Fatalf("empty sweep %d: wait = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if s.due("a", now.Add(29*time.Second)) {
		t.Error("due before its wait is over")
	}
	if !s.due("a", now.Add(30*time.Second)) {
		t.Error("not due when its wait is over")
	}
}

// Work, or a failure that has to be retried, brings the tenant back to the base
// cadence at once. Without it a broker that comes back would find the events
// waiting up to the idle cap.
func TestScheduleBusyOrFailedResetsToBase(t *testing.T) {
	s := newSweepSchedule(2*time.Second, 30*time.Second)
	now := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		s.done("a", false, now)
	}
	if s.intervalOf("a") != 30*time.Second {
		t.Fatalf("setup: wait = %v", s.intervalOf("a"))
	}
	s.done("a", true, now)
	if got := s.intervalOf("a"); got != 2*time.Second {
		t.Fatalf("after a busy sweep wait = %v, want 2s", got)
	}
}

func TestScheduleBackoffOffKeepsTheBaseCadence(t *testing.T) {
	for _, idleMax := range []time.Duration{2 * time.Second, time.Second, -1, 0} {
		s := newSweepSchedule(2*time.Second, idleMax)
		for i := 0; i < 6; i++ {
			s.done("a", false, time.Unix(1000, 0))
		}
		if got := s.intervalOf("a"); got != 2*time.Second {
			t.Errorf("idleMax %v: wait = %v, want the base 2s", idleMax, got)
		}
	}
}

// A woken tenant is swept whatever its cadence, once, and only the ones woken.
func TestScheduleWakeIsTakenOnce(t *testing.T) {
	s := newSweepSchedule(2*time.Second, 30*time.Second)
	s.wake("a")
	s.wake("a")
	s.wake("b")
	got := s.takeWoken()
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("woken = %v, want [a b] once each", names)
	}
	if again := s.takeWoken(); len(again) != 0 {
		t.Errorf("woken twice: %v", again)
	}
}

func TestSchedulePruneForgetsTenantsNoLongerSwept(t *testing.T) {
	s := newSweepSchedule(2*time.Second, 30*time.Second)
	s.done("a", false, time.Now())
	s.done("b", false, time.Now())
	s.prune([]string{"a"})
	if s.intervalOf("b") != 0 {
		t.Error("b was not forgotten")
	}
	if s.intervalOf("a") == 0 {
		t.Error("a was forgotten")
	}
}

func TestIdleIntervalFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultIdleInterval},
		{"45s", 45 * time.Second},
		{"0", 0},
		{"no-es-duracion", defaultIdleInterval},
	}
	for _, c := range cases {
		t.Setenv("STRIX_RELAY_IDLE_INTERVAL", c.env)
		if got := idleIntervalFromEnv(); got != c.want {
			t.Errorf("STRIX_RELAY_IDLE_INTERVAL=%q: %v, want %v", c.env, got, c.want)
		}
	}
}
