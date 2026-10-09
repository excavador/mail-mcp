// Package calendar is mail-mcp's Google Calendar client: live API calls,
// request-driven (no background job, no cache), one Client per account.
//
// The OAuth client id and secret and the refresh token come from files; none
// of them is ever logged or returned. Errors from the API and from the token
// endpoint are mapped to fixed text (Error), and anything that is logged is
// first scrubbed of the three credential values.
package calendar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// Scopes. Only what an account is configured for is requested.
const (
	ScopeReadonly = gcal.CalendarReadonlyScope
	// ScopeEvents lets the app create events only on calendars the user owns.
	ScopeEvents = "https://www.googleapis.com/auth/calendar.events.owned"
)

// CallBudget bounds one tool's calls to the API. A var so a test can shorten it.
var CallBudget = 30 * time.Second

// Error is a failure whose text is safe to show the client.
type Error string

func (e Error) Error() string { return string(e) }

// ErrAlreadyExists is a create whose event id is taken: the same preview was
// already created (the id is derived from the preview).
var ErrAlreadyExists = Error("an identical event was created before (possibly since cancelled), so Google refused a duplicate; change something in the event or check the calendar")

// Client talks to the Calendar API as one account.
type Client struct {
	svc     *gcal.Service
	secrets []string
	// Write: preview_event and create_event are enabled for the account.
	Write bool
}

// New builds the client for an account's calendar section. extra options
// (option.WithEndpoint, option.WithHTTPClient) are for tests; with
// option.WithHTTPClient the OAuth transport is not installed.
func New(ctx context.Context, c accounts.Calendar, extra ...option.ClientOption) (*Client, error) {
	opts := extra
	if len(extra) == 0 {
		conf := &oauth2.Config{
			ClientID: c.ClientID(), ClientSecret: c.ClientSecret(), Endpoint: google.Endpoint,
		}
		// Only the refresh token is known; the transport refreshes the
		// access token as it expires, and the token endpoint is Google's.
		hc := conf.Client(ctx, &oauth2.Token{RefreshToken: c.RefreshToken()})
		opts = []option.ClientOption{option.WithHTTPClient(hc)}
	}
	svc, err := gcal.NewService(ctx, opts...)
	if err != nil {
		return nil, errors.New("calendar: cannot build the API client")
	}
	return &Client{svc: svc, secrets: []string{c.ClientID(), c.ClientSecret(), c.RefreshToken()}, Write: c.Write}, nil
}

// Redact removes the credential values from s.
func (c *Client) Redact(s string) string {
	for _, v := range c.secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

// Err maps an API or token error to a fixed message and returns the scrubbed
// detail for the log.
func (c *Client) Err(err error) (safe error, detail string) {
	if err == nil {
		return nil, ""
	}
	detail = c.Redact(err.Error())
	var se Error
	if errors.As(err, &se) {
		return se, detail
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch {
		case ge.Code == http.StatusConflict:
			return ErrAlreadyExists, detail
		case ge.Code == http.StatusUnauthorized:
			return Error("calendar authorization failed: the refresh token was rejected; the owner must run calendar-login again"), detail
		case ge.Code == http.StatusForbidden:
			return Error("calendar API refused the request (403): the token may lack the needed scope, or the calendar is not writable"), detail
		case ge.Code == http.StatusNotFound:
			return Error("calendar or event not found"), detail
		case ge.Code == http.StatusTooManyRequests:
			return Error("calendar API rate limit; try again later"), detail
		case ge.Code == http.StatusBadRequest:
			return Error("calendar API rejected the request (400)"), detail
		}
		return Error(fmt.Sprintf("calendar API error (%d)", ge.Code)), detail
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return Error("calendar authorization failed (invalid_grant means the refresh token was revoked or expired); the owner must run calendar-login again"), detail
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Error("calendar API did not answer in time"), detail
	}
	return Error("calendar API unavailable"), detail
}

// CalendarInfo is one entry of the account's calendar list.
type CalendarInfo struct {
	ID, Summary, Description, TimeZone, AccessRole string
	Primary, Selected                              bool
}

// ListCalendars returns the account's calendars.
func (c *Client) ListCalendars(ctx context.Context) ([]CalendarInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, CallBudget)
	defer cancel()
	var out []CalendarInfo
	err := c.svc.CalendarList.List().MaxResults(250).Context(ctx).Pages(ctx, func(p *gcal.CalendarList) error {
		for _, e := range p.Items {
			out = append(out, CalendarInfo{ID: e.Id, Summary: e.Summary, Description: e.Description, TimeZone: e.TimeZone,
				AccessRole: e.AccessRole, Primary: e.Primary, Selected: e.Selected})
		}
		if len(out) >= 250 {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	return out, nil
}

var errStop = errors.New("stop")

// Time is an event boundary: a date (all-day) or an instant with its zone.
type Time struct {
	DateTime string // RFC 3339, empty for all-day
	Date     string // YYYY-MM-DD, all-day only
	TimeZone string
}

// Instant returns the moment t denotes (an all-day date is midnight UTC).
func (t Time) Instant() (time.Time, bool) {
	if t.DateTime != "" {
		v, err := time.Parse(time.RFC3339, t.DateTime)
		return v, err == nil
	}
	if t.Date != "" {
		v, err := time.Parse("2006-01-02", t.Date)
		return v, err == nil
	}
	return time.Time{}, false
}

// Attendee is one invitee.
type Attendee struct {
	Email, Name, Response string
	Optional, Self        bool
}

// Event is the part of a Google event mail-mcp shows. All text is third-party.
type Event struct {
	ID, Status, HTMLLink, Summary, Description, Location string
	CalendarID                                           string
	Start, End                                           Time
	Organizer                                            string
	OrganizerSelf                                        bool
	Attendees                                            []Attendee
	Meet                                                 string // conference/hangout link
	Recurring                                            bool
}

func convTime(t *gcal.EventDateTime) Time {
	if t == nil {
		return Time{}
	}
	return Time{DateTime: t.DateTime, Date: t.Date, TimeZone: t.TimeZone}
}

func conv(e *gcal.Event, calID string) Event {
	out := Event{
		ID: e.Id, Status: e.Status, HTMLLink: e.HtmlLink, Summary: e.Summary, Description: e.Description,
		Location: e.Location, CalendarID: calID, Start: convTime(e.Start), End: convTime(e.End),
		Recurring: e.RecurringEventId != "" || len(e.Recurrence) > 0, Meet: e.HangoutLink,
	}
	if e.Organizer != nil {
		out.Organizer = e.Organizer.Email
		if e.Organizer.DisplayName != "" {
			out.Organizer = e.Organizer.DisplayName + " <" + e.Organizer.Email + ">"
		}
		out.OrganizerSelf = e.Organizer.Self
	}
	for _, a := range e.Attendees {
		out.Attendees = append(out.Attendees, Attendee{Email: a.Email, Name: a.DisplayName, Response: a.ResponseStatus, Optional: a.Optional, Self: a.Self})
	}
	if out.Meet == "" && e.ConferenceData != nil {
		for _, ep := range e.ConferenceData.EntryPoints {
			if ep.EntryPointType == "video" {
				out.Meet = ep.Uri
				break
			}
		}
	}
	return out
}

// ListQuery is a bounded events.list.
type ListQuery struct {
	Calendar     string
	Since, Until time.Time
	Q            string
	Limit        int
	Cursor       string
}

// ListEvents expands recurring events into instances (singleEvents=true) in
// start order, one page.
func (c *Client) ListEvents(ctx context.Context, q ListQuery) (events []Event, next string, err error) {
	ctx, cancel := context.WithTimeout(ctx, CallBudget)
	defer cancel()
	call := c.svc.Events.List(q.Calendar).
		SingleEvents(true).OrderBy("startTime").
		TimeMin(q.Since.UTC().Format(time.RFC3339)).TimeMax(q.Until.UTC().Format(time.RFC3339)).
		MaxResults(int64(q.Limit)).Context(ctx)
	if q.Q != "" {
		call = call.Q(q.Q)
	}
	if q.Cursor != "" {
		call = call.PageToken(q.Cursor)
	}
	res, err := call.Do()
	if err != nil {
		return nil, "", err
	}
	for _, e := range res.Items {
		events = append(events, conv(e, q.Calendar))
	}
	return events, res.NextPageToken, nil
}

// GetEvent returns one event.
func (c *Client) GetEvent(ctx context.Context, calendarID, id string) (Event, error) {
	ctx, cancel := context.WithTimeout(ctx, CallBudget)
	defer cancel()
	e, err := c.svc.Events.Get(calendarID, id).Context(ctx).Do()
	if err != nil {
		return Event{}, err
	}
	return conv(e, calendarID), nil
}

// Busy is one busy interval.
type Busy struct{ Start, End time.Time }

// FreeBusy returns the busy intervals of the primary calendar.
func (c *Client) FreeBusy(ctx context.Context, since, until time.Time) ([]Busy, error) {
	ctx, cancel := context.WithTimeout(ctx, CallBudget)
	defer cancel()
	res, err := c.svc.Freebusy.Query(&gcal.FreeBusyRequest{
		TimeMin: since.UTC().Format(time.RFC3339), TimeMax: until.UTC().Format(time.RFC3339),
		Items: []*gcal.FreeBusyRequestItem{{Id: "primary"}},
	}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	cal, ok := res.Calendars["primary"]
	if !ok {
		return nil, Error("the calendar API returned no free/busy for the primary calendar")
	}
	if len(cal.Errors) > 0 {
		return nil, Error("the calendar API could not compute free/busy for the primary calendar")
	}
	var out []Busy
	for _, b := range cal.Busy {
		s, err1 := time.Parse(time.RFC3339, b.Start)
		e, err2 := time.Parse(time.RFC3339, b.End)
		if err1 == nil && err2 == nil {
			out = append(out, Busy{s, e})
		}
	}
	return out, nil
}

// NewEvent is what CreateEvent inserts.
type NewEvent struct {
	ID                        string // client-chosen, 5-1024 chars of a-v0-9: a retry of the same preview cannot create a second event
	Calendar                  string
	Title, Description, Where string
	Start, End                time.Time // in their zone
	TimeZone                  string
	Attendees                 []string
	Meet                      bool
	MeetRequestID             string
}

// CreateEvent inserts the event and tells every attendee (sendUpdates=all).
func (c *Client) CreateEvent(ctx context.Context, n NewEvent) (Event, error) {
	if !c.Write {
		return Event{}, Error("creating events is not enabled for this account")
	}
	ctx, cancel := context.WithTimeout(ctx, CallBudget)
	defer cancel()
	ev := &gcal.Event{
		Id: n.ID, Summary: n.Title, Description: n.Description, Location: n.Where,
		Start: &gcal.EventDateTime{DateTime: n.Start.Format(time.RFC3339), TimeZone: n.TimeZone},
		End:   &gcal.EventDateTime{DateTime: n.End.Format(time.RFC3339), TimeZone: n.TimeZone},
	}
	for _, a := range n.Attendees {
		ev.Attendees = append(ev.Attendees, &gcal.EventAttendee{Email: a})
	}
	call := c.svc.Events.Insert(n.Calendar, ev).SendUpdates("all").Context(ctx)
	if n.Meet {
		ev.ConferenceData = &gcal.ConferenceData{CreateRequest: &gcal.CreateConferenceRequest{
			RequestId:             n.MeetRequestID,
			ConferenceSolutionKey: &gcal.ConferenceSolutionKey{Type: "hangoutsMeet"},
		}}
		call = call.ConferenceDataVersion(1)
	}
	res, err := call.Do()
	if err != nil {
		return Event{}, err
	}
	return conv(res, n.Calendar), nil
}

// OutcomeUnknown reports whether a failed insert may nevertheless have
// created the event: anything but a definite 4xx answer (timeouts, resets,
// EOF, 5xx, a lost response).
func OutcomeUnknown(err error) bool {
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code >= 400 && ge.Code < 500 {
		return false
	}
	return true
}

// Registry holds the Client of every account that has a calendar.
type Registry map[string]*Client

// NewRegistry builds a Client for each account with a calendar section.
func NewRegistry(ctx context.Context, accts []accounts.Account) (Registry, error) {
	r := Registry{}
	for _, a := range accts {
		if a.Calendar == nil {
			continue
		}
		c, err := New(ctx, *a.Calendar)
		if err != nil {
			return nil, fmt.Errorf("account %q: %w", a.Name, err)
		}
		r[a.Name] = c
	}
	return r, nil
}
