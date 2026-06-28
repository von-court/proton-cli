// Package ical contains small helpers for building and parsing the iCal/VCard
// text used by Proton Calendar and Contacts. It is deliberately minimal - just
// enough for the fields the CLI reads and writes.
package ical

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sequence returns the VEVENT SEQUENCE from decrypted iCalendar text (0 if absent). Proton
// rejects a single-occurrence override whose SEQUENCE is below the master's (code 2001), and
// expects an update's SEQUENCE to not regress, so writes must read and respect it.
func Sequence(ics string) int {
	for _, line := range strings.Split(unfold(ics), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "SEQUENCE:") {
			if n, err := strconv.Atoi(strings.TrimSpace(line[len("SEQUENCE:"):])); err == nil {
				return n
			}
		}
	}
	return 0
}

// Field extracts a field value from iCal/vCard text. Handles both
// `FIELD:value` and `FIELD;PARAM=x:value` forms, plus `itemN.FIELD:…`.
func Field(text, name string) string {
	prefix := name + ":"
	prefixParam := name + ";"
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
		if strings.HasPrefix(line, prefixParam) {
			if i := strings.Index(line, ":"); i >= 0 {
				return line[i+1:]
			}
		}
		if strings.Contains(line, "."+name+";") || strings.Contains(line, "."+name+":") {
			if i := strings.Index(line, ":"); i >= 0 {
				return line[i+1:]
			}
		}
	}
	return ""
}

func EventUID() string {
	return fmt.Sprintf("%d@proton-cli", time.Now().UnixNano())
}

func ContactUID() string {
	return fmt.Sprintf("proton-cli-%d", time.Now().UnixNano())
}

// SignedVEVENT builds the signed portion of a Proton calendar event
// (Card Type 2: UID + DTSTAMP + DTSTART + DTEND + SEQUENCE).
func SignedVEVENT(uid string, start, end time.Time, allDay bool, sequence int) string {
	return SignedVEVENTEx(uid, start, end, allDay, sequence, nil)
}

// SignedVEVENTEx is SignedVEVENT with optional extra component lines (e.g. RRULE,
// RECURRENCE-ID, EXDATE) inserted after DTEND. Recurring writes need these — plain
// SignedVEVENT carries none, which would silently strip recurrence from a series.
func SignedVEVENTEx(uid string, start, end time.Time, allDay bool, sequence int, extra []string) string {
	dtstamp := time.Now().UTC().Format("20060102T150405Z")
	var dtstart, dtend string
	if allDay {
		dtstart = "DTSTART;VALUE=DATE:" + start.Format("20060102")
		dtend = "DTEND;VALUE=DATE:" + end.Format("20060102")
	} else {
		dtstart = "DTSTART:" + start.UTC().Format("20060102T150405Z")
		dtend = "DTEND:" + end.UTC().Format("20060102T150405Z")
	}
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + dtstamp,
		dtstart, dtend,
	}
	for _, e := range extra {
		if strings.TrimSpace(e) != "" {
			lines = append(lines, e)
		}
	}
	lines = append(lines, fmt.Sprintf("SEQUENCE:%d", sequence), "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// dateLine builds a DTSTART/DTEND/RECURRENCE-ID line. The value type/zone MUST match the series'
// DTSTART or a calendar server won't recognise an override: all-day → VALUE=DATE; a zoned series →
// TZID=<zone> with local wall-clock; otherwise a UTC instant.
func dateLine(name string, t time.Time, tzid string, isDate bool) string {
	if isDate {
		return name + ";VALUE=DATE:" + t.Format("20060102")
	}
	if tzid != "" {
		if loc, err := time.LoadLocation(tzid); err == nil {
			return name + ";TZID=" + tzid + ":" + t.In(loc).Format("20060102T150405")
		}
	}
	return name + ":" + t.UTC().Format("20060102T150405Z")
}

// RecurrenceIDLine builds a RECURRENCE-ID line for an occurrence's original start, in the series'
// own value type/zone so the override attaches to the right instance.
func RecurrenceIDLine(t time.Time, tzid string, isDate bool) string {
	return dateLine("RECURRENCE-ID", t, tzid, isDate)
}

// DTStartInfo reports the TZID and DATE-ness of the first DTSTART in decrypted iCalendar text, so
// overrides/shifts/splits can re-express times in the series' own zone.
func DTStartInfo(ics string) (tzid string, isDate bool) {
	for _, line := range strings.Split(unfold(ics), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "DTSTART") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			return
		}
		for _, p := range strings.Split(line[:colon], ";")[1:] {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) != 2 {
				continue
			}
			if strings.EqualFold(kv[0], "TZID") {
				tzid = kv[1]
			} else if strings.EqualFold(kv[0], "VALUE") && strings.EqualFold(kv[1], "DATE") {
				isDate = true
			}
		}
		return
	}
	return
}

// SignedVEVENTZoned is SignedVEVENTEx but writes DTSTART/DTEND in the given zone (or VALUE=DATE for
// all-day), so recurring writes preserve the series' TZID instead of collapsing to UTC.
func SignedVEVENTZoned(uid string, start, end time.Time, tzid string, isDate bool, sequence int, extra []string) string {
	dtstamp := time.Now().UTC().Format("20060102T150405Z")
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + dtstamp,
		dateLine("DTSTART", start, tzid, isDate),
		dateLine("DTEND", end, tzid, isDate),
	}
	for _, e := range extra {
		if strings.TrimSpace(e) != "" {
			lines = append(lines, e)
		}
	}
	lines = append(lines, fmt.Sprintf("SEQUENCE:%d", sequence), "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// RecurrenceLines pulls the RRULE / RECURRENCE-ID / EXDATE lines (verbatim) out of decrypted
// iCalendar text so an in-place update can re-emit them unchanged — this is what keeps a series
// recurring (and an override an override) across an EventUpdate.
func RecurrenceLines(ics string) []string {
	var out []string
	for _, line := range strings.Split(unfold(ics), "\n") {
		line = strings.TrimSpace(line)
		u := strings.ToUpper(line)
		if strings.HasPrefix(u, "RRULE:") || strings.HasPrefix(u, "RECURRENCE-ID") || strings.HasPrefix(u, "EXDATE") {
			out = append(out, line)
		}
	}
	return out
}

// EXDATELines returns only the EXDATE lines from decrypted iCalendar text.
func EXDATELines(ics string) []string {
	var out []string
	for _, l := range RecurrenceLines(ics) {
		if strings.HasPrefix(strings.ToUpper(l), "EXDATE") {
			out = append(out, l)
		}
	}
	return out
}

// TruncateRRULE drops any existing UNTIL/COUNT and appends UNTIL=<until, UTC>, ending a series
// just before a split point. The rrule argument is the bare value, e.g. "FREQ=DAILY;BYDAY=MO".
func TruncateRRULE(rrule string, until time.Time) string {
	parts := stripRRuleKeys(rrule, "UNTIL", "COUNT")
	parts = append(parts, "UNTIL="+until.UTC().Format("20060102T150405Z"))
	return strings.Join(parts, ";")
}

// StripUntilCount removes UNTIL/COUNT, yielding an open-ended rule for a split remainder.
func StripUntilCount(rrule string) string {
	return strings.Join(stripRRuleKeys(rrule, "UNTIL", "COUNT"), ";")
}

func stripRRuleKeys(rrule string, drop ...string) []string {
	dropSet := map[string]bool{}
	for _, d := range drop {
		dropSet[strings.ToUpper(d)] = true
	}
	var parts []string
	for _, p := range strings.Split(rrule, ";") {
		if p == "" {
			continue
		}
		key := strings.ToUpper(strings.SplitN(p, "=", 2)[0])
		if dropSet[key] {
			continue
		}
		parts = append(parts, p)
	}
	return parts
}

// unfold collapses RFC 5545 line folding (CRLF/LF followed by space or tab).
func unfold(text string) string {
	r := strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "")
	return r.Replace(text)
}

// EncryptedVEVENT builds the encrypted portion of a Proton calendar event
// (Card Type 3: SUMMARY + optional LOCATION).
func EncryptedVEVENT(title, location string) string {
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"BEGIN:VEVENT",
		"SUMMARY:" + title,
	}
	if location != "" {
		lines = append(lines, "LOCATION:"+location)
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// SignedVCard builds the signed portion of a Proton contact
// (Type 2: FN + UID + optional EMAIL).
func SignedVCard(name, email, uid string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\n")
	b.WriteString("FN:" + name + "\r\n")
	b.WriteString("UID:" + uid + "\r\n")
	if email != "" {
		b.WriteString("item1.EMAIL;PREF=1:" + email + "\r\n")
	}
	b.WriteString("END:VCARD")
	return b.String()
}

// EncryptedVCard builds the encrypted portion of a Proton contact
// (Type 3: TEL + NOTE + ORG).
func EncryptedVCard(phone, note, org string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\n")
	if phone != "" {
		b.WriteString("TEL;PREF=1:" + phone + "\r\n")
	}
	if note != "" {
		b.WriteString("NOTE:" + note + "\r\n")
	}
	if org != "" {
		b.WriteString("ORG:" + org + "\r\n")
	}
	b.WriteString("END:VCARD")
	return b.String()
}

// ParseTime accepts a handful of common user-entered date/time formats,
// interpreting bare dates/times in the local timezone.
func ParseTime(s string) (time.Time, error) {
	for _, f := range []string{
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %s", s)
}
