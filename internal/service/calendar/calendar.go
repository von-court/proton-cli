package calendar

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	pgp "github.com/ProtonMail/gopenpgp/v2/crypto"
	"github.com/roman-16/proton-cli/internal/account/keys"
	pgphelper "github.com/roman-16/proton-cli/internal/crypto/pgp"
	"github.com/roman-16/proton-cli/internal/ical"
	"github.com/roman-16/proton-cli/internal/proton"
)

type Service struct{ C proton.Doer }

func New(c proton.Doer) *Service { return &Service{C: c} }

type calKeys struct {
	calKR    *pgp.KeyRing
	addrKR   *pgp.KeyRing
	memberID string
	email    string
}

func canonicalEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// attendeeToken is the Proton attendee token: hex SHA-1 of UID+canonicalEmail.
func attendeeToken(uid, email string) string {
	sum := sha1.Sum([]byte(uid + canonicalEmail(email)))
	return hex.EncodeToString(sum[:])
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
	var memberID, email string
	for _, m := range mem.Members {
		if kr, ok := u.AddrKR(m.AddressID); ok {
			addrKR = kr
			memberID = m.ID
			email = m.Email
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
	return &calKeys{calKR: calKR, addrKR: addrKR, memberID: memberID, email: email}, nil
}

// decryptEventCard decrypts a Proton calendar event's shared cards. The
// session key is wrapped to decryptionKR (the calendar key for organizer
// copies, or the invited address key for received invitations) via keyPacket;
// signatures are checked against verificationKR.
// ics is the full decrypted iCalendar text, which also carries DTSTART;TZID, DTEND, EXDATE
// and RECURRENCE-ID. Callers expanding a recurring series need it: the API returns only a
// single master event at the series start.
func decryptEventCard(cards []map[string]any, keyPacket string, decryptionKR, verificationKR *pgp.KeyRing) (title, location, description, rrule, organizer, ics string, sig pgphelper.VerifyResult) {
	kp, _ := base64.StdEncoding.DecodeString(keyPacket)
	decrypted, verdicts, err := pgphelper.DecryptCardsRaw(cards, decryptionKR, verificationKR, kp)
	if err != nil {
		return "", "", "", "", "", "", pgphelper.Unverified
	}
	joined := strings.Join(decrypted, "\n")
	organizer = strings.TrimPrefix(ical.Field(joined, "ORGANIZER"), "mailto:")
	return ical.Field(joined, "SUMMARY"), ical.Field(joined, "LOCATION"), ical.Field(joined, "DESCRIPTION"), ical.Field(joined, "RRULE"), organizer, joined, pgphelper.Aggregate(verdicts...)
}

func DefaultRange() (time.Time, time.Time) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start, start.AddDate(0, 0, 30)
}
