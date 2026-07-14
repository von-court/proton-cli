// Package calendar provides Proton Calendar operations.
package calendar

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	pgp "github.com/ProtonMail/gopenpgp/v2/crypto"
	"github.com/roman-16/proton-cli/internal/crypto/ical"
	"github.com/roman-16/proton-cli/internal/crypto/keys"
	pgphelper "github.com/roman-16/proton-cli/internal/crypto/pgp"
	"github.com/roman-16/proton-cli/internal/errs"
	"github.com/roman-16/proton-cli/internal/proton"
)

type Service struct{ C proton.Doer }

func New(c proton.Doer) *Service { return &Service{C: c} }

type Calendar struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description,omitempty"`
	MemberCount int    `json:"member_count"`
}

type Event struct {
	ID         string                 `json:"id"`
	CalendarID string                 `json:"calendar_id"`
	Title      string                 `json:"title"`
	Location   string                 `json:"location,omitempty"`
	Start      time.Time              `json:"start"`
	End        time.Time              `json:"end"`
	AllDay     bool                   `json:"all_day"`
	UID        string                 `json:"uid,omitempty"`
	RRule      string                 `json:"rrule,omitempty"`
	ICS        string                 `json:"ical,omitempty"`
	Signature  pgphelper.VerifyResult `json:"signature,omitempty"`
}

type calKeys struct {
	calKR    *pgp.KeyRing
	addrKR   *pgp.KeyRing
	memberID string
}

// CalendarsList reads per-user prefs (Name/Color/Description) from Members[0].
func (s *Service) CalendarsList(ctx context.Context) ([]Calendar, error) {
	var r struct {
		Calendars []struct {
			ID      string
			Members []struct {
				Name        string
				Color       string
				Description string
				Email       string
			}
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1"}, &r); err != nil {
		return nil, err
	}
	out := make([]Calendar, 0, len(r.Calendars))
	for _, c := range r.Calendars {
		var name, color, desc string
		if len(c.Members) > 0 {
			name = c.Members[0].Name
			color = c.Members[0].Color
			desc = c.Members[0].Description
		}
		out = append(out, Calendar{ID: c.ID, Name: name, Color: color, Description: desc, MemberCount: len(c.Members)})
	}
	return out, nil
}

func (s *Service) CalendarCreate(ctx context.Context, u *keys.Unlocked, name, color string) (string, error) {
	_, addrID, _, err := u.PrimaryAddrKR()
	if err != nil {
		return "", err
	}
	var r struct{ Calendar struct{ ID string } }
	if err := s.C.Decode(ctx, proton.Request{
		Method: "POST", Path: "/calendar/v1",
		Body: map[string]any{"Name": name, "Color": color, "Display": 1, "AddressID": addrID},
	}, &r); err != nil {
		return "", err
	}
	return r.Calendar.ID, nil
}

// CalendarDelete requires the caller to have unlocked the password scope first.
func (s *Service) CalendarDelete(ctx context.Context, id string) error {
	return s.C.Decode(ctx, proton.Request{Method: "DELETE", Path: "/calendar/v1/" + id}, nil)
}

func (s *Service) ResolveCalendarID(ctx context.Context, nameOrID string) (string, error) {
	cals, err := s.CalendarsList(ctx)
	if err != nil {
		return "", err
	}
	if nameOrID == "" {
		if len(cals) == 0 {
			return "", &errs.NotFound{Kind: "calendar"}
		}
		return cals[0].ID, nil
	}
	for _, c := range cals {
		if c.ID == nameOrID {
			return c.ID, nil
		}
	}
	for _, c := range cals {
		if strings.EqualFold(c.Name, nameOrID) {
			return c.ID, nil
		}
	}
	return "", &errs.NotFound{Kind: "calendar", Ref: nameOrID}
}

const (
	// The API caps a page of events at 100 and reports no total, so a window holding more than
	// that must be walked page by page or the tail is silently lost.
	eventsPageSize = 100
	eventsMaxPages = 100
)

func (s *Service) EventsList(ctx context.Context, u *keys.Unlocked, calendarID string, start, end time.Time) ([]Event, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return nil, err
	}
	var out []Event
	for page := 0; page < eventsMaxPages; page++ {
		q := url.Values{}
		q.Set("Start", fmt.Sprintf("%d", start.Unix()))
		q.Set("End", fmt.Sprintf("%d", end.Unix()))
		q.Set("Timezone", "UTC")
		q.Set("Type", "0")
		q.Set("Page", fmt.Sprintf("%d", page))
		q.Set("PageSize", fmt.Sprintf("%d", eventsPageSize))

		var r struct {
			Events []rawEvent
		}
		if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/events", Query: q}, &r); err != nil {
			return nil, err
		}
		if len(r.Events) == 0 {
			break
		}
		for _, e := range r.Events {
			out = append(out, e.toEvent(ck))
		}
		if len(r.Events) < eventsPageSize {
			break
		}
	}
	return out, nil
}

type rawEvent struct {
	ID              string
	CalendarID      string
	StartTime       int64
	EndTime         int64
	FullDay         int
	UID             string
	SharedKeyPacket string
	SharedEvents    []map[string]any
}

func (e rawEvent) toEvent(ck *calKeys) Event {
	title, location, rrule, ics, sig := decryptTitleLocation(e.SharedEvents, e.SharedKeyPacket, ck)
	return Event{
		ID: e.ID, CalendarID: e.CalendarID, Title: title, Location: location,
		Start: time.Unix(e.StartTime, 0), End: time.Unix(e.EndTime, 0),
		AllDay: e.FullDay == 1, UID: e.UID, RRule: rrule, ICS: ics, Signature: sig,
	}
}

func (s *Service) EventGet(ctx context.Context, u *keys.Unlocked, calendarID, eventID string) (*Event, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return nil, err
	}
	var r struct{ Event rawEvent }
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/events/" + eventID}, &r); err != nil {
		return nil, err
	}
	ev := r.Event.toEvent(ck)
	return &ev, nil
}

// EventCreate creates an event. A non-empty rrule (bare value, e.g. "FREQ=DAILY;COUNT=5") makes
// it a recurring series.
func (s *Service) EventCreate(ctx context.Context, u *keys.Unlocked, calendarID, title, location string, start, end time.Time, allDay bool, rrule string) (string, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return "", err
	}
	var extra []string
	if rrule != "" {
		extra = []string{"RRULE:" + rrule}
	}
	signed := ical.SignedVEVENTEx(ical.EventUID(), start, end, allDay, 0, extra)
	encrypted := ical.EncryptedVEVENT(title, location)
	return s.putNewEvent(ctx, ck, calendarID, signed, encrypted)
}

// EventUpdate leaves empty fields unchanged.
func (s *Service) EventUpdate(ctx context.Context, u *keys.Unlocked, calendarID, eventID, title, location string, start, end time.Time) error {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return err
	}
	var r struct {
		Event struct {
			UID             string
			StartTime       int64
			EndTime         int64
			FullDay         int
			SharedEvents    []map[string]any
			SharedKeyPacket string
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/events/" + eventID}, &r); err != nil {
		return err
	}

	curTitle, curLoc, _, curICS, _ := decryptTitleLocation(r.Event.SharedEvents, r.Event.SharedKeyPacket, ck)
	if title == "" {
		title = curTitle
	}
	if location == "" {
		location = curLoc
	}
	if start.IsZero() {
		start = time.Unix(r.Event.StartTime, 0)
	}
	if end.IsZero() {
		end = time.Unix(r.Event.EndTime, 0)
	}

	// Re-emit any RRULE / RECURRENCE-ID / EXDATE so an in-place edit preserves recurrence (a plain
	// SignedVEVENT carries none, which would turn a series — or an override — into a one-off event).
	// Bump SEQUENCE from the current value so it never regresses (Proton rejects that, and an
	// override must stay >= its master).
	signed := ical.SignedVEVENTEx(r.Event.UID, start, end, r.Event.FullDay == 1, ical.Sequence(curICS)+1, ical.RecurrenceLines(curICS))
	encrypted := ical.EncryptedVEVENT(title, location)
	signedCard, encCard, _, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, r.Event.SharedKeyPacket)
	if err != nil {
		return err
	}
	body := map[string]any{
		"MemberID": ck.memberID,
		"Events": []map[string]any{{
			"ID": eventID,
			"Event": map[string]any{
				"Permissions":        63,
				"IsOrganizer":        1,
				"SharedEventContent": []any{signedCard, encCard},
				"Notifications":      nil,
				"Color":              nil,
			},
		}},
	}
	return s.C.Decode(ctx, proton.Request{Method: "PUT", Path: "/calendar/v1/" + calendarID + "/events/sync", Body: body}, nil)
}

func (s *Service) EventDelete(ctx context.Context, u *keys.Unlocked, calendarID, eventID string) error {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return err
	}
	return s.C.Decode(ctx, proton.Request{
		Method: "PUT", Path: "/calendar/v1/" + calendarID + "/events/sync",
		Body: map[string]any{"MemberID": ck.memberID, "Events": []map[string]any{{"ID": eventID}}},
	}, nil)
}

// masterInfo is the decrypted state of a (master) event needed to write a recurrence override,
// a series shift, or a split.
type masterInfo struct {
	uid       string
	allDay    bool
	start     time.Time
	end       time.Time
	keyPacket string
	title     string
	location  string
	rrule     string
	ics       string
	seq       int
}

func (s *Service) loadMaster(ctx context.Context, ck *calKeys, calendarID, eventID string) (*masterInfo, error) {
	var r struct {
		Event struct {
			UID             string
			SharedKeyPacket string
			StartTime       int64
			EndTime         int64
			FullDay         int
			SharedEvents    []map[string]any
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/events/" + eventID}, &r); err != nil {
		return nil, err
	}
	title, loc, rrule, ics, _ := decryptTitleLocation(r.Event.SharedEvents, r.Event.SharedKeyPacket, ck)
	return &masterInfo{
		uid: r.Event.UID, allDay: r.Event.FullDay == 1,
		start: time.Unix(r.Event.StartTime, 0), end: time.Unix(r.Event.EndTime, 0),
		keyPacket: r.Event.SharedKeyPacket, title: title, location: loc, rrule: rrule, ics: ics,
		seq: ical.Sequence(ics),
	}, nil
}

// putNewEvent PUTs a brand-new calendar event (fresh session key). Used for overrides and the
// remainder series of a split.
func (s *Service) putNewEvent(ctx context.Context, ck *calKeys, calendarID, signed, encrypted string) (string, error) {
	signedCard, encCard, keyPacket, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, "")
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"MemberID": ck.memberID,
		"Events": []map[string]any{{
			"Overwrite": 0,
			"Event": map[string]any{
				"Permissions":        63,
				"IsOrganizer":        1,
				"SharedKeyPacket":    keyPacket,
				"SharedEventContent": []any{signedCard, encCard},
				"Notifications":      nil,
				"Color":              nil,
			},
		}},
	}
	var r struct {
		Responses []struct {
			Response struct {
				Code  int
				Error string
				Event struct{ ID string }
			}
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "PUT", Path: "/calendar/v1/" + calendarID + "/events/sync", Body: body}, &r); err != nil {
		return "", err
	}
	// The sync endpoint returns HTTP 200 even when an individual event is rejected; the real
	// status is the per-event Code (1000 = success). Surface a rejection instead of silently
	// returning an empty id.
	if len(r.Responses) > 0 {
		resp := r.Responses[0].Response
		if resp.Event.ID != "" {
			return resp.Event.ID, nil
		}
		if resp.Code != 0 && resp.Code != 1000 {
			return "", fmt.Errorf("proton rejected event (code %d): %s", resp.Code, resp.Error)
		}
	}
	return "", nil
}

// putUpdateEvent PUTs an in-place update to an existing event, reusing its session key packet.
func (s *Service) putUpdateEvent(ctx context.Context, ck *calKeys, calendarID, eventID, signed, encrypted, keyPacket string) error {
	signedCard, encCard, _, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, keyPacket)
	if err != nil {
		return err
	}
	body := map[string]any{
		"MemberID": ck.memberID,
		"Events": []map[string]any{{
			"ID": eventID,
			"Event": map[string]any{
				"Permissions":        63,
				"IsOrganizer":        1,
				"SharedEventContent": []any{signedCard, encCard},
				"Notifications":      nil,
				"Color":              nil,
			},
		}},
	}
	return s.C.Decode(ctx, proton.Request{Method: "PUT", Path: "/calendar/v1/" + calendarID + "/events/sync", Body: body}, nil)
}

// EventCreateOverride writes a single-occurrence override: a new event sharing the master UID
// plus a RECURRENCE-ID for the occurrence's original start. The master series is left untouched.
// title/location default to the master's when empty.
func (s *Service) EventCreateOverride(ctx context.Context, u *keys.Unlocked, calendarID, masterEventID string, recurrenceID, start, end time.Time, title, location string) (string, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return "", err
	}
	m, err := s.loadMaster(ctx, ck, calendarID, masterEventID)
	if err != nil {
		return "", err
	}
	if title == "" {
		title = m.title
	}
	if location == "" {
		location = m.location
	}
	// RECURRENCE-ID and the override's own DTSTART must be in the series' value type/zone, or a
	// TZID-anchored series (e.g. Google-synced) won't recognise the override and it's dropped.
	tzid, isDate := ical.DTStartInfo(m.ics)
	rid := ical.RecurrenceIDLine(recurrenceID, tzid, isDate)
	// Proton requires an override's SEQUENCE >= the master's (code 2001 otherwise).
	signed := ical.SignedVEVENTZoned(m.uid, start, end, tzid, isDate, m.seq, []string{rid})
	encrypted := ical.EncryptedVEVENT(title, location)
	return s.putNewEvent(ctx, ck, calendarID, signed, encrypted)
}

// EventShiftSeries moves the whole series by the delta between the dragged occurrence's original
// start and its new start, preserving the RRULE/EXDATE. The new duration is end-start.
func (s *Service) EventShiftSeries(ctx context.Context, u *keys.Unlocked, calendarID, masterEventID string, recurrenceID, newStart, newEnd time.Time, title, location string) error {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return err
	}
	m, err := s.loadMaster(ctx, ck, calendarID, masterEventID)
	if err != nil {
		return err
	}
	if title == "" {
		title = m.title
	}
	if location == "" {
		location = m.location
	}
	delta := newStart.Sub(recurrenceID)
	mStart := m.start.Add(delta)
	mEnd := mStart.Add(newEnd.Sub(newStart))
	tzid, isDate := ical.DTStartInfo(m.ics)
	signed := ical.SignedVEVENTZoned(m.uid, mStart, mEnd, tzid, isDate, m.seq+1, ical.RecurrenceLines(m.ics))
	encrypted := ical.EncryptedVEVENT(title, location)
	return s.putUpdateEvent(ctx, ck, calendarID, masterEventID, signed, encrypted, m.keyPacket)
}

// EventSplitFollowing splits a series at the dragged occurrence: the master RRULE is truncated
// with UNTIL just before it, and a new (open-ended) series is created at the new time carrying the
// remaining recurrence.
func (s *Service) EventSplitFollowing(ctx context.Context, u *keys.Unlocked, calendarID, masterEventID string, recurrenceID, newStart, newEnd time.Time, title, location string) error {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return err
	}
	m, err := s.loadMaster(ctx, ck, calendarID, masterEventID)
	if err != nil {
		return err
	}
	if m.rrule == "" {
		return fmt.Errorf("event %s is not recurring", masterEventID)
	}
	if title == "" {
		title = m.title
	}
	if location == "" {
		location = m.location
	}
	// 1. Truncate the master so it ends just before the split, keeping its DTSTART/zone and EXDATEs.
	// (RFC 5545: UNTIL is UTC even when DTSTART is zoned, which TruncateRRULE already does.)
	tzid, isDate := ical.DTStartInfo(m.ics)
	truncExtra := append([]string{"RRULE:" + ical.TruncateRRULE(m.rrule, recurrenceID.Add(-time.Second))}, ical.EXDATELines(m.ics)...)
	masterSigned := ical.SignedVEVENTZoned(m.uid, m.start, m.end, tzid, isDate, m.seq+1, truncExtra)
	encrypted := ical.EncryptedVEVENT(m.title, m.location)
	if err := s.putUpdateEvent(ctx, ck, calendarID, masterEventID, masterSigned, encrypted, m.keyPacket); err != nil {
		return err
	}
	// 2. Create the remainder as a new series at the dragged time, in the same zone.
	newSigned := ical.SignedVEVENTZoned(ical.EventUID(), newStart, newEnd, tzid, isDate, 0, []string{"RRULE:" + ical.StripUntilCount(m.rrule)})
	newEncrypted := ical.EncryptedVEVENT(title, location)
	_, err = s.putNewEvent(ctx, ck, calendarID, newSigned, newEncrypted)
	return err
}

// ResolveEvent takes (calendarID, eventID), or a single title which it searches
// across all calendars over the next 30 days.
func (s *Service) ResolveEvent(ctx context.Context, u *keys.Unlocked, args []string) (string, string, error) {
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	needle := args[0]
	cals, err := s.CalendarsList(ctx)
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 30)

	type match struct {
		cal, ev, title string
		when           time.Time
	}
	var matches []match
	for _, c := range cals {
		events, err := s.EventsList(ctx, u, c.ID, start, end)
		if err != nil {
			continue
		}
		for _, e := range events {
			if e.Title != "" && strings.Contains(strings.ToLower(e.Title), strings.ToLower(needle)) {
				matches = append(matches, match{cal: c.ID, ev: e.ID, title: e.Title, when: e.Start})
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", "", &errs.NotFound{Kind: "event", Ref: needle}
	case 1:
		return matches[0].cal, matches[0].ev, nil
	}
	cands := make([]errs.Candidate, 0, len(matches))
	for _, m := range matches {
		cands = append(cands, errs.Candidate{
			ID:    m.ev,
			Label: fmt.Sprintf("%s  %s  (calendar %s)", m.when.Local().Format("2006-01-02 15:04"), m.title, m.cal),
		})
	}
	return "", "", &errs.Ambiguous{Kind: "event", Ref: needle, Candidates: cands}
}

func (s *Service) unlockCalendar(ctx context.Context, u *keys.Unlocked, calendarID string) (*calKeys, error) {
	var mem struct {
		Members []struct {
			ID, CalendarID, Email, AddressID string
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/members"}, &mem); err != nil {
		return nil, err
	}
	var addrKR *pgp.KeyRing
	var memberID string
	for _, m := range mem.Members {
		if kr, ok := u.AddrKR(m.AddressID); ok {
			addrKR = kr
			memberID = m.ID
			break
		}
	}
	if addrKR == nil {
		return nil, fmt.Errorf("no matching address key for calendar %s", calendarID)
	}

	var pass struct {
		Passphrase struct {
			MemberPassphrases []struct {
				MemberID, Passphrase, Signature string
			}
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/passphrase"}, &pass); err != nil {
		return nil, err
	}
	var calPass []byte
	for _, mp := range pass.Passphrase.MemberPassphrases {
		if mp.MemberID != memberID {
			continue
		}
		msg, err := pgp.NewPGPMessageFromArmored(mp.Passphrase)
		if err != nil {
			return nil, err
		}
		sig, err := pgp.NewPGPSignatureFromArmored(mp.Signature)
		if err != nil {
			return nil, err
		}
		dec, err := addrKR.Decrypt(msg, nil, pgp.GetUnixTime())
		if err != nil {
			return nil, fmt.Errorf("decrypt calendar passphrase: %w", err)
		}
		if err := addrKR.VerifyDetached(dec, sig, pgp.GetUnixTime()); err != nil {
			return nil, err
		}
		calPass = dec.GetBinary()
		break
	}
	if calPass == nil {
		return nil, fmt.Errorf("no passphrase found for member %s", memberID)
	}

	var keyRes struct {
		Keys []struct{ PrivateKey string }
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/keys"}, &keyRes); err != nil {
		return nil, err
	}
	calKR, err := pgp.NewKeyRing(nil)
	if err != nil {
		return nil, err
	}
	for _, k := range keyRes.Keys {
		locked, err := pgp.NewKeyFromArmored(k.PrivateKey)
		if err != nil {
			continue
		}
		unlocked, err := locked.Unlock(calPass)
		if err != nil {
			continue
		}
		_ = calKR.AddKey(unlocked)
	}
	if calKR.CountEntities() == 0 {
		return nil, fmt.Errorf("failed to unlock calendar keys")
	}
	return &calKeys{calKR: calKR, addrKR: addrKR, memberID: memberID}, nil
}

// Returns the event's title and location plus, for recurring events, the RRULE and the full
// decrypted iCalendar text (which also carries DTSTART;TZID, DTEND and EXDATE) so callers can
// expand occurrences. The recurrence data is already decrypted here; upstream only used the
// title/location.
func decryptTitleLocation(cards []map[string]any, keyPacket string, ck *calKeys) (title, location, rrule, ics string, sig pgphelper.VerifyResult) {
	kp, _ := base64.StdEncoding.DecodeString(keyPacket)
	decrypted, verdicts, err := pgphelper.DecryptCardsRaw(cards, ck.calKR, ck.addrKR, kp)
	if err != nil {
		return "", "", "", "", pgphelper.Unverified
	}
	joined := strings.Join(decrypted, "\n")
	return ical.Field(joined, "SUMMARY"), ical.Field(joined, "LOCATION"), ical.Field(joined, "RRULE"), joined, pgphelper.Aggregate(verdicts...)
}

func DefaultRange() (time.Time, time.Time) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start, start.AddDate(0, 0, 30)
}
