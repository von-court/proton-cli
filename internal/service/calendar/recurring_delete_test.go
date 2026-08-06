package calendar

// Deleting one occurrence of a series must leave the rest of the series intact.
//
// `events delete` used to take only a calendar+event id, so "delete this occurrence" had no way to
// be expressed and every caller that meant it destroyed the whole series instead. These tests pin
// the iCalendar surgery that makes the scoped delete non-destructive: the RRULE has to survive a
// single-occurrence delete, and the EXDATE has to be written in the series' own zone or the
// "deleted" occurrence comes back.
//
// The service methods themselves need unlocked calendar keys and a Proton session, so the surgery
// is factored into pure helpers (excludeOccurrenceLines / truncateBeforeLines) and tested here with
// no credentials and no network.

import (
	"strings"
	"testing"
	"time"
)

const zonedMaster = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:series-uid\r\n" +
	"DTSTART;TZID=Europe/Vienna:20260803T090000\r\nDTEND;TZID=Europe/Vienna:20260803T100000\r\n" +
	"RRULE:FREQ=WEEKLY;BYDAY=MO\r\nSEQUENCE:3\r\nEND:VEVENT\r\nEND:VCALENDAR"

// 2026-08-10 09:00 Europe/Vienna (CEST, UTC+2)
func occ(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Vienna")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return time.Date(2026, 8, 10, 9, 0, 0, 0, loc)
}

func joined(lines []string) string { return strings.ToUpper(strings.Join(lines, "\n")) }

// The whole point: excluding one occurrence must not remove the rule that generates the others.
func TestExcludeOccurrenceKeepsTheRRULE(t *testing.T) {
	extra, changed := excludeOccurrenceLines(zonedMaster, occ(t), "Europe/Vienna", false)
	if !changed {
		t.Fatal("expected the exclusion to be a change")
	}
	if !strings.Contains(joined(extra), "RRULE:FREQ=WEEKLY;BYDAY=MO") {
		t.Fatalf("the series rule was dropped — the rest of the series would be gone: %q", extra)
	}
	if !strings.Contains(joined(extra), "EXDATE") {
		t.Fatalf("no EXDATE written, so nothing was actually deleted: %q", extra)
	}
}

// A bare UTC EXDATE does not match an occurrence of a TZID-anchored series: Proton keeps
// generating it and the "deleted" occurrence silently reappears.
func TestExcludeOccurrenceWritesTheEXDATEInTheSeriesZone(t *testing.T) {
	extra, _ := excludeOccurrenceLines(zonedMaster, occ(t), "Europe/Vienna", false)
	j := joined(extra)
	if !strings.Contains(j, "EXDATE;TZID=EUROPE/VIENNA:20260810T090000") {
		t.Fatalf("EXDATE is not in the series zone/wall-clock: %q", extra)
	}
	if strings.Contains(j, "EXDATE:20260810T070000Z") {
		t.Fatalf("EXDATE collapsed to UTC; the occurrence would come back: %q", extra)
	}
}

// Retrying a delete (a failed write, a double click) must not pile up EXDATEs or bump SEQUENCE
// for nothing.
func TestExcludeOccurrenceIsIdempotent(t *testing.T) {
	already := strings.Replace(zonedMaster, "RRULE:FREQ=WEEKLY;BYDAY=MO\r\n",
		"RRULE:FREQ=WEEKLY;BYDAY=MO\r\nEXDATE;TZID=Europe/Vienna:20260810T090000\r\n", 1)
	if _, changed := excludeOccurrenceLines(already, occ(t), "Europe/Vienna", false); changed {
		t.Fatal("an already-excluded occurrence was excluded again (duplicate EXDATE)")
	}
}

// Existing exclusions are part of the series' state; re-emitting the master must carry them over
// or previously deleted occurrences resurrect.
func TestExcludeOccurrencePreservesExistingEXDATEs(t *testing.T) {
	prior := strings.Replace(zonedMaster, "RRULE:FREQ=WEEKLY;BYDAY=MO\r\n",
		"RRULE:FREQ=WEEKLY;BYDAY=MO\r\nEXDATE;TZID=Europe/Vienna:20260817T090000\r\n", 1)
	extra, changed := excludeOccurrenceLines(prior, occ(t), "Europe/Vienna", false)
	if !changed {
		t.Fatal("expected a change")
	}
	j := joined(extra)
	if !strings.Contains(j, "20260817T090000") || !strings.Contains(j, "20260810T090000") {
		t.Fatalf("lost a previously deleted occurrence: %q", extra)
	}
}

// All-day series exclude by DATE, not by an instant.
func TestExcludeOccurrenceAllDayUsesDateValue(t *testing.T) {
	allDay := "BEGIN:VEVENT\r\nDTSTART;VALUE=DATE:20260803\r\nRRULE:FREQ=DAILY\r\nEND:VEVENT"
	extra, _ := excludeOccurrenceLines(allDay, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), "", true)
	if !strings.Contains(joined(extra), "EXDATE;VALUE=DATE:20260810") {
		t.Fatalf("all-day exclusion is not a DATE value: %q", extra)
	}
}

// "This and following": the earlier occurrences must survive, so the rule stays and only gains an
// UNTIL that stops it one second before the cut.
func TestTruncateBeforeKeepsEarlierOccurrences(t *testing.T) {
	lines := truncateBeforeLines(zonedMaster, "FREQ=WEEKLY;BYDAY=MO", occ(t))
	j := joined(lines)
	if !strings.Contains(j, "FREQ=WEEKLY;BYDAY=MO") {
		t.Fatalf("the rule was dropped instead of truncated: %q", lines)
	}
	// 2026-08-10 09:00 +02:00 == 07:00Z; UNTIL is one second earlier and always UTC (RFC 5545).
	if !strings.Contains(j, "UNTIL=20260810T065959Z") {
		t.Fatalf("UNTIL does not stop just before the deleted occurrence: %q", lines)
	}
}

// A truncating rewrite must not resurrect occurrences the user deleted earlier, and must not turn
// the master into an override.
func TestTruncateBeforeKeepsEXDATEsAndDropsRecurrenceID(t *testing.T) {
	ics := strings.Replace(zonedMaster, "RRULE:FREQ=WEEKLY;BYDAY=MO\r\n",
		"RRULE:FREQ=WEEKLY;BYDAY=MO\r\nEXDATE;TZID=Europe/Vienna:20260817T090000\r\n"+
			"RECURRENCE-ID;TZID=Europe/Vienna:20260803T090000\r\n", 1)
	j := joined(truncateBeforeLines(ics, "FREQ=WEEKLY;BYDAY=MO", occ(t)))
	if !strings.Contains(j, "EXDATE;TZID=EUROPE/VIENNA:20260817T090000") {
		t.Fatalf("a previously deleted occurrence was resurrected: %q", j)
	}
	if strings.Contains(j, "RECURRENCE-ID") {
		t.Fatalf("the master kept a RECURRENCE-ID and would be read as an override: %q", j)
	}
}

// An existing UNTIL/COUNT must be replaced, not appended to (two UNTILs is an invalid rule).
func TestTruncateBeforeReplacesAnExistingBound(t *testing.T) {
	j := joined(truncateBeforeLines(zonedMaster, "FREQ=WEEKLY;COUNT=52", occ(t)))
	if strings.Contains(j, "COUNT=") {
		t.Fatalf("COUNT survived alongside UNTIL: %q", j)
	}
	if strings.Count(j, "UNTIL=") != 1 {
		t.Fatalf("expected exactly one UNTIL: %q", j)
	}
}
