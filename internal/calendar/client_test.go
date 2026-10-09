package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/excavador/mail-mcp/internal/accounts"
)

const (
	cid     = "CID-7f3a91-DISTINCT.apps.example"
	csecret = "CSECRET-b81c44-DISTINCT"
	crefr   = "RTOKEN-1//0gDISTINCT-55aa"
)

func newClient(t *testing.T, write bool, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), *accounts.NewCalendarForTest(cid, csecret, crefr, write),
		option.WithEndpoint(srv.URL+"/"), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return c, &n
}

func leaky() string { return fmt.Sprintf("id=%s secret=%s refresh=%s", cid, csecret, crefr) }

func TestRedactReplacesAllThreeCredentials(t *testing.T) {
	c, _ := newClient(t, false, nil)
	got := c.Redact("a " + leaky() + " b " + cid + cid)
	for _, s := range []string{cid, csecret, crefr} {
		if strings.Contains(got, s) {
			t.Errorf("%q survived: %s", s, got)
		}
	}
	if strings.Count(got, "[redacted]") != 5 {
		t.Errorf("redacted = %q", got)
	}
	// Empty credentials must not turn every position into a replacement.
	e, err := New(context.Background(), *accounts.NewCalendarForTest("", "", "", false), option.WithEndpoint("http://127.0.0.1:1/"), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Redact("plain text"); got != "plain text" {
		t.Errorf("Redact with empty credentials = %q", got)
	}
}

func TestErrMapsStatusesToFixedTextAndRedactsTheDetail(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{401, "calendar authorization failed"},
		{403, "403"},
		{404, "not found"},
		{429, "rate limit"},
		{400, "rejected the request (400)"},
		{500, "calendar API error (500)"},
		{502, "calendar API error (502)"},
	}
	c, _ := newClient(t, false, nil)
	for _, tc := range cases {
		ge := &googleapi.Error{Code: tc.code, Message: leaky(), Body: `{"error":"` + leaky() + `"}`}
		for _, err := range []error{ge, fmt.Errorf("wrapped: %w", ge)} {
			safe, detail := c.Err(err)
			if !strings.Contains(safe.Error(), tc.want) {
				t.Errorf("%d: safe = %q, want it to contain %q", tc.code, safe, tc.want)
			}
			for _, s := range []string{cid, csecret, crefr} {
				if strings.Contains(safe.Error(), s) || strings.Contains(detail, s) {
					t.Errorf("%d: %q leaked (safe=%q detail=%q)", tc.code, s, safe, detail)
				}
			}
			if !strings.Contains(detail, "[redacted]") {
				t.Errorf("%d: detail lost its content: %q", tc.code, detail)
			}
		}
	}
	// 409 is the already-exists refusal.
	if safe, _ := c.Err(&googleapi.Error{Code: 409}); !errors.Is(safe, ErrAlreadyExists) {
		t.Errorf("409 = %v", safe)
	}
}

func TestErrOtherFailures(t *testing.T) {
	c, _ := newClient(t, false, nil)
	if s, d := c.Err(nil); s != nil || d != "" {
		t.Errorf("Err(nil) = %v, %q", s, d)
	}
	re := &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, Body: []byte(leaky()), ErrorCode: "invalid_grant", ErrorDescription: leaky()}
	safe, detail := c.Err(fmt.Errorf("oauth2: %w", re))
	if !strings.Contains(safe.Error(), "calendar authorization failed") || !strings.Contains(safe.Error(), "invalid_grant") {
		t.Errorf("oauth error = %q", safe)
	}
	for _, s := range []string{cid, csecret, crefr} {
		if strings.Contains(safe.Error(), s) || strings.Contains(detail, s) {
			t.Errorf("oauth error leaks %q: %q / %q", s, safe, detail)
		}
	}
	if s, _ := c.Err(fmt.Errorf("x: %w", context.DeadlineExceeded)); !strings.Contains(s.Error(), "did not answer in time") {
		t.Errorf("deadline = %q", s)
	}
	s, d := c.Err(errors.New("dial tcp 10.1.2.3:443: refused for " + crefr))
	if s.Error() != "calendar API unavailable" || strings.Contains(d, crefr) || strings.Contains(s.Error(), "10.1.2.3") {
		t.Errorf("unknown error = %q / %q", s, d)
	}
	// A fixed Error passes through unchanged.
	if s, _ := c.Err(fmt.Errorf("w: %w", Error("fixed text"))); s.Error() != "fixed text" {
		t.Errorf("Error passthrough = %q", s)
	}
}

func TestOutcomeUnknown(t *testing.T) {
	for code, want := range map[int]bool{400: false, 401: false, 403: false, 404: false, 409: false, 429: false, 500: true, 502: true, 503: true} {
		if got := OutcomeUnknown(&googleapi.Error{Code: code}); got != want {
			t.Errorf("OutcomeUnknown(%d) = %v, want %v", code, got, want)
		}
	}
	for name, err := range map[string]error{
		"deadline": context.DeadlineExceeded, "canceled": context.Canceled, "eof": errors.New("EOF"),
		"wrapped 5xx": fmt.Errorf("x: %w", &googleapi.Error{Code: 500}),
	} {
		if !OutcomeUnknown(err) {
			t.Errorf("%s: outcome should be unknown", name)
		}
	}
	if OutcomeUnknown(fmt.Errorf("x: %w", &googleapi.Error{Code: 404})) {
		t.Error("a wrapped 404 is a definite answer")
	}
}

func TestCreateEventNeedsWriteAndMakesNoCallWithout(t *testing.T) {
	c, n := newClient(t, false, func(http.ResponseWriter, *http.Request) {})
	_, err := c.CreateEvent(context.Background(), NewEvent{ID: "mmabcde", Calendar: "primary", Title: "x", Start: time.Now(), End: time.Now().Add(time.Hour), TimeZone: "UTC"})
	var e Error
	if !errors.As(err, &e) || !strings.Contains(e.Error(), "not enabled") {
		t.Errorf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Errorf("%d API calls without write", n.Load())
	}
}

func TestCreateEventConflictIsAlreadyExists(t *testing.T) {
	c, _ := newClient(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":409,"message":"The requested identifier already exists. ` + crefr + `"}}`))
	})
	_, err := c.CreateEvent(context.Background(), NewEvent{ID: "mmabcde", Calendar: "primary", Title: "x", Start: time.Now(), End: time.Now().Add(time.Hour), TimeZone: "UTC"})
	if err == nil {
		t.Fatal("no error")
	}
	safe, detail := c.Err(err)
	if !errors.Is(safe, ErrAlreadyExists) || strings.Contains(detail, crefr) || OutcomeUnknown(err) {
		t.Errorf("safe=%v detail=%q unknown=%v", safe, detail, OutcomeUnknown(err))
	}
}

func TestFreeBusyAndListEventsParseTheAPI(t *testing.T) {
	c, _ := newClient(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/freeBusy":
			_, _ = w.Write([]byte(`{"calendars":{"primary":{"busy":[{"start":"2030-10-12T10:00:00Z","end":"2030-10-12T11:00:00Z"},{"start":"junk","end":"junk"}]}}}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
				{"id": "e1", "summary": "S", "start": map[string]any{"date": "2030-10-12"}, "end": map[string]any{"date": "2030-10-13"}, "recurringEventId": "r"}}})
		}
	})
	busy, err := c.FreeBusy(context.Background(), time.Date(2030, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2030, 10, 13, 0, 0, 0, 0, time.UTC))
	if err != nil || len(busy) != 1 || !busy[0].End.Equal(time.Date(2030, 10, 12, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("busy = %v, %v (an unparsable interval is dropped)", busy, err)
	}
	evs, next, err := c.ListEvents(context.Background(), ListQuery{Calendar: "primary", Since: time.Now(), Until: time.Now().Add(time.Hour), Limit: 5})
	if err != nil || next != "" || len(evs) != 1 || !evs[0].Recurring || evs[0].Start.Date != "2030-10-12" {
		t.Fatalf("events = %+v %q %v", evs, next, err)
	}
	if at, ok := evs[0].Start.Instant(); !ok || at.Day() != 12 {
		t.Errorf("Instant = %v %v", at, ok)
	}
	if _, ok := (Time{}).Instant(); ok {
		t.Error("zero Time has an instant")
	}
}

func TestCallBudgetBoundsACall(t *testing.T) {
	old := CallBudget
	CallBudget = 100 * time.Millisecond
	t.Cleanup(func() { CallBudget = old })
	c, _ := newClient(t, false, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	_, err := c.ListCalendars(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if s, _ := c.Err(err); !strings.Contains(s.Error(), "did not answer in time") {
		t.Errorf("safe = %q", s)
	}
}

func TestNewRegistryOnlyBuildsAccountsWithACalendar(t *testing.T) {
	with := accounts.Account{Name: "a", Calendar: accounts.NewCalendarForTest(cid, csecret, crefr, true)}
	without := accounts.Account{Name: "b"}
	r, err := NewRegistry(context.Background(), []accounts.Account{with, without})
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 || r["a"] == nil || !r["a"].Write || r["b"] != nil {
		t.Errorf("registry = %v", r)
	}
}
