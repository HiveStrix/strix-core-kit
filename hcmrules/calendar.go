package hcmrules

import (
	"fmt"
	"sort"
	"time"
)

// calendarDateLayout is the date format used by rule_calendar.entry_date and
// by the from/to range ResolveCalendar takes: plain "YYYY-MM-DD", the same
// layout every other rule table uses for valid_from/valid_to.
const calendarDateLayout = "2006-01-02"

// CalendarEntry is one dated entry of a jurisdiction/region/company calendar
// (mirrors rule_calendar), carrying only what ResolveCalendar needs.
type CalendarEntry struct {
	Date    string // "YYYY-MM-DD"
	Name    string
	PayRule string // "mandatory_paid" | "non_mandatory_paid" | "double"
}

// Day is one calendar entry resolved to fall inside a requested range.
type Day struct {
	Date    string
	Name    string
	PayRule string
}

// ResolveCalendar returns the entries whose Date falls inside [from, to]
// (both inclusive), ordered by date.
//
// Dates are parsed rather than compared as strings so a malformed date — on
// the range or on an entry — is reported explicitly instead of being
// silently dropped from the result: a bad date almost always means upstream
// data is wrong, not that the day does not apply.
func ResolveCalendar(entries []CalendarEntry, from, to string) ([]Day, error) {
	start, err := time.Parse(calendarDateLayout, from)
	if err != nil {
		return nil, fmt.Errorf("hcmrules: invalid range start %q: %w", from, err)
	}
	end, err := time.Parse(calendarDateLayout, to)
	if err != nil {
		return nil, fmt.Errorf("hcmrules: invalid range end %q: %w", to, err)
	}
	if end.Before(start) {
		return nil, fmt.Errorf("hcmrules: range end %q is before range start %q", to, from)
	}

	var days []Day
	for _, e := range entries {
		d, err := time.Parse(calendarDateLayout, e.Date)
		if err != nil {
			return nil, fmt.Errorf("hcmrules: invalid calendar entry date %q: %w", e.Date, err)
		}
		if d.Before(start) || d.After(end) {
			continue
		}
		days = append(days, Day(e))
	}
	sort.SliceStable(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	return days, nil
}
