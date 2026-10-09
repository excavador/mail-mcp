package organise

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

func evPreview(account string) *EventPreview {
	start := time.Date(2030, 10, 12, 14, 0, 0, 0, time.UTC)
	return &EventPreview{
		Account: account, Calendar: "primary", Title: "Planning", Start: start, End: start.Add(time.Hour),
		TimeZone: "UTC", Attendees: []string{"a@example.com"}, AttendeesStr: "a@example.com",
	}
}

func TestEventIDIsDerivedFromTheNonce(t *testing.T) {
	o := newOrg(t)
	p, err := o.IssueEvent(evPreview("acct"))
	if err != nil {
		t.Fatal(err)
	}
	id := p.EventID()
	// Google event ids: 5-1024 characters of a-v and 0-9.
	if !regexp.MustCompile(`^mm[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("event id %q is not mm + 32 lowercase hex", id)
	}
	if p.EventID() != id {
		t.Error("EventID is not stable for one preview")
	}
	q, _ := o.IssueEvent(evPreview("acct"))
	if q.EventID() == id || q.Token == p.Token {
		t.Errorf("two previews share an id/token: %s %s", id, q.EventID())
	}
}

func TestEventTokenShapeLookupAndTTL(t *testing.T) {
	o := newOrg(t)
	p, err := o.IssueEvent(evPreview("acct"))
	if err != nil {
		t.Fatal(err)
	}
	exp, mac, ok := strings.Cut(p.Token, ".")
	if !ok || mac == "" {
		t.Fatalf("token %q is not <expiry>.<mac>", p.Token)
	}
	if n, err := strconv.ParseInt(exp, 10, 64); err != nil || n != p.Expires.Unix() {
		t.Fatalf("token expiry %q != Expires %v", exp, p.Expires)
	}
	if d := time.Until(p.Expires); d > PreviewTTL || d < PreviewTTL-5*time.Second {
		t.Errorf("ttl = %v, want about %v", d, PreviewTTL)
	}
	if got, err := o.LookupEvent(p.Token); err != nil || got != p {
		t.Fatalf("LookupEvent = %v, %v", got, err)
	}
}

func TestLookupEventRefusesUnknownTamperedExpiredAndForeignTokens(t *testing.T) {
	o, o2 := newOrg(t), newOrg(t)
	p, _ := o.IssueEvent(evPreview("acct"))
	exp, mac, _ := strings.Cut(p.Token, ".")
	flip := func(s string) string {
		b := []byte(s)
		if b[len(b)-1] == 'A' {
			b[len(b)-1] = 'B'
		} else {
			b[len(b)-1] = 'A'
		}
		return string(b)
	}
	for _, tok := range []string{"", "x", ".", "1.2", "nonsense"} {
		if _, err := o.LookupEvent(tok); !errors.Is(err, ErrExpired) {
			t.Errorf("LookupEvent(%q) = %v, want ErrExpired", tok, err)
		}
	}
	if _, err := o.LookupEvent(exp + "." + flip(mac)); !errors.Is(err, ErrExpired) {
		t.Errorf("one char of the mac changed: %v", err)
	}

	// A forged entry under a stored preview is refused by the MAC, not by the map.
	forged := exp + "." + flip(mac)
	o.mu.Lock()
	o.events[forged] = p
	o.mu.Unlock()
	if _, err := o.LookupEvent(forged); !errors.Is(err, ErrExpired) {
		t.Errorf("forged token under a stored preview accepted: %v", err)
	}

	// Another organiser's key.
	o2.mu.Lock()
	o2.events = map[string]*EventPreview{p.Token: p}
	o2.mu.Unlock()
	if _, err := o2.LookupEvent(p.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("a token signed by another key was accepted: %v", err)
	}

	// A tampered field (what the token covers) breaks the MAC.
	q, _ := o.IssueEvent(evPreview("acct"))
	q.Attendees = append([]string{"evil@example.com"}, q.Attendees...)
	if _, err := o.LookupEvent(q.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("a preview whose attendees changed was accepted: %v", err)
	}
	r, _ := o.IssueEvent(evPreview("acct"))
	r.Title = "Other"
	if _, err := o.LookupEvent(r.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("a preview whose title changed was accepted: %v", err)
	}

	// Expired.
	s, _ := o.IssueEvent(evPreview("acct"))
	s.Expires = time.Now().Add(-time.Second)
	if _, err := o.LookupEvent(s.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("expired token accepted: %v", err)
	}
}

func TestEventTokensAreKeptApartFromIntentAndDraftTokens(t *testing.T) {
	o := newOrg(t)
	ctx := context.Background()
	ev, _ := o.IssueEvent(evPreview("acct"))
	in, err := o.PreviewIntent(ctx, acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	dr, err := o.IssueDraft(&DraftPreview{Account: "acct", Folder: "Drafts", Raw: []byte("Subject: x\r\n\r\nbody\r\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.LookupEvent(in.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("an intent token opened an event: %v", err)
	}
	if _, err := o.LookupEvent(dr.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("a draft token opened an event: %v", err)
	}
	if _, err := o.Lookup(ev.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("an event token opened an intent: %v", err)
	}
	if _, err := o.LookupDraft(ev.Token); !errors.Is(err, ErrExpired) {
		t.Errorf("an event token opened a draft: %v", err)
	}
	if o.Consume(ev.Token) || o.ConsumeDraft(ev.Token) {
		t.Error("an event token was consumed as an intent or a draft")
	}
	if o.ConsumeEvent(in.Token) || o.ConsumeEvent(dr.Token) {
		t.Error("an intent or draft token was consumed as an event")
	}
	// ...and nothing was disturbed by the refusals.
	if _, err := o.LookupEvent(ev.Token); err != nil {
		t.Errorf("event token lost: %v", err)
	}
	if _, err := o.Lookup(in.Token); err != nil {
		t.Errorf("intent token lost: %v", err)
	}
	if _, err := o.LookupDraft(dr.Token); err != nil {
		t.Errorf("draft token lost: %v", err)
	}
}

func TestConsumeEventSucceedsOnlyOnceAndNotWhenExpired(t *testing.T) {
	o := newOrg(t)
	p, _ := o.IssueEvent(evPreview("acct"))
	if !o.ConsumeEvent(p.Token) {
		t.Fatal("first ConsumeEvent of a live token = false")
	}
	if _, err := o.LookupEvent(p.Token); !errors.Is(err, ErrExpired) {
		t.Fatalf("consumed token still valid: %v", err)
	}
	if o.ConsumeEvent(p.Token) {
		t.Error("second ConsumeEvent = true: a token approved two events")
	}
	if o.ConsumeEvent("never issued") {
		t.Error("ConsumeEvent of an unknown token = true")
	}
	q, _ := o.IssueEvent(evPreview("acct"))
	q.Expires = time.Now().Add(-time.Second)
	if o.ConsumeEvent(q.Token) {
		t.Error("ConsumeEvent of an expired token = true")
	}
}

func TestRacingEventConsumesYieldExactlyOneWinner(t *testing.T) {
	o := newOrg(t)
	p, _ := o.IssueEvent(evPreview("acct"))
	start := make(chan struct{})
	wins := make(chan bool, 16)
	for range 16 {
		go func() { <-start; wins <- o.ConsumeEvent(p.Token) }()
	}
	close(start)
	n := 0
	for range 16 {
		select {
		case w := <-wins:
			if w {
				n++
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out")
		}
	}
	if n != 1 {
		t.Errorf("%d winners, want 1", n)
	}
}

func TestOnly32EventPreviewsAreHeldOldestEvicted(t *testing.T) {
	o := newOrg(t)
	var toks []string
	for range maxPreviews + 3 {
		p, err := o.IssueEvent(evPreview("acct"))
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, p.Token)
	}
	for i, tok := range toks {
		_, err := o.LookupEvent(tok)
		if i < 3 && !errors.Is(err, ErrExpired) {
			t.Errorf("preview %d should have been evicted: %v", i, err)
		}
		if i >= 3 && err != nil {
			t.Errorf("preview %d lost: %v", i, err)
		}
	}
}

func TestUnelicitedEventsAreCappedPerAccountPerHour(t *testing.T) {
	if UnelicitedEventsPerHour != 10 {
		t.Fatalf("UnelicitedEventsPerHour = %d, want 10", UnelicitedEventsPerHour)
	}
	o := newOrg(t)
	for i := range UnelicitedEventsPerHour {
		if !o.TakeUnelicitedEvent("a") {
			t.Fatalf("event %d refused", i+1)
		}
	}
	if o.TakeUnelicitedEvent("a") {
		t.Error("the 11th event in an hour was allowed")
	}
	if !o.TakeUnelicitedEvent("b") {
		t.Error("another account shares the cap")
	}
	// Entries older than the window no longer count.
	o.mu.Lock()
	old := time.Now().Add(-unelicitedWindow - time.Minute)
	for i := range o.eventTimes["a"] {
		o.eventTimes["a"][i] = old
	}
	o.mu.Unlock()
	if !o.TakeUnelicitedEvent("a") {
		t.Error("the cap did not expire after the window")
	}
}

func TestEventQuestionKeyIsBoundToThePreview(t *testing.T) {
	o := newOrg(t)
	p, _ := o.IssueEvent(evPreview("acct"))
	q, _ := o.IssueEvent(evPreview("acct"))
	if o.QuestionKeyEvent(p) != o.QuestionKeyEvent(p) {
		t.Error("key is not stable")
	}
	if o.QuestionKeyEvent(p) == o.QuestionKeyEvent(q) {
		t.Error("two previews share a question key")
	}
	if !strings.HasPrefix(o.QuestionKeyEvent(p), "approve-") {
		t.Errorf("key = %q", o.QuestionKeyEvent(p))
	}
	// An event token can carry a question (the 2026-07-28 elicitation state)...
	state, err := o.NewQuestion(p.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !o.TakeQuestion(p.Token, state) {
		t.Error("a question state was refused for an event token")
	}
	if o.TakeQuestion(p.Token, state) {
		t.Error("a question state was accepted twice")
	}
	// ...and a state for one token does not serve another.
	state2, _ := o.NewQuestion(p.Token)
	if o.TakeQuestion(q.Token, state2) {
		t.Error("a question state served another token")
	}
}
