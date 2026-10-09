package server

// Harness for the Google Calendar tools: an httptest fake of the Calendar
// API (one per account), the accounts and server wired to it, and helpers for
// the event tools. Handlers are installed before the fake starts; nothing here
// sleeps to synchronise.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/api/option"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/calendar"
)

// Distinctive credential values: any of them in a tool result or a log line is a leak.
const (
	tCID     = "CID-7f3a91-DISTINCT.apps.example"
	tSecret  = "CSECRET-b81c44-DISTINCT"
	tRefresh = "RTOKEN-1//0gDISTINCT-55aa"
)

var allSecrets = []string{tCID, tSecret, tRefresh}

type creq struct {
	Method, Path string
	Query        url.Values
	Body         []byte
}

type fakeCal struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []creq
}

// newFakeCal starts a fake Calendar API serving routes (ServeMux patterns,
// e.g. "GET /calendars/{id}/events"). Unrouted requests get 404.
func newFakeCal(t *testing.T, routes map[string]http.HandlerFunc) *fakeCal {
	t.Helper()
	f := &fakeCal{t: t}
	mux := http.NewServeMux()
	for pat, h := range routes {
		mux.HandleFunc(pat, func(w http.ResponseWriter, r *http.Request) {
			h(w, r)
		})
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.reqs = append(f.reqs, creq{r.Method, r.URL.Path, r.URL.Query(), body})
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCal) requests(method, path string) []creq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []creq
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCal) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonOK(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, v) }
}

// apiError answers with a Google-style error whose message holds every secret.
func apiError(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		leak := strings.Join(allSecrets, " ")
		writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": "boom " + leak,
			"errors": []map[string]any{{"message": leak, "reason": "x"}}}})
	}
}

// rawError answers with a plain-text body that holds every secret.
func rawError(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "refresh_token="+tRefresh+" client_id="+tCID+" client_secret="+tSecret, code)
	}
}

// insertEcho answers an events.insert with the event as received plus a link.
func insertEcho(extra func(ev map[string]any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ev)
		id, _ := ev["id"].(string)
		ev["htmlLink"] = "https://calendar.example/e/" + id
		ev["status"] = "confirmed"
		if extra != nil {
			extra(ev)
		}
		writeJSON(w, 200, ev)
	}
}

type calAcct struct {
	fake  *fakeCal
	write bool
}

type calEnv struct {
	*wenv
	reg   calendar.Registry
	fakes map[string]*fakeCal
}

// newCalEnv: accounts acct and other (Gmail) and proton (Proton); the ones in
// cals get a calendar served by their fake.
func newCalEnv(t *testing.T, cals map[string]calAcct) *calEnv {
	t.Helper()
	e := newWEnv(t, true, []wspec{{"acct", accounts.Gmail}, {"other", accounts.Gmail}, {"proton", accounts.Proton}}, "INBOX", "Work")
	ce := &calEnv{wenv: e, reg: calendar.Registry{}, fakes: map[string]*fakeCal{}}
	for i := range e.accts {
		name := e.accts[i].Name
		ca, ok := cals[name]
		if !ok {
			continue
		}
		e.accts[i].Calendar = accounts.NewCalendarForTest(tCID, tSecret, tRefresh, ca.write)
		c, err := calendar.New(context.Background(), *e.accts[i].Calendar,
			option.WithEndpoint(ca.fake.srv.URL+"/"), option.WithHTTPClient(ca.fake.srv.Client()))
		if err != nil {
			t.Fatal(err)
		}
		ce.reg[name] = c
		ce.fakes[name] = ca.fake
	}
	return ce
}

func (ce *calEnv) admin(opts ...Option) *mcp.ClientSession {
	return ce.connect(Admin, nil, append([]Option{WithHistory(ce.hist), WithOrganiser(ce.org), WithCalendars(ce.reg)}, opts...)...)
}

func (ce *calEnv) read() *mcp.ClientSession {
	return ce.connect(Read, nil, WithCalendars(ce.reg))
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// captureLog routes slog to a buffer for the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	old := slog.Default()
	buf := &syncBuf{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

func noSecrets(t *testing.T, what, s string) {
	t.Helper()
	for _, sec := range allSecrets {
		if strings.Contains(s, sec) {
			t.Errorf("%s leaks %q: %s", what, sec, s)
		}
	}
}

// ---- event tool helpers ----

type evPrevT struct {
	Account           string   `json:"account"`
	Invitations       string   `json:"invitations"`
	Attendees         string   `json:"attendees"`
	AttendeeCount     int      `json:"attendee_count"`
	Calendar          string   `json:"calendar"`
	Start             string   `json:"start"`
	End               string   `json:"end"`
	TimeZone          string   `json:"time_zone"`
	AddMeetLink       bool     `json:"add_meet_link"`
	Location          string   `json:"location"`
	DescriptionSHA256 string   `json:"description_sha256"`
	Links             []string `json:"links"`
	Warnings          []string `json:"warnings"`
	Notice            string   `json:"notice"`
	PreviewToken      string   `json:"preview_token"`
	Untrusted         struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Location    string   `json:"location"`
		Attendees   []string `json:"attendees"`
	} `json:"untrusted"`
}

type evCreatedT struct {
	Account   string   `json:"account"`
	Calendar  string   `json:"calendar"`
	EventID   string   `json:"event_id"`
	HTMLLink  string   `json:"html_link"`
	MeetLink  string   `json:"meet_link"`
	Invited   []string `json:"invited"`
	HistoryID string   `json:"history_id"`
	Notice    string   `json:"notice"`
}

func evArgs(over map[string]any) map[string]any {
	a := map[string]any{
		"account": "acct", "title": "Planning", "start": "2030-10-12T14:00", "end": "2030-10-12T15:00", "time_zone": "Europe/Amsterdam",
	}
	for k, v := range over {
		if v == nil {
			delete(a, k)
		} else {
			a[k] = v
		}
	}
	return a
}

func previewEv(t *testing.T, cs *mcp.ClientSession, over map[string]any) evPrevT {
	t.Helper()
	p, _ := ok[evPrevT](t, cs, "preview_event", evArgs(over))
	return p
}

func createEvArgs(p evPrevT) map[string]any {
	return map[string]any{
		"preview_token": p.PreviewToken, "approved": true,
		"expect_account": p.Account, "expect_calendar": p.Calendar, "expect_start": p.Start, "expect_end": p.End,
		"expect_attendees": p.Attendees, "expect_attendee_count": p.AttendeeCount, "expect_title": p.Untrusted.Title,
		"expect_location": p.Location, "expect_meet": p.AddMeetLink, "expect_description_sha256": p.DescriptionSHA256,
	}
}

func toolErr(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res := call(t, cs, tool, args)
	if !res.IsError {
		t.Fatalf("%s %v: want a tool error, got %s", tool, args, text(res))
	}
	return text(res)
}
