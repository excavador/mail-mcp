package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/calendar"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/organise"
)

const insertPath = "/calendars/primary/events"

// writableEnv: acct and other both have a calendar with write on, each served
// by a fake that echoes inserts. The fakes are returned for inspection.
func writableEnv(t *testing.T, insert http.HandlerFunc) (*calEnv, *fakeCal, *fakeCal) {
	t.Helper()
	if insert == nil {
		insert = insertEcho(nil)
	}
	fa := newFakeCal(t, map[string]http.HandlerFunc{"POST /calendars/{id}/events": insert})
	fo := newFakeCal(t, map[string]http.HandlerFunc{"POST /calendars/{id}/events": insertEcho(nil)})
	return newCalEnv(t, map[string]calAcct{"acct": {fa, true}, "other": {fo, true}}), fa, fo
}

func addrs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d@example.com", prefix, i)
	}
	return out
}

func insertedBody(t *testing.T, r creq) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("insert body %q: %v", r.Body, err)
	}
	return m
}

// ---- preview: time zones and times ----

func TestPreviewEventTimeZones(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	cs := ce.admin()

	for name, tc := range map[string]struct {
		over               map[string]any
		wantStart, wantEnd string
		wantZone           string
	}{
		"local time in Amsterdam (summer)": {map[string]any{}, "2030-10-12T14:00:00+02:00", "2030-10-12T15:00:00+02:00", "Europe/Amsterdam"},
		"local time in Amsterdam (winter)": {map[string]any{"start": "2030-12-12T14:00", "end": "2030-12-12T15:30:15"}, "2030-12-12T14:00:00+01:00", "2030-12-12T15:30:15+01:00", "Europe/Amsterdam"},
		"UTC":                              {map[string]any{"time_zone": "UTC"}, "2030-10-12T14:00:00Z", "2030-10-12T15:00:00Z", "UTC"},
		"RFC 3339 converted to the zone":   {map[string]any{"start": "2030-10-12T12:00:00Z", "end": "2030-10-12T13:00:00Z"}, "2030-10-12T14:00:00+02:00", "2030-10-12T15:00:00+02:00", "Europe/Amsterdam"},
		"RFC 3339 with another offset":     {map[string]any{"time_zone": "Asia/Tokyo", "start": "2030-10-12T14:00:00-05:00", "end": "2030-10-12T15:00:00-05:00"}, "2030-10-13T04:00:00+09:00", "2030-10-13T05:00:00+09:00", "Asia/Tokyo"},
		"exactly seven days":               {map[string]any{"end": "2030-10-19T14:00"}, "2030-10-12T14:00:00+02:00", "2030-10-19T14:00:00+02:00", "Europe/Amsterdam"},
	} {
		t.Run(name, func(t *testing.T) {
			p := previewEv(t, cs, tc.over)
			if p.Start != tc.wantStart || p.End != tc.wantEnd || p.TimeZone != tc.wantZone {
				t.Errorf("start/end/zone = %s / %s / %s, want %s / %s / %s", p.Start, p.End, p.TimeZone, tc.wantStart, tc.wantEnd, tc.wantZone)
			}
		})
	}

	for name, tc := range map[string]struct {
		over map[string]any
		want string
	}{
		"Local":            {map[string]any{"time_zone": "Local"}, "time_zone:"},
		"empty":            {map[string]any{"time_zone": ""}, "time_zone:"},
		"blank":            {map[string]any{"time_zone": "  "}, "time_zone:"},
		"unknown":          {map[string]any{"time_zone": "Foo/Bar"}, "not a known IANA zone"},
		"traversal":        {map[string]any{"time_zone": "../x"}, "time_zone:"},
		"traversal inside": {map[string]any{"time_zone": "Europe/../x"}, "time_zone:"},
		"newline":          {map[string]any{"time_zone": "UTC\nx"}, "time_zone:"},
		"end == start":     {map[string]any{"end": "2030-10-12T14:00"}, "end must be after start"},
		"end < start":      {map[string]any{"end": "2030-10-12T13:59"}, "end must be after start"},
		"over seven days":  {map[string]any{"end": "2030-10-19T14:01"}, "at most 7 days"},
		"not a time":       {map[string]any{"start": "tomorrow"}, "start:"},
		"date only":        {map[string]any{"end": "2030-10-12"}, "end:"},
		"bad end":          {map[string]any{"end": "14:00"}, "end:"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if msg := toolErr(t, cs, "preview_event", evArgs(tc.over)); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q lacks %q", msg, tc.want)
			}
		})
	}
	if fa.total() != 0 {
		t.Errorf("preview made %d API calls", fa.total())
	}
}

func TestPreviewEventFieldValidation(t *testing.T) {
	ce, _, _ := writableEnv(t, nil)
	cs := ce.admin()
	for name, tc := range map[string]struct {
		over map[string]any
		want string
	}{
		"empty title":      {map[string]any{"title": "  "}, "title:"},
		"newline title":    {map[string]any{"title": "a\nb"}, "title:"},
		"invisible title":  {map[string]any{"title": "a\u202eb"}, "title:"},
		"long title":       {map[string]any{"title": strings.Repeat("t", 513)}, "title:"},
		"location control": {map[string]any{"location": "a\x07"}, "location:"},
		"long location":    {map[string]any{"location": strings.Repeat("l", 513)}, "location:"},
		"description NUL":  {map[string]any{"description": "a\x00b"}, "description:"},
		"description 8K+1": {map[string]any{"description": strings.Repeat("d", 8193)}, "description:"},
		"bad calendar":     {map[string]any{"calendar": "a/b"}, "not a calendar id"},
		"calendar dots":    {map[string]any{"calendar": ".."}, "not a calendar id"},
		"unknown account":  {map[string]any{"account": "nope"}, "unknown account"},
	} {
		t.Run(name, func(t *testing.T) {
			if msg := toolErr(t, cs, "preview_event", evArgs(tc.over)); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q lacks %q", msg, tc.want)
			}
		})
	}
	// An account whose calendar is read-only refuses to preview.
	f := newFakeCal(t, nil)
	ce2 := newCalEnv(t, map[string]calAcct{"acct": {f, true}, "other": {f, false}})
	if msg := toolErr(t, ce2.admin(), "preview_event", evArgs(map[string]any{"account": "other"})); !strings.Contains(msg, "not enabled") {
		t.Errorf("read-only calendar: %q", msg)
	}
	// Edges are accepted; CRLF in a description is normalised; the past is a warning.
	p := previewEv(t, cs, map[string]any{"title": strings.Repeat("t", 512), "description": "a\r\nb", "start": "2020-01-01T10:00", "end": "2020-01-01T11:00"})
	if p.Untrusted.Description != "a\nb" || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "past") {
		t.Errorf("preview = %+v", p)
	}
	previewEv(t, cs, map[string]any{"description": strings.Repeat("d", 8192)})
}

func TestPreviewEventAttendeeParsing(t *testing.T) {
	ce, _, _ := writableEnv(t, nil)
	cs := ce.admin()
	p := previewEv(t, cs, map[string]any{"attendees": []string{
		"Alice@Example.COM", "Bob Builder <Bob@example.org>", "alice@example.com", "  carol@sub.example.co.uk ", "d+tag@example.com"}})
	if p.Attendees != "alice@example.com,bob@example.org,carol@sub.example.co.uk,d+tag@example.com" || p.AttendeeCount != 4 {
		t.Errorf("attendees = %q (%d)", p.Attendees, p.AttendeeCount)
	}
	if fmt.Sprint(p.Untrusted.Attendees) != "[alice@example.com bob@example.org carol@sub.example.co.uk d+tag@example.com]" {
		t.Errorf("untrusted.attendees = %v", p.Untrusted.Attendees)
	}
	for _, bad := range []string{
		"", "not-an-email", "a@localhost", "a@b", "a@exa mple.com", "a@example..com", "a@-ex.com", "a@ex-.com", "@example.com",
		"a\x00@example.com", "a@example.com\n", "Bob\x07 <b@example.com>", "a\u200b@example.com",
		"a@@example.com", `"a@b"@example.com`, `"quoted local"@example.com`, "a@b@example.com", "a,b@example.com",
		strings.Repeat("l", 65) + "@example.com", "<a@example.com", "a@example.com, b@example.com",
	} {
		msg := toolErr(t, cs, "preview_event", evArgs(map[string]any{"attendees": []string{bad}}))
		if !strings.Contains(msg, "attendees:") {
			t.Errorf("attendee %q: %q", bad, msg)
		}
	}
	if p := previewEv(t, cs, map[string]any{"attendees": addrs("p", 50)}); p.AttendeeCount != 50 {
		t.Errorf("50 attendees: %d", p.AttendeeCount)
	}
	if msg := toolErr(t, cs, "preview_event", evArgs(map[string]any{"attendees": addrs("p", 51)})); !strings.Contains(msg, "at most 50") {
		t.Errorf("51 attendees: %q", msg)
	}
}

func TestPreviewEventInvitationNotices(t *testing.T) {
	ce, _, _ := writableEnv(t, nil)
	cs := ce.admin()
	for name, tc := range map[string]struct {
		att  []string
		want string
	}{
		"none": {nil, "no attendees, no invitations"},
		"one":  {[]string{"A@example.com"}, "Google will email invitations to 1 attendee: a@example.com"},
		"two":  {[]string{"a@example.com", "b@example.org"}, "Google will email invitations to 2 attendees: a@example.com, b@example.org"},
	} {
		t.Run(name, func(t *testing.T) {
			over := map[string]any{}
			if tc.att != nil {
				over["attendees"] = tc.att
			}
			p := previewEv(t, cs, over)
			if p.Invitations != tc.want {
				t.Errorf("invitations = %q, want %q", p.Invitations, tc.want)
			}
			if !strings.HasPrefix(p.Notice, tc.want) {
				t.Errorf("notice does not start with the invitation text: %q", p.Notice)
			}
			if !strings.Contains(p.Notice, "Nothing has been created yet") {
				t.Errorf("notice: %q", p.Notice)
			}
		})
	}
}

func TestPreviewEventShowsEverythingRecipientsGet(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	cs := ce.admin()
	desc := "Agenda: https://evil.example/login?x=1 and www.other.example/path, mailto:boss@example.com\n" + strings.Repeat("x", 600)
	p := previewEv(t, cs, map[string]any{
		"attendees": []string{"a@example.com"}, "description": desc, "location": "Room 4, see https://maps.example/r4",
		"add_meet_link": true,
	})
	sum := sha256.Sum256([]byte(desc))
	if p.DescriptionSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("description_sha256 = %s", p.DescriptionSHA256)
	}
	for _, link := range []string{"https://evil.example/login?x=1", "www.other.example/path,", "https://maps.example/r4"} {
		found := false
		for _, l := range p.Links {
			found = found || strings.HasPrefix(l, strings.TrimSuffix(link, ","))
		}
		if !found {
			t.Errorf("link %q missing from %v", link, p.Links)
		}
	}
	for _, want := range []string{
		"Ends: 2030-10-12T15:00:00+02:00", "Location: Room 4, see https://maps.example/r4", "Google Meet link: yes",
		"sha256 " + p.DescriptionSHA256, "Links in this invitation: https://evil.example/login?x=1",
	} {
		if !strings.Contains(p.Notice, want) {
			t.Errorf("notice lacks %q:\n%s", want, p.Notice)
		}
	}
	if strings.Contains(p.Notice, strings.Repeat("x", 501)) || !strings.Contains(p.Notice, strings.Repeat("x", 100)) {
		t.Errorf("description in the notice is not capped at 500 characters")
	}
	if !p.AddMeetLink || p.Location != "Room 4, see https://maps.example/r4" {
		t.Errorf("preview = %+v", p)
	}

	// With no description, location or link the notice says so, and the digest is that of "".
	q := previewEv(t, cs, nil)
	empty := sha256.Sum256(nil)
	if q.DescriptionSHA256 != hex.EncodeToString(empty[:]) || len(q.Links) != 0 {
		t.Errorf("empty preview = %+v", q)
	}
	for _, want := range []string{"Location: none", "Google Meet link: no", "Description: none", "Links in this invitation: none"} {
		if !strings.Contains(q.Notice, want) {
			t.Errorf("notice lacks %q:\n%s", want, q.Notice)
		}
	}
	if fa.total() != 0 {
		t.Errorf("preview made %d API calls", fa.total())
	}
}

// ---- create_event: request, result ----

func TestCreateEventRequestShapeAndResult(t *testing.T) {
	ce, fa, _ := writableEnv(t, insertEcho(func(ev map[string]any) { ev["hangoutLink"] = "https://meet.google.com/abc-defg-hij" }))
	cs := ce.admin()
	p := previewEv(t, cs, map[string]any{
		"attendees": []string{"B@example.org", "a@example.com"}, "description": "Agenda", "location": "Room 4", "add_meet_link": true,
	})
	o, _ := ok[evCreatedT](t, cs, "create_event", createEvArgs(p))

	rq := fa.requests("POST", insertPath)
	if len(rq) != 1 {
		t.Fatalf("%d inserts", len(rq))
	}
	if rq[0].Query.Get("sendUpdates") != "all" || rq[0].Query.Get("conferenceDataVersion") != "1" {
		t.Errorf("query = %v", rq[0].Query)
	}
	b := insertedBody(t, rq[0])
	id, _ := b["id"].(string)
	if !regexp.MustCompile(`^mm[0-9a-v]{40}$`).MatchString(id) {
		t.Errorf("id = %q", id)
	}
	if b["summary"] != "Planning" || b["description"] != "Agenda" || b["location"] != "Room 4" {
		t.Errorf("body = %s", rq[0].Body)
	}
	for k, want := range map[string]string{"start": "2030-10-12T14:00:00+02:00", "end": "2030-10-12T15:00:00+02:00"} {
		m, _ := b[k].(map[string]any)
		if m["dateTime"] != want || m["timeZone"] != "Europe/Amsterdam" {
			t.Errorf("%s = %v", k, m)
		}
	}
	atts, _ := b["attendees"].([]any)
	if len(atts) != 2 || atts[0].(map[string]any)["email"] != "b@example.org" || atts[1].(map[string]any)["email"] != "a@example.com" {
		t.Errorf("attendees = %v", atts)
	}
	cd, _ := b["conferenceData"].(map[string]any)
	cr, _ := cd["createRequest"].(map[string]any)
	key, _ := cr["conferenceSolutionKey"].(map[string]any)
	if key["type"] != "hangoutsMeet" || cr["requestId"] == "" || cr["requestId"] == nil {
		t.Errorf("conferenceData = %v", cd)
	}

	if o.EventID != id || o.Account != "acct" || o.Calendar != "primary" || o.MeetLink != "https://meet.google.com/abc-defg-hij" ||
		o.HTMLLink != "https://calendar.example/e/"+id || fmt.Sprint(o.Invited) != "[b@example.org a@example.com]" || o.HistoryID == "" {
		t.Errorf("result = %+v", o)
	}
	if !strings.Contains(o.Notice, "Event created.") || !strings.Contains(o.Notice, "(sent)") {
		t.Errorf("notice = %q", o.Notice)
	}
}

func TestCreateEventWithoutMeetOrAttendeesSendsNeitherAndMeetLinkSources(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	cs := ce.admin()
	p := previewEv(t, cs, nil)
	o, _ := ok[evCreatedT](t, cs, "create_event", createEvArgs(p))
	r := fa.requests("POST", insertPath)[0]
	b := insertedBody(t, r)
	if r.Query.Has("conferenceDataVersion") || b["conferenceData"] != nil || b["attendees"] != nil {
		t.Errorf("unexpected conference/attendees: %v %s", r.Query, r.Body)
	}
	if o.MeetLink != "" || len(o.Invited) != 0 {
		t.Errorf("result = %+v", o)
	}

	// The Meet link comes from a video entry point when there is no hangoutLink.
	ce2, _, _ := writableEnv(t, insertEcho(func(ev map[string]any) {
		ev["conferenceData"] = map[string]any{"entryPoints": []map[string]any{
			{"entryPointType": "phone", "uri": "tel:+31"}, {"entryPointType": "video", "uri": "https://meet.google.com/zzz-yyyy-xxx"}}}
	}))
	cs2 := ce2.admin()
	o2, _ := ok[evCreatedT](t, cs2, "create_event", createEvArgs(previewEv(t, cs2, map[string]any{"add_meet_link": true})))
	if o2.MeetLink != "https://meet.google.com/zzz-yyyy-xxx" {
		t.Errorf("meet link = %q", o2.MeetLink)
	}
}

func TestEventIDIsContentDerivedSoAnIdenticalEventIsRefused(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	ce, fa, _ := writableEnv(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var ev map[string]any
		_ = json.Unmarshal(raw, &ev)
		id, _ := ev["id"].(string)
		mu.Lock()
		dup := seen[id]
		seen[id] = true
		mu.Unlock()
		if dup {
			apiError(409)(w, r)
			return
		}
		insertEcho(nil)(w, r)
	})
	cs := ce.admin()
	over := map[string]any{"attendees": []string{"a@example.com"}}
	o, _ := ok[evCreatedT](t, cs, "create_event", createEvArgs(previewEv(t, cs, over)))

	// The same event previewed again (a new token) gets the same id, and Google's 409 is the answer.
	msg := toolErr(t, cs, "create_event", createEvArgs(previewEv(t, cs, over)))
	if !strings.Contains(msg, "identical event was created before") {
		t.Errorf("409 gives %q", msg)
	}
	noSecrets(t, "409 error", msg)
	rq := fa.requests("POST", insertPath)
	if len(rq) != 2 || insertedBody(t, rq[1])["id"] != o.EventID {
		t.Errorf("ids differ between identical previews: %v", rq)
	}
	// Any change gives another id.
	over["title"] = "Planning 2"
	o3, _ := ok[evCreatedT](t, cs, "create_event", createEvArgs(previewEv(t, cs, over)))
	if o3.EventID == o.EventID {
		t.Error("a different title shares the id")
	}
	// A refused duplicate is not a created event: only two are in the history.
	if recs := ce.hist.List("acct", 10); len(recs) != 2 {
		t.Errorf("history has %d records", len(recs))
	}
}

// ---- create_event: binding, approval ----

func TestCreateEventRefusesEveryMismatchWithoutConsumingTheToken(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	cs := ce.admin()
	p := previewEv(t, cs, map[string]any{
		"attendees": []string{"a@example.com", "b@example.org"}, "description": "Agenda", "location": "Room 4", "add_meet_link": true})
	base := createEvArgs(p)
	mutate := func(k string, v any) map[string]any {
		a := map[string]any{}
		for kk, vv := range base {
			a[kk] = vv
		}
		a[k] = v
		return a
	}
	for _, tc := range []struct {
		key string
		val any
	}{
		{"expect_account", "other"},
		{"expect_calendar", "team@group.calendar.google.com"},
		{"expect_start", "2030-10-12T15:00"},
		{"expect_start", "garbage"},
		{"expect_end", "2030-10-12T16:00"},
		{"expect_end", ""},
		{"expect_attendees", "a@example.com"},
		{"expect_attendees", "a@example.com,b@example.org,c@example.com"},
		{"expect_attendee_count", 3},
		{"expect_attendee_count", 0},
		{"expect_title", "Planning!"},
		{"expect_location", "Room 5"},
		{"expect_location", ""},
		{"expect_meet", false},
		{"expect_description_sha256", strings.Repeat("0", 64)},
		{"expect_description_sha256", ""},
	} {
		msg := toolErr(t, cs, "create_event", mutate(tc.key, tc.val))
		if !strings.Contains(msg, tc.key+" does not match the preview") {
			t.Errorf("%s=%v: %q", tc.key, tc.val, msg)
		}
	}
	if msg := toolErr(t, cs, "create_event", mutate("approved", false)); !strings.Contains(msg, "approved must be true") {
		t.Errorf("approved=false: %q", msg)
	}
	delete(base, "approved")
	if msg := toolErr(t, cs, "create_event", base); msg == "" {
		t.Error("missing approved accepted")
	}
	base["approved"] = true
	if fa.total() != 0 {
		t.Fatalf("%d API calls for refused creates", fa.total())
	}
	// Equivalent spellings the preview itself shows are accepted: upper-case digest, local start.
	args := mutate("expect_description_sha256", strings.ToUpper(p.DescriptionSHA256))
	args["expect_start"] = "2030-10-12T14:00"
	args["expect_end"] = "2030-10-12T15:00"
	ok[evCreatedT](t, cs, "create_event", args)
	if n := len(fa.requests("POST", insertPath)); n != 1 {
		t.Errorf("%d inserts after the refusals", n)
	}
}

func TestCreateEventRefusesBadConsumedTamperedAndForeignTokens(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	cs := ce.admin()
	p := previewEv(t, cs, nil)
	with := func(tok string) map[string]any { a := createEvArgs(p); a["preview_token"] = tok; return a }

	flipped := p.PreviewToken[:len(p.PreviewToken)-1] + map[bool]string{true: "B", false: "A"}[strings.HasSuffix(p.PreviewToken, "A")]
	for _, tok := range []string{"", "x", "1.2", flipped} {
		toolErr(t, cs, "create_event", with(tok))
	}
	if fa.total() != 0 {
		t.Fatalf("%d API calls for bad tokens", fa.total())
	}
	ok[evCreatedT](t, cs, "create_event", createEvArgs(p))
	toolErr(t, cs, "create_event", createEvArgs(p)) // consumed
	if n := len(fa.requests("POST", insertPath)); n != 1 {
		t.Errorf("%d inserts", n)
	}
}

func TestIntentTokenDoesNotOpenCreateEventAndEventTokenDoesNotOpenApply(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	ce.add("INBOX", "m1", "a@example.com", "hello")
	ce.refresh("acct")
	cs := ce.admin()
	in := preview(t, cs, "acct", "INBOX", fromCrit("a@example.com"), "Work", "move")
	ev := previewEv(t, cs, nil)

	args := createEvArgs(ev)
	args["preview_token"] = in.PreviewToken
	toolErr(t, cs, "create_event", args)
	if fa.total() != 0 {
		t.Errorf("an intent token created an event")
	}

	ce.log.reset()
	ap := applyArgs(in)
	ap["preview_token"] = ev.PreviewToken
	toolErr(t, cs, "apply_intent", ap)
	ce.noWrites(t)

	// Both tokens still work for their own tool.
	ok[evCreatedT](t, cs, "create_event", createEvArgs(ev))
	if a := apply(t, cs, in); a.Done != 1 {
		t.Errorf("apply = %+v", a)
	}
}

func TestRacingCreatesOfOneTokenYieldExactlyOneEvent(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	sessions := []*mcp.ClientSession{ce.admin(), ce.admin(), ce.admin(), ce.admin()}
	p := previewEv(t, sessions[0], map[string]any{"attendees": []string{"a@example.com"}})
	args := createEvArgs(p)
	start := make(chan struct{})
	results := make(chan bool, len(sessions))
	for _, cs := range sessions {
		go func() {
			<-start
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_event", Arguments: args})
			results <- err == nil && !res.IsError
		}()
	}
	close(start)
	wins := 0
	for range sessions {
		select {
		case w := <-results:
			if w {
				wins++
			}
		case <-time.After(30 * time.Second):
			t.Fatal("timed out")
		}
	}
	if wins != 1 {
		t.Errorf("%d creates succeeded, want 1", wins)
	}
	if n := len(fa.requests("POST", insertPath)); n != 1 {
		t.Errorf("%d inserts, want 1", n)
	}
	if recs := ce.hist.List("acct", 10); len(recs) != 1 {
		t.Errorf("%d history records", len(recs))
	}
}

// ---- history, undo, unknown outcomes ----

func historyText(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, en.Name()))
		sb.Write(b)
	}
	return sb.String()
}

func TestCreateEventIsRecordedWithoutTheDescriptionAndCannotBeUndone(t *testing.T) {
	ce, _, _ := writableEnv(t, nil)
	cs := ce.admin()
	p := previewEv(t, cs, map[string]any{"attendees": []string{"a@example.com"}, "description": "TOPSECRET-AGENDA-TEXT", "add_meet_link": true})
	o, _ := ok[evCreatedT](t, cs, "create_event", createEvArgs(p))

	recs := ce.hist.List("acct", 10)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	r := recs[0]
	if r.Kind != history.KindCreateEvent || r.ID != o.HistoryID || r.Target != "primary" || r.Preview.ApprovedBy != history.ApprovedClientTool || r.Error != "" {
		t.Errorf("record = %+v", r)
	}
	ev := r.Event
	if ev == nil || ev.EventID != o.EventID || ev.Title != "Planning" || ev.Start != p.Start || ev.End != p.End || ev.TimeZone != "Europe/Amsterdam" ||
		fmt.Sprint(ev.Attendees) != "[a@example.com]" || !ev.Meet || ev.HTMLLink == "" {
		t.Errorf("event info = %+v", ev)
	}
	if txt := historyText(t, ce.histDir); strings.Contains(txt, "TOPSECRET-AGENDA-TEXT") || !strings.Contains(txt, o.EventID) {
		t.Errorf("history file: %s", txt)
	}
	if h, _ := listHistory(t, cs, "acct"); len(h.Records) != 1 || h.Records[0].Kind != "create_event" {
		t.Errorf("list_history = %+v", h)
	}

	msg := toolErr(t, cs, "undo", map[string]any{"history_id": o.HistoryID})
	if !strings.Contains(msg, "event cannot be undone") {
		t.Errorf("undo: %q", msg)
	}
	if len(ce.hist.List("acct", 10)) != 1 {
		t.Error("a refused undo wrote history")
	}
}

func TestCreateEventOutcomeUnknownIsRecordedAndSaysSo(t *testing.T) {
	old := calendar.CallBudget
	calendar.CallBudget = 200 * time.Millisecond
	t.Cleanup(func() { calendar.CallBudget = old })

	cases := map[string]http.HandlerFunc{
		"timeout": func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		"500":     apiError(500),
		"503":     rawError(503),
		"reset": func(w http.ResponseWriter, r *http.Request) {
			hj, _ := w.(http.Hijacker)
			c, _, err := hj.Hijack()
			if err == nil {
				_ = c.Close()
			}
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			ce, fa, _ := writableEnv(t, h)
			cs := ce.admin()
			p := previewEv(t, cs, map[string]any{"attendees": []string{"a@example.com"}})
			msg := toolErr(t, cs, "create_event", createEvArgs(p))
			if !strings.Contains(msg, "may or may not have been created") {
				t.Errorf("error = %q", msg)
			}
			noSecrets(t, "error", msg)
			recs := ce.hist.List("acct", 10)
			if len(recs) != 1 || recs[0].Kind != history.KindCreateEvent || !strings.Contains(recs[0].Error, "may or may not have been created") {
				t.Fatalf("history = %+v", recs)
			}
			if recs[0].Event == nil || !regexp.MustCompile(`^mm[0-9a-v]{40}$`).MatchString(recs[0].Event.EventID) {
				t.Errorf("record has no event id: %+v", recs[0].Event)
			}
			if h, _ := listHistory(t, cs, "acct"); len(h.Records) != 1 || !strings.Contains(h.Records[0].Error, "may or may not") {
				t.Errorf("list_history = %+v", h)
			}
			if len(fa.requests("POST", insertPath)) != 1 {
				t.Errorf("insert attempted %d times", len(fa.requests("POST", insertPath)))
			}
			// The token is spent: nothing is retried behind the owner's back.
			toolErr(t, cs, "create_event", createEvArgs(p))
		})
	}
}

func TestCreateEventDefiniteRefusalsAreNotRecorded(t *testing.T) {
	for _, code := range []int{400, 403, 404} {
		ce, _, _ := writableEnv(t, apiError(code))
		cs := ce.admin()
		msg := toolErr(t, cs, "create_event", createEvArgs(previewEv(t, cs, nil)))
		noSecrets(t, "error", msg)
		if strings.Contains(msg, "may or may not") {
			t.Errorf("%d reported as unknown: %q", code, msg)
		}
		if n := len(ce.hist.List("acct", 10)); n != 0 {
			t.Errorf("%d: %d history records", code, n)
		}
	}
}

// ---- rate limits, elicitation ----

func TestClientApprovalCapsAttendeesAndInviteesPerHour(t *testing.T) {
	ce, fa, fo := writableEnv(t, nil)
	cs := ce.admin()
	mk := func(i, n int) evPrevT {
		return previewEv(t, cs, map[string]any{"title": fmt.Sprintf("Event %d", i), "attendees": addrs(fmt.Sprintf("p%d-", i), n)})
	}

	// 11 attendees: refused on this path, the token survives and a smaller preview is a different event.
	big := mk(0, organise.UnelicitedAttendeesPerEvent+1)
	if msg := toolErr(t, cs, "create_event", createEvArgs(big)); !strings.Contains(msg, "more than 10 attendees cannot be invited on the client's tool approval alone") {
		t.Errorf("11 attendees: %q", msg)
	}
	if fa.total() != 0 {
		t.Fatal("API call for a refused create")
	}
	if _, err := ce.org.LookupEvent(big.PreviewToken); err != nil {
		t.Errorf("a refusal consumed the token: %v", err)
	}

	// 10 + 10 invitees fit the 20 an hour; one more does not, and a refusal counts nothing.
	ok[evCreatedT](t, cs, "create_event", createEvArgs(mk(1, 10)))
	ok[evCreatedT](t, cs, "create_event", createEvArgs(mk(2, 10)))
	one := mk(3, 1)
	if msg := toolErr(t, cs, "create_event", createEvArgs(one)); !strings.Contains(msg, "more than 20 invitees an hour") {
		t.Errorf("21st invitee: %q", msg)
	}
	if n := len(fa.requests("POST", insertPath)); n != 2 {
		t.Errorf("%d inserts, want 2", n)
	}
	// An event without invitees invites nobody and is not capped.
	ok[evCreatedT](t, cs, "create_event", createEvArgs(previewEv(t, cs, map[string]any{"title": "Solo"})))

	// The cap is per account.
	po := previewEv(t, cs, map[string]any{"account": "other", "attendees": addrs("o", 10)})
	ok[evCreatedT](t, cs, "create_event", createEvArgs(po))
	if n := len(fo.requests("POST", insertPath)); n != 1 {
		t.Errorf("other account inserts = %d", n)
	}
}

func TestElicitationShowsTheOwnerEverythingAndIsNotCapped(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	var asked atomic.Int32
	var mu sync.Mutex
	var msgs []string
	cs := ce.connect(Admin, func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked.Add(1)
		mu.Lock()
		msgs = append(msgs, r.Params.Message)
		mu.Unlock()
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}, WithHistory(ce.hist), WithOrganiser(ce.org), WithCalendars(ce.reg), WithApprovalMode(ApprovalElicitation))

	p := previewEv(t, cs, map[string]any{
		"title": "Board", "attendees": []string{"a@example.com", "b@example.org"}, "location": "HQ", "add_meet_link": true,
		"description": "see https://evil.example/x " + strings.Repeat("y", 700)})
	o, _ := ok[evCreatedT](t, cs, "create_event", createEvArgs(p))
	if asked.Load() != 1 || o.EventID == "" {
		t.Fatalf("asked %d, result %+v", asked.Load(), o)
	}
	for _, want := range []string{
		"CALENDAR EVENT", "account acct", "Google will email invitations to 2 attendees: a@example.com, b@example.org",
		"Title: Board", "Starts: 2030-10-12T14:00:00+02:00", "Ends: 2030-10-12T15:00:00+02:00", "Location: HQ", "Google Meet link: yes",
		"sha256 " + p.DescriptionSHA256, "Links in this invitation: https://evil.example/x",
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("approval text lacks %q:\n%s", want, msgs[0])
		}
	}
	if strings.Contains(msgs[0], strings.Repeat("y", 501)) {
		t.Error("the approval text carries an uncapped description")
	}
	if r := ce.hist.List("acct", 1)[0]; r.Preview.ApprovedBy != history.ApprovedElicitation {
		t.Errorf("approved_by = %q", r.Preview.ApprovedBy)
	}

	// Neither the attendee cap nor the invitee cap applies when the owner is asked.
	for i := range 4 {
		ok[evCreatedT](t, cs, "create_event", createEvArgs(previewEv(t, cs, map[string]any{
			"title": fmt.Sprintf("Big %d", i), "attendees": addrs(fmt.Sprintf("q%d-", i), 12)})))
	}
	if asked.Load() != 5 || len(fa.requests("POST", insertPath)) != 5 {
		t.Errorf("asked %d, inserts %d", asked.Load(), len(fa.requests("POST", insertPath)))
	}
}

func TestElicitationDeclineCreatesNothingAndKeepsTheToken(t *testing.T) {
	ce, fa, _ := writableEnv(t, nil)
	var accept atomic.Bool
	cs := ce.connect(Admin, func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		if accept.Load() {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		}
		return &mcp.ElicitResult{Action: "decline"}, nil
	}, WithHistory(ce.hist), WithOrganiser(ce.org), WithCalendars(ce.reg), WithApprovalMode(ApprovalElicitation))
	p := previewEv(t, cs, map[string]any{"attendees": []string{"a@example.com"}})
	if msg := toolErr(t, cs, "create_event", createEvArgs(p)); !strings.Contains(msg, "did not approve") {
		t.Errorf("decline: %q", msg)
	}
	if fa.total() != 0 || len(ce.hist.List("acct", 5)) != 0 {
		t.Error("a declined create had an effect")
	}
	accept.Store(true)
	ok[evCreatedT](t, cs, "create_event", createEvArgs(p))
}
