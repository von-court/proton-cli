// Package calendar provides Proton Calendar operations.
package calendar

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
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

func (s *Service) EventsList(ctx context.Context, u *keys.Unlocked, calendarID string, start, end time.Time) ([]Event, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("Start", fmt.Sprintf("%d", start.Unix()))
	q.Set("End", fmt.Sprintf("%d", end.Unix()))
	q.Set("Timezone", "UTC")
	q.Set("Type", "0")

	var r struct {
		Events []rawEvent
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/events", Query: q}, &r); err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.toEvent(ck))
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

func (s *Service) EventCreate(ctx context.Context, u *keys.Unlocked, calendarID, title, location string, start, end time.Time, allDay bool) (string, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return "", err
	}
	signed := ical.SignedVEVENT(ical.EventUID(), start, end, allDay, 0)
	encrypted := ical.EncryptedVEVENT(title, location)
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
				Event struct{ ID string }
			}
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "PUT", Path: "/calendar/v1/" + calendarID + "/events/sync", Body: body}, &r); err != nil {
		return "", err
	}
	if len(r.Responses) > 0 {
		return r.Responses[0].Response.Event.ID, nil
	}
	return "", nil
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

	// SEQUENCE must strictly increase or Proton ignores the revision (RFC 5545 §3.8.7.4),
	// silently keeping the stored event — so a second edit of the same event would no-op.
	// Read the current SEQUENCE from the decrypted card and bump it (default 0 -> 1).
	seq := 1
	if cur := strings.TrimSpace(ical.Field(curICS, "SEQUENCE")); cur != "" {
		if n, err := strconv.Atoi(cur); err == nil {
			seq = n + 1
		}
	}

	// Preserve the series structure (recurrence rule + exception/extra dates) so editing a
	// recurring event keeps it recurring instead of collapsing it to a single occurrence.
	// (DTSTART is rewritten in UTC; a series anchored to a local zone that crosses a DST
	// boundary could drift by an hour — acceptable for now, noted as a follow-up.)
	var recur []string
	for _, line := range strings.Split(curICS, "\n") {
		l := strings.TrimSpace(line)
		switch u := strings.ToUpper(l); {
		case strings.HasPrefix(u, "RRULE:"),
			strings.HasPrefix(u, "EXDATE:"), strings.HasPrefix(u, "EXDATE;"),
			strings.HasPrefix(u, "RDATE:"), strings.HasPrefix(u, "RDATE;"):
			recur = append(recur, l)
		}
	}

	signed := ical.SignedVEVENT(r.Event.UID, start, end, r.Event.FullDay == 1, seq, recur...)
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
