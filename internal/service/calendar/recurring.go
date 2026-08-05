package calendar

// Recurring-event writes (von-court). Proton stores a recurring series as a single master
// VEVENT carrying the RRULE; editing one occurrence, or the tail of a series, means writing
// additional events that reference the master's UID via RECURRENCE-ID. Upstream's event
// writes only handle the whole-event case.

import (
	"context"
	"fmt"
	"time"

	"github.com/roman-16/proton-cli/internal/account/keys"
	pgphelper "github.com/roman-16/proton-cli/internal/crypto/pgp"
	"github.com/roman-16/proton-cli/internal/ical"
	"github.com/roman-16/proton-cli/internal/proton"
)

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
	desc      string
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
	title, loc, desc, rrule, _, ics, _ := decryptEventCard(r.Event.SharedEvents, r.Event.SharedKeyPacket, ck.calKR, ck.addrKR)
	return &masterInfo{
		uid: r.Event.UID, allDay: r.Event.FullDay == 1,
		start: time.Unix(r.Event.StartTime, 0), end: time.Unix(r.Event.EndTime, 0),
		keyPacket: r.Event.SharedKeyPacket, title: title, location: loc, desc: desc, rrule: rrule, ics: ics,
		seq: ical.Sequence(ics),
	}, nil
}

// putNewEvent PUTs a brand-new calendar event (fresh session key). Used for overrides and the
// remainder series of a split.
func (s *Service) putNewEvent(ctx context.Context, ck *calKeys, calendarID, signed, encrypted string) (string, error) {
	signedCard, encCard, keyPacket, _, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, "")
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
	signedCard, encCard, _, _, err := pgphelper.EncryptAndSignCardSplit(signed, encrypted, ck.calKR, ck.addrKR, keyPacket)
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
	encrypted := ical.EncryptedVEVENT(title, location, "")
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
	encrypted := ical.EncryptedVEVENT(title, location, "")
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
	encrypted := ical.EncryptedVEVENT(m.title, m.location, "")
	if err := s.putUpdateEvent(ctx, ck, calendarID, masterEventID, masterSigned, encrypted, m.keyPacket); err != nil {
		return err
	}
	// 2. Create the remainder as a new series at the dragged time, in the same zone.
	newSigned := ical.SignedVEVENTZoned(ical.EventUID(), newStart, newEnd, tzid, isDate, 0, []string{"RRULE:" + ical.StripUntilCount(m.rrule)})
	newEncrypted := ical.EncryptedVEVENT(title, location, "")
	_, err = s.putNewEvent(ctx, ck, calendarID, newSigned, newEncrypted)
	return err
}
