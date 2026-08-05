package calendar

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	pgp "github.com/ProtonMail/gopenpgp/v2/crypto"
	"github.com/roman-16/proton-cli/internal/account/keys"
	pgphelper "github.com/roman-16/proton-cli/internal/crypto/pgp"
	"github.com/roman-16/proton-cli/internal/ical"
	"github.com/roman-16/proton-cli/internal/proton"
	"github.com/roman-16/proton-cli/internal/ref"
	"github.com/roman-16/proton-cli/internal/units"
)

type Event struct {
	ID          string                 `json:"id"`
	CalendarID  string                 `json:"calendar_id"`
	Title       string                 `json:"title"`
	Location    string                 `json:"location,omitempty"`
	Description string                 `json:"description,omitempty"`
	RRule       string                 `json:"rrule,omitempty"`
	ICS         string                 `json:"ical,omitempty"`
	Start       time.Time              `json:"start"`
	End         time.Time              `json:"end"`
	AllDay      bool                   `json:"all_day"`
	UID         string                 `json:"uid,omitempty"`
	Signature   pgphelper.VerifyResult `json:"signature,omitempty"`
}

const (
	// The API caps a page of events at 100 and reports no total, so a window holding more than
	// that must be walked page by page or the tail is silently lost.
	eventsPageSize = 100
	eventsMaxPages = 100
)

var eventQueryTypes = []string{
	"0", // part-day, starting in the window
	"1", // part-day, ongoing across the window start
	"2", // full-day, starting in the window
	"3", // full-day, ongoing across the window start
}

func (s *Service) EventsList(ctx context.Context, u *keys.Unlocked, calendarID string, start, end time.Time) ([]Event, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return nil, err
	}
	raw, err := s.collectRawEvents(ctx, calendarID, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.toEvent(ck))
	}
	return out, nil
}

func (s *Service) collectRawEvents(ctx context.Context, calendarID string, start, end time.Time) ([]rawEvent, error) {
	// The four quadrants are independent queries over the same window, so run them concurrently:
	// they share this process's session, and serialising them would quadruple the wall-clock of
	// what is already a network-bound call.
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		perTyp = make([][]rawEvent, len(eventQueryTypes))
		firstE error
	)
	for i, typ := range eventQueryTypes {
		wg.Add(1)
		go func(i int, typ string) {
			defer wg.Done()
			raw, err := s.eventsPage(ctx, calendarID, start, end, typ)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstE == nil {
					firstE = err
				}
				return
			}
			perTyp[i] = raw
		}(i, typ)
	}
	wg.Wait()
	if firstE != nil {
		return nil, firstE
	}

	// An event can match more than one quadrant (e.g. a recurring series is both "starting" and
	// "ongoing"), so de-duplicate by ID. Iterating perTyp in Type order keeps the output stable.
	var out []rawEvent
	seen := make(map[string]struct{})
	for _, raw := range perTyp {
		for _, e := range raw {
			if _, dup := seen[e.ID]; dup {
				continue
			}
			seen[e.ID] = struct{}{}
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Service) eventsPage(ctx context.Context, calendarID string, start, end time.Time, typ string) ([]rawEvent, error) {
	var out []rawEvent
	for page := 0; page < eventsMaxPages; page++ {
		q := url.Values{}
		q.Set("Start", fmt.Sprintf("%d", start.Unix()))
		q.Set("End", fmt.Sprintf("%d", end.Unix()))
		q.Set("Timezone", "UTC")
		q.Set("Type", typ)
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
		out = append(out, r.Events...)
		if len(r.Events) < eventsPageSize {
			break
		}
	}
	return out, nil
}

type rawEvent struct {
	ID                   string
	CalendarID           string
	StartTime            int64
	EndTime              int64
	FullDay              int
	UID                  string
	IsOrganizer          int
	IsProtonProtonInvite int
	AddressID            string
	AttendeesInfo        struct {
		Attendees     []rawAttendee
		MoreAttendees int
	}
	SharedKeyPacket  string
	AddressKeyPacket string
	SharedEvents     []map[string]any
}

// rawAttendee is the cleartext per-attendee record from AttendeesInfo: it maps
// the deterministic X-PM token to the server attendee ID and current status.
type rawAttendee struct {
	ID     string
	Token  string
	Status int
}

func (e rawEvent) toEvent(ck *calKeys) Event {
	title, location, description, rrule, _, ics, sig := decryptEventCard(e.SharedEvents, e.SharedKeyPacket, ck.calKR, ck.addrKR)
	return Event{
		ID: e.ID, CalendarID: e.CalendarID, Title: title, Location: location, Description: description, RRule: rrule,
		Start: time.Unix(e.StartTime, 0), End: time.Unix(e.EndTime, 0),
		AllDay: e.FullDay == 1, UID: e.UID, ICS: ics, Signature: sig,
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

// EventInput is the full set of fields for creating an event.
type EventInput struct {
	Title       string
	Location    string
	Description string
	Start, End  time.Time
	AllDay      bool
	RRule       string   // iCal RRULE value, e.g. "FREQ=WEEKLY;COUNT=10"
	Reminders   []string // durations before start, e.g. "15m", "1h", "1d"
	Attendees   []string // participant emails
}

// EventResult is the outcome of EventCreate. Invite is non-nil only when the
// event has external (non-Proton) attendees that need an emailed ICS.
type EventResult struct {
	ID     string
	Invite *Invite
}

// Invite is a ready-to-send ICS invitation for external attendees.
type Invite struct {
	ICS        string
	Recipients []string
	Subject    string
}

// buildAttendees computes per-attendee tokens, the clear-text Attendees list,
// AddedProtonAttendees (the shared session key wrapped to each Proton
// attendee's key) and the list of external (non-Proton) attendee emails.
func (s *Service) buildAttendees(ctx context.Context, uid string, emails []string, sk *pgp.SessionKey) (atts []ical.Attendee, clear, added []map[string]any, external []string, err error) {
	seen := map[string]bool{}
	for _, raw := range emails {
		email := strings.TrimSpace(raw)
		if email == "" || seen[canonicalEmail(email)] {
			continue
		}
		seen[canonicalEmail(email)] = true
		token := attendeeToken(uid, email)
		atts = append(atts, ical.Attendee{Email: email, Token: token})
		clear = append(clear, map[string]any{"Token": token, "Status": 0})

		var keysRes struct {
			Address struct {
				Keys []struct{ PublicKey string }
			}
		}
		if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/core/v4/keys/all", Query: proton.Query("Email", email)}, &keysRes); err != nil {
			return nil, nil, nil, nil, err
		}
		if len(keysRes.Address.Keys) > 0 {
			recKey, err := pgp.NewKeyFromArmored(keysRes.Address.Keys[0].PublicKey)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("parse attendee key for %s: %w", email, err)
			}
			recKR, err := pgp.NewKeyRing(recKey)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			kp, err := recKR.EncryptSessionKey(sk)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			added = append(added, map[string]any{"Email": email, "AddressKeyPacket": base64.StdEncoding.EncodeToString(kp)})
		} else {
			external = append(external, email)
		}
	}
	return atts, clear, added, external, nil
}

func (s *Service) EventCreate(ctx context.Context, u *keys.Unlocked, calendarID string, in EventInput) (*EventResult, error) {
	ck, err := s.unlockCalendar(ctx, u, calendarID)
	if err != nil {
		return nil, err
	}
	notifs, err := buildReminders(in.Reminders)
	if err != nil {
		return nil, err
	}

	organizer := ""
	if len(in.Attendees) > 0 {
		organizer = ck.email
	}
	uid := ical.EventUID()
	signed := ical.SignedVEVENT(uid, in.Start, in.End, in.AllDay, 0, in.RRule, organizer)
	encrypted := ical.EncryptedVEVENT(in.Title, in.Location, in.Description)
	signedCard, encCard, keyPacket, sk, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, "")
	if err != nil {
		return nil, err
	}

	event := map[string]any{
		"Permissions":        63,
		"IsOrganizer":        1,
		"SharedKeyPacket":    keyPacket,
		"SharedEventContent": []any{signedCard, encCard},
		"Notifications":      notifs,
		"Color":              nil,
	}

	var invite *Invite
	if len(in.Attendees) > 0 {
		atts, clear, added, external, err := s.buildAttendees(ctx, uid, in.Attendees, sk)
		if err != nil {
			return nil, err
		}
		attCard, err := pgphelper.EncryptPartWithSessionKey(ical.AttendeesVEVENT(uid, atts), sk, ck.addrKR)
		if err != nil {
			return nil, err
		}
		event["AttendeesEventContent"] = []any{attCard}
		event["Attendees"] = clear
		if len(added) > 0 {
			event["AddedProtonAttendees"] = added
		}
		if len(external) > 0 {
			invite = &Invite{
				ICS:        ical.InviteICS(uid, in.Title, in.Location, in.Description, in.Start, in.End, in.AllDay, organizer, atts),
				Recipients: external,
				Subject:    "Invitation: " + in.Title,
			}
		}
	}

	body := map[string]any{
		"MemberID": ck.memberID,
		"Events":   []map[string]any{{"Overwrite": 0, "Event": event}},
	}
	var r struct {
		Responses []struct {
			Response struct {
				Event struct{ ID string }
			}
		}
	}
	if err := s.C.Decode(ctx, proton.Request{Method: "PUT", Path: "/calendar/v1/" + calendarID + "/events/sync", Body: body}, &r); err != nil {
		return nil, err
	}
	res := &EventResult{Invite: invite}
	if len(r.Responses) > 0 {
		res.ID = r.Responses[0].Response.Event.ID
	}
	return res, nil
}

// EventUpdate leaves empty fields unchanged.
func (s *Service) EventUpdate(ctx context.Context, u *keys.Unlocked, calendarID, eventID, title, location, description string, start, end time.Time) error {
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

	curTitle, curLoc, curDesc, _, curOrganizer, curICS, _ := decryptEventCard(r.Event.SharedEvents, r.Event.SharedKeyPacket, ck.calKR, ck.addrKR)
	if title == "" {
		title = curTitle
	}
	if location == "" {
		location = curLoc
	}
	if description == "" {
		description = curDesc
	}
	if start.IsZero() {
		start = time.Unix(r.Event.StartTime, 0)
	}
	if end.IsZero() {
		end = time.Unix(r.Event.EndTime, 0)
	}

	// Preserve recurrence and organizer across updates (both live in the signed part). The
	// RRULE / RECURRENCE-ID / EXDATE lines are re-emitted verbatim rather than rebuilt from the
	// RRULE value alone, so an in-place edit cannot silently turn a series — or a single
	// occurrence override — into a one-off event.
	//
	// SEQUENCE is bumped from the event's current value rather than hardcoded: Proton rejects a
	// regressing SEQUENCE (code 2001), and an override must stay >= its master, so writing a
	// fixed 1 corrupts any event that has already been edited.
	extra := ical.RecurrenceLines(curICS)
	if curOrganizer != "" {
		extra = append([]string{"ORGANIZER;CN=" + curOrganizer + ":mailto:" + curOrganizer}, extra...)
	}
	signed := ical.SignedVEVENTZoned(r.Event.UID, start, end, "", r.Event.FullDay == 1, ical.Sequence(curICS)+1, extra)
	encrypted := ical.EncryptedVEVENT(title, location, description)
	signedCard, encCard, _, _, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, r.Event.SharedKeyPacket)
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
	m, err := ref.Pick("event", needle, matches,
		func(m match) string { return m.ev },
		func(m match) string {
			return fmt.Sprintf("%s  %s  (calendar %s)", m.when.Local().Format("2006-01-02 15:04"), m.title, m.cal)
		})
	if err != nil {
		return "", "", err
	}
	return m.cal, m.ev, nil
}

func buildReminders(reminders []string) ([]map[string]any, error) {
	if len(reminders) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(reminders))
	for _, r := range reminders {
		trig, err := icalTrigger(r)
		if err != nil {
			return nil, fmt.Errorf("invalid reminder %q: %w", r, err)
		}
		// Type 1 = device notification.
		out = append(out, map[string]any{"Type": 1, "Trigger": trig})
	}
	return out, nil
}

// icalTrigger converts a duration-before-start ("15m", "1h", "1d") into an iCal
// negative trigger ("-PT15M", "-PT60M", "-P1D").
func icalTrigger(dur string) (string, error) {
	d, err := units.ParseDuration(dur)
	if err != nil {
		return "", err
	}
	if d <= 0 {
		return "", fmt.Errorf("must be positive")
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("-P%dD", int(d/(24*time.Hour))), nil
	}
	return fmt.Sprintf("-PT%dM", int(d/time.Minute)), nil
}
