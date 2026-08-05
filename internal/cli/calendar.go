package cli

import (
	"fmt"
	"time"

	"github.com/roman-16/proton-cli/internal/ical"
	"github.com/roman-16/proton-cli/internal/render"
	calsvc "github.com/roman-16/proton-cli/internal/service/calendar"
	mailsvc "github.com/roman-16/proton-cli/internal/service/mail"
	"github.com/roman-16/proton-cli/internal/units"
	"github.com/roman-16/proton-cli/internal/view"
	"github.com/spf13/cobra"
)

func newCalendarCmd() *cobra.Command {
	c := &cobra.Command{Use: "calendar", Short: "Calendar operations"}
	c.AddCommand(calendarsCmd(), eventsCmd())
	return c
}

// ── calendar calendars ──

func calendarsCmd() *cobra.Command {
	c := &cobra.Command{Use: "calendars", Short: "Manage calendars"}
	c.AddCommand(&cobra.Command{
		Use: "list", Short: "List calendars",
		RunE: run([]Step{stepAuth}, func(c *Invocation) error {
			cals, err := c.App.Calendar.CalendarsList(c.Ctx)
			if err != nil {
				return err
			}
			return view.Render(c.R(), c.short(), c.App.IDCache, view.List[calsvc.Calendar]{
				Columns: []view.Column[calsvc.Calendar]{
					{Header: "ID", ID: true, Cell: func(cal calsvc.Calendar) string { return cal.ID }},
					{Header: "NAME", Cell: func(cal calsvc.Calendar) string { return cal.Name }},
					{Header: "COLOR", Cell: func(cal calsvc.Calendar) string { return cal.Color }},
					{Header: "MEMBERS", Cell: func(cal calsvc.Calendar) string { return fmt.Sprintf("%d", cal.MemberCount) }},
				},
				CacheIDs: func(cal calsvc.Calendar) []string { return []string{cal.ID} },
			}, cals)
		}),
	})
	var cName, cColor string
	create := &cobra.Command{
		Use: "create", Short: "Create a calendar",
		RunE: run([]Step{stepAuth}, func(c *Invocation) error {
			if cName == "" {
				return fmt.Errorf("--name is required")
			}
			if err := validateAccentColor(cColor); err != nil {
				return err
			}
			if c.dryRun("create calendar %q", cName) {
				return nil
			}
			u, err := c.App.Unlock(c.Ctx)
			if err != nil {
				return err
			}
			id, err := c.App.Calendar.CalendarCreate(c.Ctx, u, cName, cColor)
			if err != nil {
				return err
			}
			c.R().ID(id, fmt.Sprintf("Created calendar %q", cName))
			return nil
		}),
	}
	create.Flags().StringVar(&cName, "name", "", "Calendar name")
	create.Flags().StringVar(&cColor, "color", "#8080FF", "Calendar color (hex; must be a Proton accent color)")
	c.AddCommand(create)

	c.AddCommand(&cobra.Command{
		Use: "delete CALENDAR_ID", Short: "Delete a calendar (requires password)",
		Args: cobra.ExactArgs(1),
		RunE: run([]Step{stepAuth, stepResolve}, func(c *Invocation) error {
			if c.App.Creds.Password == "" {
				return fmt.Errorf("password is required for calendar delete")
			}
			calID := c.Args[0]
			if c.dryRun("delete calendar %s", calID) {
				return nil
			}
			c.R().Info("Unlocking password scope...")
			if err := c.App.API.UnlockPasswordScope(c.Ctx, c.App.Creds.User, []byte(c.App.Creds.Password)); err != nil {
				return fmt.Errorf("unlock password scope: %w", err)
			}
			if err := c.App.Calendar.CalendarDelete(c.Ctx, calID); err != nil {
				return err
			}
			c.R().Success("Calendar deleted.")
			return nil
		}),
	})

	var renName, renColor string
	rename := &cobra.Command{
		Use: "rename CALENDAR_ID", Short: "Rename or recolor a calendar",
		Args: cobra.ExactArgs(1),
		RunE: run([]Step{stepAuth, stepResolve, stepUnlock}, func(c *Invocation) error {
			if renName == "" && renColor == "" {
				return fmt.Errorf("nothing to update: pass --name or --color")
			}
			if err := validateAccentColor(renColor); err != nil {
				return err
			}
			if c.dryRun("update calendar %s", c.Args[0]) {
				return nil
			}
			if err := c.App.Calendar.CalendarRename(c.Ctx, c.U, c.Args[0], renName, renColor); err != nil {
				return err
			}
			c.R().Success("Calendar updated.")
			return nil
		}),
	}
	rename.Flags().StringVar(&renName, "name", "", "New calendar name")
	rename.Flags().StringVar(&renColor, "color", "", "New color (hex; must be a Proton accent color)")
	c.AddCommand(rename)
	return c
}

// ── calendar events ──

func eventsCmd() *cobra.Command {
	c := &cobra.Command{Use: "events", Short: "Manage calendar events"}

	var listCal, listStart, listEnd string
	list := &cobra.Command{
		Use: "list", Short: "List events",
		RunE: run([]Step{stepAuth}, func(c *Invocation) error {
			u, err := c.App.Unlock(c.Ctx)
			if err != nil {
				return err
			}
			listCalRef, err := resolvePrefix(c.App, listCal)
			if err != nil {
				return err
			}
			id, err := c.App.Calendar.ResolveCalendarID(c.Ctx, listCalRef)
			if err != nil {
				return err
			}
			start, end := calsvc.DefaultRange()
			if listStart != "" {
				t, err := time.Parse("2006-01-02", listStart)
				if err != nil {
					return fmt.Errorf("invalid --start: %w", err)
				}
				start = t
			}
			if listEnd != "" {
				t, err := time.Parse("2006-01-02", listEnd)
				if err != nil {
					return fmt.Errorf("invalid --end: %w", err)
				}
				end = t
			}
			events, err := c.App.Calendar.EventsList(c.Ctx, u, id, start, end)
			if err != nil {
				return err
			}
			return view.Render(c.R(), c.short(), c.App.IDCache, view.List[calsvc.Event]{
				Columns: []view.Column[calsvc.Event]{
					{Header: "DATE", Cell: func(e calsvc.Event) string { return e.Start.Local().Format("2006-01-02") }},
					{Header: "TIME", Cell: func(e calsvc.Event) string { return e.Start.Local().Format("15:04") }},
					{Header: "DURATION", Cell: func(e calsvc.Event) string { return units.Duration(e.End.Sub(e.Start)) }},
					{Header: "TITLE", Cell: func(e calsvc.Event) string { return e.Title }},
					{Header: "LOCATION", Cell: func(e calsvc.Event) string { return e.Location }},
					{Header: "CALENDAR_ID", ID: true, Cell: func(e calsvc.Event) string { return e.CalendarID }},
					{Header: "EVENT_ID", ID: true, Cell: func(e calsvc.Event) string { return e.ID }},
				},
				CacheIDs: func(e calsvc.Event) []string { return []string{e.CalendarID, e.ID} },
			}, events)
		}),
	}
	list.Flags().StringVar(&listCal, "calendar", "", "Calendar ID or name")
	list.Flags().StringVar(&listStart, "start", "", "Start date YYYY-MM-DD")
	list.Flags().StringVar(&listEnd, "end", "", "End date YYYY-MM-DD")
	c.AddCommand(list)

	c.AddCommand(&cobra.Command{
		Use: "get {CALENDAR_ID EVENT_ID | TITLE}", Short: "Get an event (decrypted)",
		Args: cobra.RangeArgs(1, 2),
		RunE: run([]Step{stepAuth, stepResolve}, func(c *Invocation) error {
			u, err := c.App.Unlock(c.Ctx)
			if err != nil {
				return err
			}
			calID, eventID, err := c.App.Calendar.ResolveEvent(c.Ctx, u, c.Args)
			if err != nil {
				return err
			}
			ev, err := c.App.Calendar.EventGet(c.Ctx, u, calID, eventID)
			if err != nil {
				return err
			}
			if c.R().Format != render.FormatText {
				return c.R().Object(ev)
			}
			out := c.R().Stdout
			_, _ = fmt.Fprintf(out, "Event:    %s\n", ev.Title)
			_, _ = fmt.Fprintf(out, "Start:    %s\n", ev.Start.Local().Format("2006-01-02 15:04"))
			_, _ = fmt.Fprintf(out, "End:      %s\n", ev.End.Local().Format("2006-01-02 15:04"))
			_, _ = fmt.Fprintf(out, "Duration: %s\n", units.Duration(ev.End.Sub(ev.Start)))
			if ev.Location != "" {
				_, _ = fmt.Fprintf(out, "Location: %s\n", ev.Location)
			}
			if ev.Description != "" {
				_, _ = fmt.Fprintf(out, "Description: %s\n", ev.Description)
			}
			if ev.RRule != "" {
				_, _ = fmt.Fprintf(out, "Recurrence: %s\n", ev.RRule)
			}
			_, _ = fmt.Fprintf(out, "ID:       %s\n", ev.ID)
			_, _ = fmt.Fprintf(out, "Calendar: %s\n", ev.CalendarID)
			if ev.Signature != "" {
				_, _ = fmt.Fprintf(out, "Signature: %s\n", sigText(ev.Signature))
			}
			return nil
		}),
	})

	var eCal, eTitle, eLocation, eDescription, eStart, eDuration, eRRule string
	var eReminders, eAttendees []string
	var eAllDay bool
	create := &cobra.Command{
		Use: "create", Short: "Create an event",
		RunE: run([]Step{stepAuth}, func(c *Invocation) error {
			if eTitle == "" || eStart == "" {
				return fmt.Errorf("--title and --start are required")
			}
			u, err := c.App.Unlock(c.Ctx)
			if err != nil {
				return err
			}
			eCalRef, err := resolvePrefix(c.App, eCal)
			if err != nil {
				return err
			}
			calID, err := c.App.Calendar.ResolveCalendarID(c.Ctx, eCalRef)
			if err != nil {
				return err
			}
			start, err := ical.ParseTime(eStart)
			if err != nil {
				return fmt.Errorf("invalid --start: %w", err)
			}
			dur, err := time.ParseDuration(eDuration)
			if err != nil {
				return fmt.Errorf("invalid --duration: %w", err)
			}
			if c.dryRun("create event %q in calendar %s with %d attendee(s)", eTitle, calID, len(eAttendees)) {
				return nil
			}
			res, err := c.App.Calendar.EventCreate(c.Ctx, u, calID, calsvc.EventInput{
				Title: eTitle, Location: eLocation, Description: eDescription,
				Start: start, End: start.Add(dur), AllDay: eAllDay,
				RRule: eRRule, Reminders: eReminders, Attendees: eAttendees,
			})
			if err != nil {
				return err
			}
			if res.Invite != nil {
				body := fmt.Sprintf("You have been invited to %q.\n\nThe calendar invitation is attached.", eTitle)
				if _, err := c.App.Mail.Send(c.Ctx, u, mailsvc.SendOptions{
					To:      res.Invite.Recipients,
					Subject: res.Invite.Subject,
					Body:    body,
					InlineAttachments: []mailsvc.InlineAttachment{{
						Filename: "invite.ics", MIMEType: "text/calendar; method=REQUEST", Data: []byte(res.Invite.ICS),
					}},
				}); err != nil {
					c.R().Info(fmt.Sprintf("event created, but sending the invitation email to %d external attendee(s) failed: %v", len(res.Invite.Recipients), err))
				}
			}
			c.R().ID(res.ID, fmt.Sprintf("Created event %q", eTitle))
			return nil
		}),
	}
	create.Flags().StringVar(&eCal, "calendar", "", "Calendar ID or name")
	create.Flags().StringVar(&eTitle, "title", "", "Event title")
	create.Flags().StringVar(&eLocation, "location", "", "Event location")
	create.Flags().StringVar(&eDescription, "description", "", "Event description")
	create.Flags().StringVar(&eStart, "start", "", "Start time (RFC3339 or YYYY-MM-DDTHH:MM)")
	create.Flags().StringVar(&eDuration, "duration", "1h", "Duration")
	create.Flags().BoolVar(&eAllDay, "all-day", false, "All-day event")
	create.Flags().StringVar(&eRRule, "rrule", "", "Recurrence rule (iCal RRULE, e.g. FREQ=WEEKLY;COUNT=10)")
	create.Flags().StringArrayVar(&eReminders, "remind", nil, "Reminder before start (e.g. 15m, 1h, 1d; repeatable)")
	create.Flags().StringArrayVar(&eAttendees, "attendee", nil, "Attendee email; Proton users are added directly, external users are emailed an invite (repeatable)")
	c.AddCommand(create)

	var uTitle, uLocation, uDescription, uStart, uDuration, uOccurrence, uScope string
	update := &cobra.Command{
		Use: "update CALENDAR_ID EVENT_ID", Short: "Update an event",
		Args: cobra.ExactArgs(2),
		RunE: run([]Step{stepAuth, stepResolve}, func(c *Invocation) error {
			u, err := c.App.Unlock(c.Ctx)
			if err != nil {
				return err
			}
			var start, end time.Time
			if uStart != "" {
				t, err := ical.ParseTime(uStart)
				if err != nil {
					return fmt.Errorf("invalid --start: %w", err)
				}
				start = t
				if uDuration != "" {
					d, err := time.ParseDuration(uDuration)
					if err != nil {
						return fmt.Errorf("invalid --duration: %w", err)
					}
					end = start.Add(d)
				}
			}
			// Recurring single-occurrence scopes: --occurrence is the edited occurrence's original
			// start; --scope picks how the edit maps to iCalendar (single override / split / shift).
			if uOccurrence != "" {
				occ, err := ical.ParseTime(uOccurrence)
				if err != nil {
					return fmt.Errorf("invalid --occurrence: %w", err)
				}
				if start.IsZero() || end.IsZero() {
					return fmt.Errorf("--start and --duration are required with --occurrence")
				}
				if c.dryRun("update event with scope %q", uScope) {
					return nil
				}
				switch uScope {
				case "single", "":
					id, err := c.App.Calendar.EventCreateOverride(c.Ctx, u, c.Args[0], c.Args[1], occ, start, end, uTitle, uLocation)
					if err != nil {
						return err
					}
					c.R().ID(id, "Occurrence overridden.")
					return nil
				case "following":
					if err := c.App.Calendar.EventSplitFollowing(c.Ctx, u, c.Args[0], c.Args[1], occ, start, end, uTitle, uLocation); err != nil {
						return err
					}
					c.R().Success("Series split at occurrence.")
					return nil
				case "all":
					if err := c.App.Calendar.EventShiftSeries(c.Ctx, u, c.Args[0], c.Args[1], occ, start, end, uTitle, uLocation); err != nil {
						return err
					}
					c.R().Success("Series shifted.")
					return nil
				default:
					return fmt.Errorf("invalid --scope %q (want single|following|all)", uScope)
				}
			}

			if c.dryRun("update event") {
				return nil
			}
			if err := c.App.Calendar.EventUpdate(c.Ctx, u, c.Args[0], c.Args[1], uTitle, uLocation, uDescription, start, end); err != nil {
				return err
			}
			c.R().Success("Event updated.")
			return nil
		}),
	}
	update.Flags().StringVar(&uTitle, "title", "", "New title")
	update.Flags().StringVar(&uLocation, "location", "", "New location")
	update.Flags().StringVar(&uDescription, "description", "", "New description")
	update.Flags().StringVar(&uStart, "start", "", "New start time")
	update.Flags().StringVar(&uDuration, "duration", "", "New duration")
	update.Flags().StringVar(&uOccurrence, "occurrence", "", "Original start of the edited occurrence (enables recurring single-occurrence scopes)")
	update.Flags().StringVar(&uScope, "scope", "", "Recurring edit scope with --occurrence: single|following|all")
	c.AddCommand(update)

	var respStatus string
	respond := &cobra.Command{
		Use:   "respond {CALENDAR_ID EVENT_ID | TITLE}",
		Short: "Reply to an invitation (--status accept|tentative|decline)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := calsvc.StatusFromFlag(respStatus)
			if err != nil {
				return err
			}
			return run([]Step{stepAuth, stepResolve}, func(c *Invocation) error {
				u, err := c.App.Unlock(c.Ctx)
				if err != nil {
					return err
				}
				calID, eventID, err := c.App.Calendar.ResolveEvent(c.Ctx, u, c.Args)
				if err != nil {
					return err
				}
				if c.dryRun("respond %q to event %s in calendar %s", respStatus, eventID, calID) {
					return nil
				}
				res, err := c.App.Calendar.EventRespond(c.Ctx, u, calID, eventID, status)
				if err != nil {
					return err
				}
				if res.Reply != nil {
					if _, err := c.App.Mail.Send(c.Ctx, u, mailsvc.SendOptions{
						To:      res.Reply.Recipients,
						Subject: res.Reply.Subject,
						Body:    res.Reply.Body,
						InlineAttachments: []mailsvc.InlineAttachment{{
							Filename: "invite.ics", MIMEType: "text/calendar; method=REPLY", Data: []byte(res.Reply.ICS),
						}},
					}); err != nil {
						c.R().Info(fmt.Sprintf("responded, but notifying the organizer by email failed: %v", err))
					}
				}
				name := res.Title
				if name == "" {
					name = eventID
				}
				c.R().Success(fmt.Sprintf("Responded %q to %q", res.Status, name))
				return nil
			})(cmd, args)
		},
	}
	respond.Flags().StringVar(&respStatus, "status", "", "Response: accept, tentative, or decline")
	c.AddCommand(respond)

	c.AddCommand(&cobra.Command{
		Use: "delete {CALENDAR_ID EVENT_ID | TITLE}", Short: "Delete an event",
		Args: cobra.RangeArgs(1, 2),
		RunE: run([]Step{stepAuth, stepResolve}, func(c *Invocation) error {
			u, err := c.App.Unlock(c.Ctx)
			if err != nil {
				return err
			}
			calID, eventID, err := c.App.Calendar.ResolveEvent(c.Ctx, u, c.Args)
			if err != nil {
				return err
			}
			if c.dryRun("delete event %s in calendar %s", eventID, calID) {
				return nil
			}
			if err := c.App.Calendar.EventDelete(c.Ctx, u, calID, eventID); err != nil {
				return err
			}
			c.R().Success("Event deleted.")
			return nil
		}),
	})
	return c
}
