package hcmrules

import (
	"testing"
)

// costaRica2026Entries exercises the real CR mandatory/non-mandatory-pay
// distinction the theory names explicitly: 2 Aug and 12 Oct are
// non-mandatory-pay feriados, unlike Jan 1 or Dec 25.
func costaRica2026Entries() []CalendarEntry {
	return []CalendarEntry{
		{Date: "2026-01-01", Name: "Año Nuevo", PayRule: "mandatory_paid"},
		{Date: "2026-04-11", Name: "Día de Juan Santamaría", PayRule: "mandatory_paid"},
		{Date: "2026-08-02", Name: "Virgen de los Ángeles", PayRule: "non_mandatory_paid"},
		{Date: "2026-10-12", Name: "Día de las Culturas", PayRule: "non_mandatory_paid"},
		{Date: "2026-12-25", Name: "Navidad", PayRule: "mandatory_paid"},
	}
}

func TestResolveCalendar_FullYear(t *testing.T) {
	got, err := ResolveCalendar(costaRica2026Entries(), "2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatalf("ResolveCalendar: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d days, want 5", len(got))
	}
	// Ordered ascending by date.
	for i := 1; i < len(got); i++ {
		if got[i-1].Date >= got[i].Date {
			t.Errorf("days not ordered: %s before %s", got[i-1].Date, got[i].Date)
		}
	}
}

func TestResolveCalendar_NarrowRange_NonMandatoryPaidPair(t *testing.T) {
	got, err := ResolveCalendar(costaRica2026Entries(), "2026-08-01", "2026-10-31")
	if err != nil {
		t.Fatalf("ResolveCalendar: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d days, want 2", len(got))
	}
	for _, day := range got {
		if day.PayRule != "non_mandatory_paid" {
			t.Errorf("day %s: got pay_rule %q, want non_mandatory_paid", day.Date, day.PayRule)
		}
	}
	if got[0].Date != "2026-08-02" || got[1].Date != "2026-10-12" {
		t.Errorf("got dates %s, %s in the wrong order", got[0].Date, got[1].Date)
	}
}

func TestResolveCalendar_NoEntriesInRange(t *testing.T) {
	got, err := ResolveCalendar(costaRica2026Entries(), "2027-01-01", "2027-12-31")
	if err != nil {
		t.Fatalf("ResolveCalendar: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d days, want 0", len(got))
	}
}

func TestResolveCalendar_Errors(t *testing.T) {
	if _, err := ResolveCalendar(costaRica2026Entries(), "2026-13-01", "2026-12-31"); err == nil {
		t.Error("expected an error for a malformed range start")
	}
	if _, err := ResolveCalendar(costaRica2026Entries(), "2026-01-01", "not-a-date"); err == nil {
		t.Error("expected an error for a malformed range end")
	}
	if _, err := ResolveCalendar(costaRica2026Entries(), "2026-12-31", "2026-01-01"); err == nil {
		t.Error("expected an error when range end is before range start")
	}
	badEntries := []CalendarEntry{{Date: "not-a-date", Name: "Bad Entry", PayRule: "mandatory_paid"}}
	if _, err := ResolveCalendar(badEntries, "2026-01-01", "2026-12-31"); err == nil {
		t.Error("expected an error for a malformed entry date")
	}
}
