// Package ical contains small helpers for building and parsing the iCal/VCard
// text used by Proton Calendar and Contacts. It is deliberately minimal - just
// enough for the fields the CLI reads and writes.
package ical

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

// Fields is the multi-value form of Field: it returns every value for a vCard/
// iCal property (e.g. all EMAIL or TEL lines), in document order.
func Fields(text, name string) []string {
	var out []string
	prefix := name + ":"
	prefixParam := name + ";"
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, prefix):
			out = append(out, strings.TrimPrefix(line, prefix))
		case strings.HasPrefix(line, prefixParam):
			if i := strings.Index(line, ":"); i >= 0 {
				out = append(out, line[i+1:])
			}
		case strings.Contains(line, "."+name+";") || strings.Contains(line, "."+name+":"):
			if i := strings.Index(line, ":"); i >= 0 {
				out = append(out, line[i+1:])
			}
		}
	}
	return out
}

// vcardLine is a parsed vCard content line: [group.]FIELD[;params]:value.
type vcardLine struct {
	group  string
	field  string // upper-cased
	params string
	value  string
}

func parseVcardLine(raw string) (vcardLine, bool) {
	line := strings.TrimSpace(raw)
	colon := strings.Index(line, ":")
	if colon < 0 {
		return vcardLine{}, false
	}
	name, value := line[:colon], line[colon+1:]
	field, params := name, ""
	if semi := strings.Index(name, ";"); semi >= 0 {
		field, params = name[:semi], name[semi+1:]
	}
	group := ""
	if dot := strings.Index(field, "."); dot >= 0 {
		group, field = field[:dot], field[dot+1:]
	}
	return vcardLine{group: group, field: strings.ToUpper(field), params: params, value: value}, true
}

func canonicalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// EmailGroup returns the vCard group (e.g. "item1") whose EMAIL property value
// canonically matches email, or "" if none is found. Proton stores per-email
// key properties (KEY, X-PM-*) under the same group as the matching email.
func EmailGroup(text, email string) string {
	want := canonicalizeEmail(email)
	for _, raw := range strings.Split(text, "\n") {
		l, ok := parseVcardLine(raw)
		if !ok || l.field != "EMAIL" || l.group == "" {
			continue
		}
		if canonicalizeEmail(l.value) == want {
			return l.group
		}
	}
	return ""
}

// GroupValues returns every value of field within group, ordered by the vCard
// PREF parameter (ascending; properties without PREF keep document order,
// after the prefixed ones).
func GroupValues(text, group, field string) []string {
	field = strings.ToUpper(field)
	type pv struct {
		pref int
		val  string
	}
	var found []pv
	for i, raw := range strings.Split(text, "\n") {
		l, ok := parseVcardLine(raw)
		if !ok || l.group != group || l.field != field {
			continue
		}
		found = append(found, pv{pref: prefParam(l.params, i), val: l.value})
	}
	sort.SliceStable(found, func(a, b int) bool { return found[a].pref < found[b].pref })
	out := make([]string, len(found))
	for i, p := range found {
		out[i] = p.val
	}
	return out
}

// GroupValue returns the first (highest-preference) value of field within
// group, or "".
func GroupValue(text, group, field string) string {
	if vs := GroupValues(text, group, field); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// prefParam extracts the numeric PREF= parameter, or a large fallback that
// preserves document order for properties without one.
func prefParam(params string, docIndex int) int {
	for _, p := range strings.Split(params, ";") {
		if strings.HasPrefix(strings.ToUpper(p), "PREF=") {
			if n, err := strconv.Atoi(strings.TrimSpace(p[len("PREF="):])); err == nil {
				return n
			}
		}
	}
	return 1_000_000 + docIndex
}

func EventUID() string {
	return fmt.Sprintf("%d@proton-cli", time.Now().UnixNano())
}

func ContactUID() string {
	return fmt.Sprintf("proton-cli-%d", time.Now().UnixNano())
}

// Attendee is one calendar event participant. Token is the Proton attendee
// token (hex SHA-1 of UID+canonicalEmail) used to address the encrypted part.
type Attendee struct {
	Email string
	Token string
}

func eventDates(start, end time.Time, allDay bool) (dtstart, dtend string) {
	if allDay {
		return "DTSTART;VALUE=DATE:" + start.Format("20060102"), "DTEND;VALUE=DATE:" + end.Format("20060102")
	}
	return "DTSTART:" + start.UTC().Format("20060102T150405Z"), "DTEND:" + end.UTC().Format("20060102T150405Z")
}

func attendeeLine(a Attendee) string {
	return fmt.Sprintf("ATTENDEE;CN=%s;ROLE=REQ-PARTICIPANT;RSVP=TRUE;PARTSTAT=NEEDS-ACTION;X-PM-TOKEN=%s:mailto:%s", a.Email, a.Token, a.Email)
}

// SignedVEVENT builds the signed portion of a Proton calendar event (Card
// Type 2: UID + DTSTAMP + DTSTART + DTEND + optional ORGANIZER + optional RRULE
// + SEQUENCE). ORGANIZER lives in the signed shared part for events with
// attendees.
func SignedVEVENT(uid string, start, end time.Time, allDay bool, sequence int, rrule, organizer string) string {
	dtstamp := time.Now().UTC().Format("20060102T150405Z")
	dtstart, dtend := eventDates(start, end, allDay)
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + dtstamp,
		dtstart, dtend,
	}
	if organizer != "" {
		lines = append(lines, "ORGANIZER;CN="+organizer+":mailto:"+organizer)
	}
	if rrule != "" {
		lines = append(lines, "RRULE:"+rrule)
	}
	lines = append(lines, fmt.Sprintf("SEQUENCE:%d", sequence), "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// AttendeesVEVENT builds the encrypted attendees part (a VEVENT carrying UID +
// one ATTENDEE line per participant, each tagged with its X-PM-TOKEN).
func AttendeesVEVENT(uid string, attendees []Attendee) string {
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
	}
	for _, a := range attendees {
		lines = append(lines, attendeeLine(a))
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// InviteICS builds a self-contained METHOD:REQUEST VCALENDAR suitable for
// emailing as a text/calendar invitation to external attendees.
func InviteICS(uid, summary, location, description string, start, end time.Time, allDay bool, organizer string, attendees []Attendee) string {
	dtstamp := time.Now().UTC().Format("20060102T150405Z")
	dtstart, dtend := eventDates(start, end, allDay)
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"METHOD:REQUEST", "CALSCALE:GREGORIAN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + dtstamp,
		dtstart, dtend,
		"SUMMARY:" + summary,
	}
	if location != "" {
		lines = append(lines, "LOCATION:"+location)
	}
	if description != "" {
		lines = append(lines, "DESCRIPTION:"+description)
	}
	if organizer != "" {
		lines = append(lines, "ORGANIZER;CN="+organizer+":mailto:"+organizer)
	}
	for _, a := range attendees {
		lines = append(lines, attendeeLine(a))
	}
	lines = append(lines, "SEQUENCE:0", "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// ReplyICS builds a self-contained METHOD:REPLY VCALENDAR answering an
// invitation. It carries the RFC-required fields plus a single ATTENDEE line
// tagged with the responder's PARTSTAT (ACCEPTED/TENTATIVE/DECLINED), suitable
// for emailing to the organizer as a text/calendar reply. protonReply adds the
// X-PM-PROTON-REPLY marker Proton sets on Proton-to-Proton replies.
func ReplyICS(uid, summary, location, organizer, attendeeEmail, partstat string, start, end time.Time, allDay, protonReply bool) string {
	dtstamp := time.Now().UTC().Format("20060102T150405Z")
	dtstart, dtend := eventDates(start, end, allDay)
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"METHOD:REPLY", "CALSCALE:GREGORIAN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + dtstamp,
		dtstart, dtend,
	}
	if summary != "" {
		lines = append(lines, "SUMMARY:"+summary)
	}
	if location != "" {
		lines = append(lines, "LOCATION:"+location)
	}
	if organizer != "" {
		lines = append(lines, "ORGANIZER;CN="+organizer+":mailto:"+organizer)
	}
	if protonReply {
		lines = append(lines, "X-PM-PROTON-REPLY;TYPE=boolean:true")
	}
	lines = append(lines, fmt.Sprintf("ATTENDEE;PARTSTAT=%s:mailto:%s", partstat, attendeeEmail))
	lines = append(lines, "SEQUENCE:0", "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// EncryptedVEVENT builds the encrypted portion of a Proton calendar event
// (Card Type 3: SUMMARY + optional LOCATION + optional DESCRIPTION).
func EncryptedVEVENT(title, location, description string) string {
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//proton-cli//EN",
		"BEGIN:VEVENT",
		"SUMMARY:" + title,
	}
	if location != "" {
		lines = append(lines, "LOCATION:"+location)
	}
	if description != "" {
		lines = append(lines, "DESCRIPTION:"+description)
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return strings.Join(lines, "\r\n")
}

// SignedVCard builds the signed portion of a Proton contact
// (Type 2: FN + UID + one EMAIL per address). Emails go in the signed card so
// they remain searchable.
func SignedVCard(name string, emails []string, uid string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\n")
	b.WriteString("FN:" + name + "\r\n")
	b.WriteString("UID:" + uid + "\r\n")
	n := 0
	for _, e := range emails {
		if e == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "item%d.EMAIL;PREF=%d:%s\r\n", n, n, e)
	}
	b.WriteString("END:VCARD")
	return b.String()
}

// SignedEmail is one email entry in a Proton signed contact card, with any
// pinned keys and per-email crypto flags stored under its group.
type SignedEmail struct {
	Address   string
	KeyValues []string // raw vCard KEY property values, preference order
	Encrypt   *bool
	Sign      *bool
	Scheme    string
}

// SignedContact is the subset of a Proton signed contact card proton-cli
// models: name, UID, and per-email entries (including pinned keys).
type SignedContact struct {
	Name   string
	UID    string
	Emails []SignedEmail
}

// FindEmail returns the entry whose address canonically matches addr, or nil.
func (c *SignedContact) FindEmail(addr string) *SignedEmail {
	want := canonicalizeEmail(addr)
	for i := range c.Emails {
		if canonicalizeEmail(c.Emails[i].Address) == want {
			return &c.Emails[i]
		}
	}
	return nil
}

// ParseSignedVCard reads signed-card text into a SignedContact, capturing
// per-email pinned keys and crypto flags by group.
func ParseSignedVCard(text string) SignedContact {
	out := SignedContact{Name: Field(text, "FN"), UID: Field(text, "UID")}
	seen := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		l, ok := parseVcardLine(raw)
		if !ok || l.field != "EMAIL" || l.group == "" || seen[l.group] {
			continue
		}
		seen[l.group] = true
		e := SignedEmail{
			Address:   l.value,
			KeyValues: GroupValues(text, l.group, "KEY"),
			Scheme:    GroupValue(text, l.group, "X-PM-SCHEME"),
		}
		if v := GroupValue(text, l.group, "X-PM-ENCRYPT"); v != "" {
			b := strings.EqualFold(strings.TrimSpace(v), "true")
			e.Encrypt = &b
		}
		if v := GroupValue(text, l.group, "X-PM-SIGN"); v != "" {
			b := strings.EqualFold(strings.TrimSpace(v), "true")
			e.Sign = &b
		}
		out.Emails = append(out.Emails, e)
	}
	return out
}

// BuildSignedVCard serializes a SignedContact back to signed-card text,
// emitting per-email EMAIL/KEY/X-PM-* properties grouped as item1..itemN.
func BuildSignedVCard(c SignedContact) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\n")
	b.WriteString("FN:" + c.Name + "\r\n")
	b.WriteString("UID:" + c.UID + "\r\n")
	n := 0
	for _, e := range c.Emails {
		if e.Address == "" {
			continue
		}
		n++
		group := fmt.Sprintf("item%d", n)
		fmt.Fprintf(&b, "%s.EMAIL;PREF=%d:%s\r\n", group, n, e.Address)
		for i, kv := range e.KeyValues {
			fmt.Fprintf(&b, "%s.KEY;PREF=%d:%s\r\n", group, i+1, kv)
		}
		if e.Encrypt != nil {
			fmt.Fprintf(&b, "%s.X-PM-ENCRYPT:%s\r\n", group, boolStr(*e.Encrypt))
		}
		if e.Sign != nil {
			fmt.Fprintf(&b, "%s.X-PM-SIGN:%s\r\n", group, boolStr(*e.Sign))
		}
		if e.Scheme != "" {
			fmt.Fprintf(&b, "%s.X-PM-SCHEME:%s\r\n", group, e.Scheme)
		}
	}
	b.WriteString("END:VCARD")
	return b.String()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// VCardFields holds the encrypted-card contact properties.
type VCardFields struct {
	Phones   []string
	Note     string
	Org      string
	Title    string
	Birthday string
	Address  string
	URL      string
}

// EncryptedVCard builds the encrypted portion of a Proton contact (Type 3:
// TEL + NOTE + ORG + TITLE + BDAY + ADR + URL). Empty fields are omitted.
func EncryptedVCard(f VCardFields) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\n")
	n := 0
	for _, p := range f.Phones {
		if p == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "TEL;PREF=%d:%s\r\n", n, p)
	}
	if f.Note != "" {
		b.WriteString("NOTE:" + f.Note + "\r\n")
	}
	if f.Org != "" {
		b.WriteString("ORG:" + f.Org + "\r\n")
	}
	if f.Title != "" {
		b.WriteString("TITLE:" + f.Title + "\r\n")
	}
	if f.Birthday != "" {
		b.WriteString("BDAY:" + f.Birthday + "\r\n")
	}
	if f.Address != "" {
		b.WriteString("ADR:" + f.Address + "\r\n")
	}
	if f.URL != "" {
		b.WriteString("URL:" + f.URL + "\r\n")
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

// --- recurring-event support (von-court) ------------------------------------------

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
