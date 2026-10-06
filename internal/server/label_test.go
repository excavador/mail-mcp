package server

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/history"
)

// stableIDs returns the stable ids of the messages of acct with these subjects.
func (e *wenv) stableIDs(account string, subjects ...string) []string {
	e.t.Helper()
	var out []string
	for _, s := range subjects {
		var id string
		if err := e.db().QueryRow(`SELECT stable_id FROM messages WHERE account = ? AND subject = ?`, account, s).Scan(&id); err != nil {
			e.t.Fatalf("stable id of %q: %v", s, err)
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (e *wenv) tag(tag string, subjects ...string) {
	e.t.Helper()
	if _, err := e.cache.AddTags(context.Background(), "acct", tag, e.stableIDs("acct", subjects...), ""); err != nil {
		e.t.Fatal(err)
	}
}

func idsOf(r history.Record, folder string) []string {
	out := append([]string(nil), r.Touched[folder]...)
	sort.Strings(out)
	return out
}

func TestCriterionTagSelectsOnlyTaggedMessagesAndCombinesWithTheRest(t *testing.T) {
	e := basic(t, true) // INBOX: a1, a2 (alice), b1 (bob)
	e.tag("purchase", "a1", "b1")
	cs := e.admin()

	// The tag alone is a matcher; only tagged mail of the folder matches.
	p := preview(t, cs, "acct", "INBOX", map[string]any{"tag": "purchase"}, "Work", "label")
	if p.Matched != 2 {
		t.Fatalf("tag alone matched %d, want 2", p.Matched)
	}
	// ANDed with from, since and before, and the name is normalised like tags.
	if p := preview(t, cs, "acct", "INBOX", map[string]any{"tag": "Purchase", "from": "alice@example.com"}, "Work", "label"); p.Matched != 1 {
		t.Errorf("tag AND from matched %d, want 1", p.Matched)
	}
	if p := preview(t, cs, "acct", "INBOX", map[string]any{"tag": "purchase", "since": "2999-01-01"}, "Work", "label"); p.Matched != 0 {
		t.Errorf("tag AND since matched %d, want 0", p.Matched)
	}
	if p := preview(t, cs, "acct", "INBOX", map[string]any{"tag": "nosuch"}, "Work", "label"); p.Matched != 0 {
		t.Errorf("an unused tag matched %d, want 0", p.Matched)
	}
	// Validated as tags are; the folder is still required.
	requireToolError(t, cs, "preview_intent", previewArgs("acct", "INBOX", map[string]any{"tag": "bad tag!"}, "Work", "label"), "lowercase letters")
	requireToolError(t, cs, "preview_intent", previewArgs("acct", "INBOX", map[string]any{"tag": ""}, "Work", "label"), "at least one of")
	requireToolError(t, cs, "preview_intent", map[string]any{"account": "acct", "criterion": map[string]any{"tag": "purchase"}, "target": "Work", "action": "label"}, "folder")
	// unlabel is undo's, not a caller's.
	requireToolError(t, cs, "preview_intent", previewArgs("acct", "INBOX", map[string]any{"tag": "purchase"}, "Work", "unlabel"), "unlabel is only")

	// Apply labels exactly the tagged ones; the tag shows in the history.
	apply(t, cs, preview(t, cs, "acct", "INBOX", map[string]any{"tag": "purchase"}, "Work", "label"))
	sameSet(t, "Work", e.members("acct", "Work"), "a1", "b1")
	sameSet(t, "INBOX", e.members("acct", "INBOX"), "a1", "a2", "b1")
	h, _ := listHistory(t, cs, "acct")
	if c := h.Records[0].Untrusted.Intent.Criterion; c["tag"] != "purchase" {
		t.Errorf("history criterion = %v, want the tag", c)
	}
}

func TestLabelCapIsAThousandAndMoveStaysAtFifty(t *testing.T) {
	if DefaultMaxUnelicitedLabel != 1000 || DefaultMaxUnelicited != 50 {
		t.Fatalf("defaults are label %d move %d, want 1000 and 50", DefaultMaxUnelicitedLabel, DefaultMaxUnelicited)
	}
	e := newWEnv(t, true, gmailPair, "INBOX", "Work", "Tags")
	e.addMany("INBOX", "bulk@example.com", 60)
	e.refresh("acct")
	e.log.reset()
	cs := e.admin()

	// 60 messages: a label is applied on the client's approval, a move is not.
	mv := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Work", "move")
	requireToolError(t, cs, "apply_intent", applyArgs(mv), "more than 50 messages")
	e.noWrites(t)
	lb := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Work", "label")
	if a := apply(t, cs, lb); a.Done != 60 || a.ApprovedBy != "client-tool-approval" {
		t.Fatalf("label apply = %+v", a)
	}
	// The echo fields stay mandatory for a large label.
	lb2 := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Tags", "label")
	args := applyArgs(lb2)
	args["expect_matched"] = 59
	requireToolError(t, cs, "apply_intent", args, "expect_matched does not match")
	delete(args, "expect_action")
	requireToolError(t, cs, "apply_intent", args, "expect_action")

	// The label cap is configurable and enforced at its own number, and so is
	// the undo of a label.
	cs5 := e.connect(Admin, nil, WithHistory(e.hist), WithOrganiser(e.org), WithMaxUnelicitedLabel(59))
	requireToolError(t, cs5, "apply_intent", applyArgs(lb2), "more than 59 messages")
	cs60 := e.connect(Admin, nil, WithHistory(e.hist), WithOrganiser(e.org), WithMaxUnelicitedLabel(60))
	done := apply(t, cs60, lb2)
	u, _ := ok[prevT](t, cs60, "undo", map[string]any{"history_id": done.HistoryID})
	if u.Action != "unlabel" || u.Matched != 60 {
		t.Fatalf("undo preview = %+v", u)
	}
	requireToolError(t, cs5, "apply_intent", applyArgs(u), "more than 59 messages")
	if a := apply(t, cs60, u); a.Done != 60 {
		t.Errorf("undo apply = %+v", a)
	}
}

func TestApplyDescriptionStatesBothCaps(t *testing.T) {
	e := basic(t, true)
	cs := e.admin()
	d := toolNames(t, cs)["apply_intent"].Description
	for _, want := range []string{"above 50 messages", "for label, and the undo of a label: above 1000"} {
		if !strings.Contains(d, want) {
			t.Errorf("apply_intent description lacks %q: %s", want, d)
		}
	}
}

// labelUndoRoundTrip is the shared body of the Gmail and Proton label undo
// tests: x1 and x3 get the label from the intent, x2 had it before, and x3
// gained it on the server after the preview (and after the last refresh).
func labelUndoRoundTrip(t *testing.T, e *wenv, label string) {
	t.Helper()
	cs := e.admin()
	e.tag("purchase", "x1", "x2", "x3")
	p := preview(t, cs, "acct", "INBOX", map[string]any{"tag": "purchase"}, label, "label")
	if p.Matched != 3 {
		t.Fatalf("preview matched %d, want 3", p.Matched)
	}
	// x3 is labelled by someone else between preview and apply.
	e.addAt("acct", label, "x3", alice, "x3", t0)
	a := apply(t, cs, p)
	if a.Done != 1 {
		t.Fatalf("apply = %+v, want 1 labelled (x2 and x3 already had it)", a)
	}
	got := e.members("acct", label)
	for _, s := range []string{"x1", "x2", "x3"} {
		if !has(got, s) {
			t.Fatalf("%s = %v, want x1 x2 x3", label, got)
		}
	}
	rec, _ := e.hist.Get(a.HistoryID)
	sameSet(t, "touched", idsOf(rec, "INBOX"), e.stableIDs("acct", "x1")...)
	if want := e.stableIDs("acct", "x2", "x3"); len(rec.AlreadyInTarget["INBOX"]) != 2 {
		t.Fatalf("already_in_target = %v, want %v", rec.AlreadyInTarget, want)
	}
	h, _ := listHistory(t, cs, "acct")
	if r := h.Records[0]; r.TouchedCount != 1 || r.AlreadyInTarget != 2 {
		t.Fatalf("history[0] = touched %d already %d, want 1 and 2", r.TouchedCount, r.AlreadyInTarget)
	}

	// Undo removes the label from x1 only.
	u, _ := ok[prevT](t, cs, "undo", map[string]any{"history_id": a.HistoryID})
	if u.Action != "unlabel" || u.Source != label || u.Target != "INBOX" || u.Unlabel != 1 || u.Matched != 1 || u.MoveBack != 0 {
		t.Fatalf("undo preview = %+v", u)
	}
	e.log.reset()
	ua := apply(t, cs, u)
	if ua.Done != 1 {
		t.Errorf("undo apply = %+v, want done 1", ua)
	}
	got = e.members("acct", label)
	if has(got, "x1") || !has(got, "x2") || !has(got, "x3") {
		t.Errorf("%s after undo = %v, want x2 and x3 only", label, got)
	}
	sameSet(t, "INBOX untouched", e.members("acct", "INBOX"), "x1", "x2", "x3")
	if e.log.count("STORE") != 1 || e.log.count("EXPUNGE") != 1 || e.log.count("MOVE") != 0 || e.log.count("COPY") != 0 {
		t.Errorf("verbs = %v, want one STORE and one UID EXPUNGE", e.log.verbs())
	}
	for _, ln := range e.log.lines() {
		if strings.Contains(ln, "EXPUNGE") && !strings.Contains(ln, "UID EXPUNGE") {
			t.Errorf("a plain EXPUNGE was sent: %q", ln)
		}
	}
	h, _ = listHistory(t, cs, "acct")
	r := h.Records[0]
	if r.Kind != "undo" || r.Undoes != a.HistoryID || r.TouchedCount != 1 || r.Action != "unlabel" {
		t.Errorf("undo record = %+v", r)
	}
	urec, _ := e.hist.Get(r.ID)
	sameSet(t, "undo touched", idsOf(urec, label), e.stableIDs("acct", "x1")...)
	requireToolError(t, cs, "undo", map[string]any{"history_id": a.HistoryID}, "already undone")
	// The undo itself is not undoable, nor reappliable.
	requireToolError(t, cs, "undo", map[string]any{"history_id": ua.HistoryID}, "only an applied intent")
}

func TestGmailLabelUndoRemovesOnlyTheLabelsTheIntentAdded(t *testing.T) {
	e := newWEnv(t, true, gmailPair, "INBOX", "Purchases/Imported", "[Gmail]/All Mail")
	for _, id := range []string{"x1", "x2", "x3"} {
		e.add("INBOX", id, alice, id)
	}
	e.add("Purchases/Imported", "x2", alice, "x2")
	e.refresh("acct")
	e.log.reset()
	labelUndoRoundTrip(t, e, "Purchases/Imported")
}

func TestProtonLabelUndoRemovesOnlyTheCopiesTheIntentAdded(t *testing.T) {
	e := newWEnv(t, true, []wspec{{"acct", accounts.Proton}}, "INBOX", "Labels/Purchases")
	for _, id := range []string{"x1", "x2", "x3"} {
		e.add("INBOX", id, alice, id)
	}
	e.add("Labels/Purchases", "x2", alice, "x2")
	e.refresh("acct")
	e.log.reset()
	labelUndoRoundTrip(t, e, "Labels/Purchases")
}

func TestLabelUndoRefusals(t *testing.T) {
	e := newWEnv(t, true, gmailPair, "INBOX", "Work")
	e.add("INBOX", "x1", alice, "x1")
	e.add("Work", "x1", alice, "x1")
	e.refresh("acct")
	cs := e.admin()
	// Everything already carried the label: the intent labelled nothing.
	a := apply(t, cs, preview(t, cs, "acct", "INBOX", fromCrit("alice@example.com"), "Work", "label"))
	requireToolError(t, cs, "undo", map[string]any{"history_id": a.HistoryID}, "labelled nothing")
	// A message that lost the label since is not found, and is not counted.
	e.add("INBOX", "x2", alice, "x2")
	e.refresh("acct")
	b := apply(t, cs, preview(t, cs, "acct", "INBOX", map[string]any{"from": "alice@example.com", "subject_contains": "x2"}, "Work", "label"))
	e.removeBySubject("Work", "x2")
	requireToolError(t, cs, "undo", map[string]any{"history_id": b.HistoryID}, "none of those messages carry the label")
}
