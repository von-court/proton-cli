package calendar

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/roman-16/proton-cli/internal/account/keys"
	"github.com/roman-16/proton-cli/internal/errs"
	"github.com/roman-16/proton-cli/internal/ical"
	"github.com/roman-16/proton-cli/internal/proton"
)

// Attendee participation statuses (ATTENDEE_STATUS_API in WebClients).
const (
	partstatNeedsAction = 0
	partstatTentative   = 1
	partstatDeclined    = 2
	partstatAccepted    = 3
)

// StatusFromFlag maps the CLI --status word to an ATTENDEE_STATUS_API value.
func StatusFromFlag(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "accept":
		return partstatAccepted, nil
	case "tentative":
		return partstatTentative, nil
	case "decline":
		return partstatDeclined, nil
	}
	return 0, fmt.Errorf("invalid --status %q (use: accept, tentative, decline)", s)
}

// partstatICS maps an ATTENDEE_STATUS_API value to its iCal PARTSTAT verb.
func partstatICS(status int) string {
	switch status {
	case partstatAccepted:
		return "ACCEPTED"
	case partstatTentative:
		return "TENTATIVE"
	case partstatDeclined:
		return "DECLINED"
	}
	return "NEEDS-ACTION"
}

// statusWord is the human phrasing used in the reply email and success line,
// mirroring WebClients' response wording.
func statusWord(status int) string {
	switch status {
	case partstatAccepted:
		return "accepted"
	case partstatTentative:
		return "tentatively accepted"
	case partstatDeclined:
		return "declined"
	}
	return "did not answer"
}

func replyBody(selfEmail string, status int, title string) string {
	return fmt.Sprintf("%s %s your invitation to %s", selfEmail, statusWord(status), title)
}

func replySubject(start time.Time, allDay bool) string {
	if allDay {
		return "Re: Invitation for an event on " + start.Local().Format("2006-01-02")
	}
	return "Re: Invitation for an event starting on " + start.Local().Format("2006-01-02 15:04")
}

// findSelfAttendee returns the attendee ID and matched email for the account's
// own attendee record, matching each address's deterministic X-PM token
// (hex SHA-1 of UID+canonicalEmail) against the event's attendee list.
func findSelfAttendee(uid string, addrs []keys.Address, attendees []rawAttendee) (attendeeID, selfEmail string, ok bool) {
	tokenToEmail := make(map[string]string, len(addrs))
	for _, a := range addrs {
		tokenToEmail[attendeeToken(uid, a.Email)] = a.Email
	}
	for _, at := range attendees {
		if email, found := tokenToEmail[at.Token]; found {
			return at.ID, email, true
		}
	}
	return "", "", false
}

// findSelfAttendeePaged walks the paginated attendees endpoint (page 0 is
// already in the event's AttendeesInfo) when an event has more attendees than
// the inline list carried.
func (s *Service) findSelfAttendeePaged(ctx context.Context, calendarID, eventID, uid string, addrs []keys.Address) (attendeeID, selfEmail string, ok bool, err error) {
	for page := 1; ; page++ {
		var r struct {
			Attendees     []rawAttendee
			MoreAttendees int
		}
		q := url.Values{}
		q.Set("Page", fmt.Sprintf("%d", page))
		if err := s.C.Decode(ctx, proton.Request{
			Method: "GET", Path: fmt.Sprintf("/calendar/v1/%s/events/%s/attendees", calendarID, eventID), Query: q,
		}, &r); err != nil {
			return "", "", false, err
		}
		if id, email, found := findSelfAttendee(uid, addrs, r.Attendees); found {
			return id, email, true, nil
		}
		if r.MoreAttendees != 1 || len(r.Attendees) == 0 {
			return "", "", false, nil
		}
	}
}

// Reply is a ready-to-send METHOD:REPLY invitation answer for the organizer.
type Reply struct {
	ICS        string
	Recipients []string
	Subject    string
	Body       string
}

// RespondResult is the outcome of EventRespond. Reply is non-nil when the
// organizer should be emailed the answer.
type RespondResult struct {
	Title  string
	Status string
	Reply  *Reply
}

// EventRespond replies to an invitation by updating the account's attendee
// participation status (accept/tentative/decline) and, matching WebClients,
// preparing a METHOD:REPLY notification for the organizer.
func (s *Service) EventRespond(ctx context.Context, u *keys.Unlocked, calendarID, eventID string, status int) (*RespondResult, error) {
	var r struct{ Event rawEvent }
	if err := s.C.Decode(ctx, proton.Request{Method: "GET", Path: "/calendar/v1/" + calendarID + "/events/" + eventID}, &r); err != nil {
		return nil, err
	}
	ev := r.Event
	if ev.IsOrganizer == 1 {
		return nil, errs.WithExit(1, fmt.Errorf("you are the organizer of this event; RSVP is for attendees"))
	}

	attendeeID, selfEmail, ok := findSelfAttendee(ev.UID, u.Addresses, ev.AttendeesInfo.Attendees)
	if !ok && ev.AttendeesInfo.MoreAttendees == 1 {
		var err error
		attendeeID, selfEmail, ok, err = s.findSelfAttendeePaged(ctx, calendarID, eventID, ev.UID, u.Addresses)
		if err != nil {
			return nil, err
		}
	}
	if !ok {
		return nil, &errs.NotFound{Kind: "attendee record for you on this event"}
	}

	// Recording the answer needs no calendar keys: the UID is cleartext, the
	// attendee token is deterministic, and the attendee ID comes from the
	// cleartext AttendeesInfo. So update the partstat before any unlock.
	if err := s.C.Decode(ctx, proton.Request{
		Method: "PUT",
		Path:   fmt.Sprintf("/calendar/v1/%s/events/%s/attendees/%s", calendarID, eventID, attendeeID),
		Body:   map[string]any{"Status": status, "UpdateTime": time.Now().Unix()},
	}, nil); err != nil {
		return nil, err
	}

	res := &RespondResult{Status: statusWord(status)}
	// The organizer reply needs the decrypted title/organizer, so it is
	// best-effort: the answer is already recorded even if unlock/decrypt fails.
	if ck, err := s.unlockCalendar(ctx, u, calendarID); err == nil {
		// A received invitation wraps its shared content to the invited address
		// (AddressKeyPacket + that address's key), not the calendar key.
		packet, decKR := ev.SharedKeyPacket, ck.calKR
		if packet == "" && ev.AddressKeyPacket != "" {
			if kr, ok := u.AddrKR(ev.AddressID); ok {
				packet, decKR = ev.AddressKeyPacket, kr
			}
		}
		title, location, _, _, organizer, _, _ := decryptEventCard(ev.SharedEvents, packet, decKR, ck.addrKR)
		res.Title = title
		if organizer != "" && !strings.EqualFold(organizer, selfEmail) {
			start := time.Unix(ev.StartTime, 0)
			end := time.Unix(ev.EndTime, 0)
			ics := ical.ReplyICS(ev.UID, title, location, organizer, selfEmail, partstatICS(status),
				start, end, ev.FullDay == 1, ev.IsProtonProtonInvite == 1)
			res.Reply = &Reply{
				ICS:        ics,
				Recipients: []string{organizer},
				Subject:    replySubject(start, ev.FullDay == 1),
				Body:       replyBody(selfEmail, status, title),
			}
		}
	}
	return res, nil
}
