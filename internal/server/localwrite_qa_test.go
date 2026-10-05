package server

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// QA tests for senders, tags, saved queries and the read/admin split, all
// through the MCP tools.

type qsSenders struct {
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor"`
	Senders    []struct {
		Addr         string `json:"addr"`
		Name         string `json:"name"`
		Kind         string `json:"kind"`
		KindSource   string `json:"kind_source"`
		NMsgs        int    `json:"n_msgs"`
		NRepliedByMe int    `json:"n_replied_by_me"`
		ListID       string `json:"list_id"`
	} `json:"senders"`
}

func (s qsSenders) addrs() []string {
	var out []string
	for _, r := range s.Senders {
		out = append(out, r.Addr)
	}
	slices.Sort(out)
	return out
}

type qsTag struct {
	Tag       string `json:"tag"`
	Matched   int    `json:"matched"`
	Changed   int    `json:"changed"`
	Unchanged int    `json:"unchanged"`
	DryRun    bool   `json:"dry_run"`
	HistoryID string `json:"history_id"`
	Untrusted struct {
		Samples []struct {
			StableID string `json:"stable_id"`
		} `json:"samples"`
	} `json:"untrusted"`
}

type qsTags struct {
	Tags []struct {
		Tag   string `json:"tag"`
		Count int    `json:"count"`
	} `json:"tags"`
}

type qsSearch struct {
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor"`
	Hits       []struct {
		StableID string `json:"stable_id"`
		From     string `json:"from"`
	} `json:"hits"`
	Facets map[string][]struct {
		Value string `json:"value"`
		Count int    `json:"count"`
	} `json:"facets"`
}

func (s qsSearch) ids() []string {
	var out []string
	for _, h := range s.Hits {
		out = append(out, h.StableID)
	}
	slices.Sort(out)
	return out
}

func qsToolErr(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, contains string) {
	t.Helper()
	res := call(t, cs, tool, args)
	if !res.IsError {
		t.Errorf("%s %v: accepted, want an error", tool, args)
		return
	}
	if contains != "" && !strings.Contains(strings.ToLower(text(res)), strings.ToLower(contains)) {
		t.Errorf("%s %v: error %q lacks %q", tool, args, text(res), contains)
	}
}

// qsSids maps the short message ids the tests give to e.add (m00, t1, ...) to
// the cache's stable ids; unknown names are passed through unchanged.
func qsSids(t *testing.T, e *wenv, names ...string) []string {
	t.Helper()
	db := e.db()
	out := make([]string, 0, len(names))
	for _, n := range names {
		var sid string
		err := db.QueryRow(`SELECT stable_id FROM messages WHERE account = 'acct' AND message_id LIKE ?`, "%"+n+"@test%").Scan(&sid)
		if err != nil {
			sid = n
		}
		out = append(out, sid)
	}
	return out
}

func qsSorted(xs []string) []string { xs = slices.Clone(xs); slices.Sort(xs); return xs }

func qsHistoryCount(t *testing.T, cs *mcp.ClientSession) int {
	t.Helper()
	h, _ := listHistory(t, cs, "acct")
	return h.Count
}

func qsSendersOf(t *testing.T, cs *mcp.ClientSession, args map[string]any) qsSenders {
	t.Helper()
	if _, has := args["account"]; !has {
		args["account"] = "acct"
	}
	out, _ := ok[qsSenders](t, cs, "senders", args)
	return out
}

// qsEnv has mail with known ids: shop invoices, alice, a list, and a few
// owner replies. Owner address is me@home.test.
func qsEnv(t *testing.T) *wenv {
	t.Helper()
	e := newWEnv(t, true, gmailPair, "INBOX", "Sent")
	e.cache.SetOwners(map[string][]string{"acct": {"me@home.test"}})
	return e
}

func TestQAGateTransactionalNeverRepliedThroughReadTool(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "shopA", "Shop <no-reply@shop.example>", "Your order 42 has shipped")
	e.add("INBOX", "shopB", "Parcel <noreply@parcel.example>", "Tracking your delivery")
	e.add("INBOX", "bol1", "Bol <info@bol.com>", "Uw bestelling")
	e.add("INBOX", "repl", "Store <no-reply@replied.example>", "Receipt for your payment")
	e.add("INBOX", "alice1", "Alice <alice@x.example>", "lunch")
	e.add("INBOX", "gh1", "GitHub <notifications@github.com>", "[r] PR")
	e.addAt("acct", "INBOX", "list1", "News <news@lists.example>", "weekly", t0, "List-Id: <weekly.lists.example>")
	e.refresh("acct")
	// The owner answers the replied.example store from the Sent folder (the only
	// place a reply is believed); its mail is transactional by header rules but
	// the reply makes it human, so the owner pins it back to transactional to
	// have a transactional sender that was replied to.
	e.addAt("acct", "Sent", "me1", "Me <me@home.test>", "Re: Receipt for your payment", t0.Add(time.Hour), "In-Reply-To: <repl@test>")
	e.refresh("acct")
	ad := e.admin()
	ok[map[string]any](t, ad, "set_sender_kind", map[string]any{"account": "acct", "addr": "no-reply@replied.example", "kind": "transactional"})

	rd := e.connect(Read, nil, WithHistory(e.hist), WithOrganiser(e.org))
	all := qsSendersOf(t, rd, map[string]any{"kind": "transactional"})
	if want := []string{"info@bol.com", "no-reply@replied.example", "no-reply@shop.example", "noreply@parcel.example"}; !slices.Equal(all.addrs(), want) {
		t.Fatalf("transactional senders: %v, want %v", all.addrs(), want)
	}
	got := qsSendersOf(t, rd, map[string]any{"kind": "transactional", "never_replied": true})
	if want := []string{"info@bol.com", "no-reply@shop.example", "noreply@parcel.example"}; !slices.Equal(got.addrs(), want) || got.Total != 3 {
		t.Errorf("gate: %v (total %d), want %v", got.addrs(), got.Total, want)
	}
	for _, r := range got.Senders {
		if r.NRepliedByMe != 0 || r.Kind != "transactional" {
			t.Errorf("gate row %+v", r)
		}
	}
	// The replied transactional sender really has a reply count.
	for _, r := range all.Senders {
		if r.Addr == "no-reply@replied.example" && (r.NRepliedByMe != 1 || r.KindSource != "owner") {
			t.Errorf("replied sender: %+v", r)
		}
	}
}

func TestQAReadServerHasReadToolsAdminHasAllWithRightAnnotations(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "a1", "Alice <alice@x.example>", "hi")
	e.refresh("acct")
	readTools := []string{"senders", "list_tags", "list_saved_queries"}
	writeTools := []string{"set_sender_kind", "tag_messages", "untag_messages", "save_query"}

	rd := toolNames(t, e.connect(Read, nil, WithHistory(e.hist), WithOrganiser(e.org)))
	for _, n := range readTools {
		if rd[n] == nil {
			t.Errorf("read server lacks %s", n)
		}
	}
	for _, n := range writeTools {
		if rd[n] != nil {
			t.Errorf("read server has %s", n)
		}
	}
	// Even without the history and organiser options the read server is the same.
	plain := toolNames(t, e.connect(Read, nil))
	for _, n := range readTools {
		if plain[n] == nil {
			t.Errorf("plain read server lacks %s", n)
		}
	}
	for _, n := range writeTools {
		if plain[n] != nil {
			t.Errorf("plain read server has %s", n)
		}
	}

	ad := toolNames(t, e.admin())
	for _, n := range append(slices.Clone(readTools), writeTools...) {
		tl := ad[n]
		if tl == nil {
			t.Errorf("admin server lacks %s", n)
			continue
		}
		if tl.Annotations == nil {
			t.Errorf("%s has no annotations", n)
			continue
		}
		wantRO := slices.Contains(readTools, n)
		if tl.Annotations.ReadOnlyHint != wantRO {
			t.Errorf("%s ReadOnlyHint=%v, want %v", n, tl.Annotations.ReadOnlyHint, wantRO)
		}
	}
	for _, n := range readTools {
		if rd[n] != nil && !rd[n].Annotations.ReadOnlyHint {
			t.Errorf("read-server %s is not read-only", n)
		}
	}
}

func qsTagEnv(t *testing.T, n int) (*wenv, *mcp.ClientSession) {
	t.Helper()
	e := qsEnv(t)
	for i := 0; i < n; i++ {
		e.add("INBOX", fmt.Sprintf("m%02d", i), fmt.Sprintf("Sender %d <s%d@x.example>", i%3, i%3), fmt.Sprintf("invoice number %d", i))
	}
	e.refresh("acct")
	e.log.reset()
	return e, e.admin()
}

func TestQATagMessagesTakesExactlyOneSelector(t *testing.T) {
	e, cs := qsTagEnv(t, 3)
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{"account": "acct", "tag": "t"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, tool := range []string{"tag_messages", "untag_messages"} {
		for name, args := range map[string]map[string]any{
			"none":             base(nil),
			"empty ids":        base(map[string]any{"stable_ids": []string{}}),
			"ids+tid":          base(map[string]any{"stable_ids": []string{"x"}, "tid": "t1"}),
			"ids+query":        base(map[string]any{"stable_ids": []string{"x"}, "query": "invoice"}),
			"tid+query":        base(map[string]any{"tid": "t1", "query": "invoice"}),
			"tid+filter":       base(map[string]any{"tid": "t1", "from": "alice"}),
			"ids+tid+query":    base(map[string]any{"stable_ids": []string{"x"}, "tid": "t1", "query": "invoice"}),
			"ids+folder only":  base(map[string]any{"stable_ids": []string{"x"}, "folder": "INBOX"}),
			"dry run, no sel.": base(map[string]any{"dry_run": true}),
		} {
			res := call(t, cs, tool, args)
			if !res.IsError {
				t.Errorf("%s %s: accepted", tool, name)
			} else if !strings.Contains(text(res), "exactly one") {
				t.Errorf("%s %s: error %q", tool, name, text(res))
			}
		}
	}
	// Each single selector alone is fine.
	ok[qsTag](t, cs, "tag_messages", base(map[string]any{"stable_ids": qsSids(t, e, "m00")}))
	ok[qsTag](t, cs, "tag_messages", base(map[string]any{"query": "invoice"}))
	ok[qsTag](t, cs, "tag_messages", base(map[string]any{"from": "s1@x"}))
}

func TestQATagDryRunChangesNothingAndSamplesAtMostTen(t *testing.T) {
	e, cs := qsTagEnv(t, 15)
	before := qsHistoryCount(t, cs)
	d, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/dry", "query": "invoice", "dry_run": true})
	if !d.DryRun || d.Matched != 15 || d.Changed != 0 || d.HistoryID != "" {
		t.Errorf("dry run: %+v", d)
	}
	if n := len(d.Untrusted.Samples); n != 10 {
		t.Errorf("samples: %d, want 10", n)
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 0 {
		t.Errorf("dry run created a tag: %+v", l)
	}
	if s, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/dry"}); s.Total != 0 {
		t.Errorf("dry run tagged messages: %d", s.Total)
	}
	if got := qsHistoryCount(t, cs); got != before {
		t.Errorf("dry run wrote history: %d -> %d", before, got)
	}
	// Dry run of untag on tagged messages changes nothing either.
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/dry", "stable_ids": qsSids(t, e, "m00", "m01")})
	h1 := qsHistoryCount(t, cs)
	u, _ := ok[qsTag](t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "case/dry", "query": "invoice", "dry_run": true})
	if u.Matched != 2 || u.Changed != 0 || u.HistoryID != "" {
		t.Errorf("untag dry run: %+v", u)
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 || l.Tags[0].Count != 2 {
		t.Errorf("untag dry run changed tags: %+v", l)
	}
	if got := qsHistoryCount(t, cs); got != h1 {
		t.Errorf("untag dry run wrote history")
	}
}

func TestQARetaggingIsIdempotent(t *testing.T) {
	e, cs := qsTagEnv(t, 4)
	args := map[string]any{"account": "acct", "tag": "case/a", "query": "invoice"}
	first, _ := ok[qsTag](t, cs, "tag_messages", args)
	if first.Changed != 4 || first.Unchanged != 0 || first.HistoryID == "" {
		t.Fatalf("first: %+v", first)
	}
	h := qsHistoryCount(t, cs)
	second, _ := ok[qsTag](t, cs, "tag_messages", args)
	if second.Matched != 4 || second.Changed != 0 || second.Unchanged != 4 || second.HistoryID != "" {
		t.Errorf("second: %+v", second)
	}
	if got := qsHistoryCount(t, cs); got != h {
		t.Errorf("a no-op tag wrote a history record")
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 || l.Tags[0].Count != 4 {
		t.Errorf("list_tags: %+v", l)
	}
	// Unknown ids are ignored, known ones tagged once.
	mixed, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/b", "stable_ids": qsSids(t, e, "m00", "m00", "nope")})
	if mixed.Matched != 1 || mixed.Changed != 1 {
		t.Errorf("duplicate and unknown ids: %+v", mixed)
	}
}

func TestQATagNamesNormalisedAndInvalidRejected(t *testing.T) {
	e, cs := qsTagEnv(t, 2)
	got, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "Case/X", "stable_ids": qsSids(t, e, "m00")})
	if got.Tag != "case/x" {
		t.Errorf("normalised tag %q", got.Tag)
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 || l.Tags[0].Tag != "case/x" {
		t.Errorf("list_tags: %+v", l)
	}
	if s, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "CASE/X"}); s.Total != 1 {
		t.Errorf("search with an upper-case tag: %d", s.Total)
	}
	for _, bad := range []string{"", "  ", "-x", ".x", "/x", "a b", "a;b", "a,b", "é", "a'--", "a\nb", "x\x00y", strings.Repeat("a", 101)} {
		for _, tool := range []string{"tag_messages", "untag_messages"} {
			if res := call(t, cs, tool, map[string]any{"account": "acct", "tag": bad, "stable_ids": qsSids(t, e, "m00")}); !res.IsError {
				t.Errorf("%s accepted tag %q", tool, bad)
			}
		}
		if res := call(t, cs, "search", map[string]any{"account": "acct", "tag": bad}); !res.IsError && bad != "" && strings.TrimSpace(bad) != "" {
			t.Errorf("search accepted tag %q", bad)
		}
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 {
		t.Errorf("an invalid tag was stored: %+v", l)
	}
	// A 100-character tag is the longest allowed.
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": strings.Repeat("a", 100), "stable_ids": qsSids(t, e, "m00")})
}

func TestQATagCapOf5000IsAnErrorNotATruncation(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "seed", "Seed <seed@x.example>", "seed")
	e.refresh("acct")
	cs := e.admin()
	db := e.db()
	bulk := func(from, to int) {
		t.Helper()
		if _, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT ? UNION ALL SELECT i+1 FROM n WHERE i < ?)
INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, internal_date)
SELECT 'acct', 'bulk' || i, 'x', 'Bulk <bulk@cap.example>', 'bulk ' || i, 1700000000 + i, 1700000000 + i FROM n`, from, to); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT ? UNION ALL SELECT i+1 FROM n WHERE i < ?)
INSERT INTO message_thread (account, stable_id, tid) SELECT 'acct', 'bulk' || i, 'bigthread' FROM n`, from, to); err != nil {
			t.Fatal(err)
		}
	}
	bulk(1, 5000)
	// Exactly 5000 is allowed, by query and by thread, with at most 10 samples.
	d, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "cap", "from": "bulk@cap", "dry_run": true})
	if d.Matched != 5000 || len(d.Untrusted.Samples) != 10 {
		t.Fatalf("5000 by query, dry: matched %d samples %d", d.Matched, len(d.Untrusted.Samples))
	}
	if r, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "cap", "tid": "bigthread"}); r.Changed != 5000 {
		t.Fatalf("5000 by tid: %+v", r)
	}
	bulk(5001, 5001)
	for name, args := range map[string]map[string]any{
		"query": {"account": "acct", "tag": "cap2", "from": "bulk@cap"},
		"tid":   {"account": "acct", "tag": "cap2", "tid": "bigthread"},
		"dry":   {"account": "acct", "tag": "cap2", "from": "bulk@cap", "dry_run": true},
		"ids": func() map[string]any {
			ids := make([]string, 5001)
			for i := range ids {
				ids[i] = fmt.Sprintf("bulk%d", i+1)
			}
			return map[string]any{"account": "acct", "tag": "cap2", "stable_ids": ids}
		}(),
	} {
		if !call(t, cs, "tag_messages", args).IsError {
			t.Errorf("%s: 5001 messages accepted (truncated?)", name)
		}
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 || l.Tags[0].Tag != "cap" || l.Tags[0].Count != 5000 {
		t.Errorf("a refused call changed tags: %+v", l)
	}
	if !call(t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "cap", "tid": "bigthread"}).IsError {
		t.Error("untag of 5001 by tid accepted")
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); l.Tags[0].Count != 5000 {
		t.Errorf("a refused untag changed tags: %+v", l)
	}
}

func TestQAUntagByQueryLooksOnlyAtTaggedMessages(t *testing.T) {
	e, cs := qsTagEnv(t, 5)
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/a", "stable_ids": qsSids(t, e, "m00", "m01")})
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/b", "stable_ids": qsSids(t, e, "m02")})
	d, _ := ok[qsTag](t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "case/a", "query": "invoice", "dry_run": true})
	if d.Matched != 2 {
		t.Errorf("untag by query matched %d, want only the 2 tagged (not all 5)", d.Matched)
	}
	u, _ := ok[qsTag](t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "case/a", "query": "invoice"})
	if u.Matched != 2 || u.Changed != 2 || u.Unchanged != 0 {
		t.Errorf("untag: %+v", u)
	}
	l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"})
	if len(l.Tags) != 1 || l.Tags[0].Tag != "case/b" || l.Tags[0].Count != 1 {
		t.Errorf("other tags disturbed: %+v", l)
	}
	// Untagging what is not tagged is a no-op with no history record.
	n, _ := ok[qsTag](t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "case/a", "query": "invoice"})
	if n.Matched != 0 || n.Changed != 0 || n.HistoryID != "" {
		t.Errorf("second untag: %+v", n)
	}
}

func TestQAUndoTagRemovesOnlyRowsOfThatHistoryID(t *testing.T) {
	e, cs := qsTagEnv(t, 4)
	h1, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/a", "stable_ids": qsSids(t, e, "m00", "m01")})
	h2, _ := ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/a", "stable_ids": qsSids(t, e, "m01", "m02")})
	if h2.Changed != 1 {
		t.Fatalf("h2: %+v", h2)
	}
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/other", "stable_ids": qsSids(t, e, "m00")})
	tagged := func(tag string) []string {
		s, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": tag})
		return s.ids()
	}
	r, _ := ok[map[string]any](t, cs, "undo", map[string]any{"history_id": h1.HistoryID})
	if r["untagged"] != float64(2) {
		t.Errorf("undo h1 untagged %v", r["untagged"])
	}
	// m01 was already tagged by h1 when h2 ran, so h2 only added m02: after undoing h1, m02 stays.
	if got := tagged("case/a"); !slices.Equal(got, qsSorted(qsSids(t, e, "m02"))) {
		t.Errorf("after undo h1: %v, want [m02]", got)
	}
	if got := tagged("case/other"); !slices.Equal(got, qsSorted(qsSids(t, e, "m00"))) {
		t.Errorf("another tag was touched: %v", got)
	}
	ok[map[string]any](t, cs, "undo", map[string]any{"history_id": h2.HistoryID})
	if got := tagged("case/a"); len(got) != 0 {
		t.Errorf("after undo h2: %v", got)
	}
	if got := tagged("case/other"); !slices.Equal(got, qsSorted(qsSids(t, e, "m00"))) {
		t.Errorf("another tag was touched by h2 undo: %v", got)
	}
}

func TestQAUndoUntagRestoresExactlyTheRemovedRows(t *testing.T) {
	e, cs := qsTagEnv(t, 4)
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/a", "stable_ids": qsSids(t, e, "m00", "m01", "m02")})
	u, _ := ok[qsTag](t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "case/a", "stable_ids": qsSids(t, e, "m00", "m01")})
	if u.Changed != 2 {
		t.Fatalf("untag: %+v", u)
	}
	r, _ := ok[map[string]any](t, cs, "undo", map[string]any{"history_id": u.HistoryID})
	if r["retagged"] != float64(2) {
		t.Errorf("retagged %v", r["retagged"])
	}
	s, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/a"})
	if !slices.Equal(s.ids(), qsSorted(qsSids(t, e, "m00", "m01", "m02"))) {
		t.Errorf("after undo: %v", s.ids())
	}
	if l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 || l.Tags[0].Count != 3 {
		t.Errorf("list_tags: %+v", l)
	}
}

func TestQAListTagsCountsAndOrder(t *testing.T) {
	e, cs := qsTagEnv(t, 5)
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "few", "stable_ids": qsSids(t, e, "m00")})
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "many", "query": "invoice"})
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "mid", "stable_ids": qsSids(t, e, "m00", "m01", "m02")})
	l, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "acct"})
	var got []string
	for _, x := range l.Tags {
		got = append(got, fmt.Sprintf("%s=%d", x.Tag, x.Count))
	}
	if want := []string{"many=5", "mid=3", "few=1"}; !slices.Equal(got, want) {
		t.Errorf("list_tags %v, want %v", got, want)
	}
	if o, _ := ok[qsTags](t, cs, "list_tags", map[string]any{"account": "other"}); len(o.Tags) != 0 {
		t.Errorf("tags leaked across accounts: %+v", o)
	}
}

func TestQASearchTagReturnsOnlyTaggedIncludingFacets(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "t1", "Shop <a@shop.example>", "invoice one")
	e.add("INBOX", "t2", "Shop <a@shop.example>", "invoice two")
	e.add("INBOX", "t3", "Other <b@other.example>", "invoice three")
	e.add("INBOX", "u1", "Shop <a@shop.example>", "invoice untagged")
	e.add("INBOX", "u2", "Zed <z@zed.example>", "invoice untagged too")
	e.refresh("acct")
	cs := e.admin()
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/f", "stable_ids": qsSids(t, e, "t1", "t2", "t3")})
	s, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/f", "facets": []string{"sender", "domain"}})
	if !slices.Equal(s.ids(), qsSorted(qsSids(t, e, "t1", "t2", "t3"))) || s.Total != 3 {
		t.Errorf("hits %v total %d", s.ids(), s.Total)
	}
	counts := func(k string) map[string]int {
		m := map[string]int{}
		for _, v := range s.Facets[k] {
			m[v.Value] = v.Count
		}
		return m
	}
	dom := counts("domain")
	if len(dom) != 2 || dom["shop.example"] != 2 || dom["other.example"] != 1 {
		t.Errorf("domain facet counts untagged mail: %v", dom)
	}
	if sd := counts("sender"); len(sd) != 2 {
		t.Errorf("sender facet counts untagged mail: %v", sd)
	}
	// With text: tag AND text.
	s2, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/f", "query": "three"})
	if !slices.Equal(s2.ids(), qsSorted(qsSids(t, e, "t3"))) {
		t.Errorf("tag+text: %v", s2.ids())
	}
	// Thread grouping honours the tag too.
	s3, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/f", "group_by": "thread"})
	if s3.Total != 3 {
		t.Errorf("thread grouping with tag: %d", s3.Total)
	}
}

func TestQASavedQueryFillsOnlyEmptyInputs(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "s1", "Shop <a@shop.example>", "invoice shop")
	e.add("INBOX", "s2", "Shop <a@shop.example>", "parcel shop")
	e.add("INBOX", "x1", "Alice <alice@x.example>", "invoice alice")
	e.refresh("acct")
	cs := e.admin()
	ok[map[string]any](t, cs, "save_query", map[string]any{"account": "acct", "name": "inv.shop", "query": "invoice", "from": "shop.example", "group_by": "message"})
	ids := func(args map[string]any) []string {
		args["account"] = "acct"
		s, _ := ok[qsSearch](t, cs, "search", args)
		return s.ids()
	}
	if got := ids(map[string]any{"saved": "inv.shop"}); !slices.Equal(got, qsSorted(qsSids(t, e, "s1"))) {
		t.Errorf("saved alone: %v", got)
	}
	// query given: replaces the saved query, from still comes from the saved one.
	if got := ids(map[string]any{"saved": "inv.shop", "query": "parcel"}); !slices.Equal(got, qsSorted(qsSids(t, e, "s2"))) {
		t.Errorf("saved + query override: %v", got)
	}
	// from given: replaces the saved from, query still invoice.
	if got := ids(map[string]any{"saved": "inv.shop", "from": "alice@x"}); !slices.Equal(got, qsSorted(qsSids(t, e, "x1"))) {
		t.Errorf("saved + from override: %v", got)
	}
	// Saved name is case-insensitive like tags.
	if got := ids(map[string]any{"saved": "INV.SHOP"}); !slices.Equal(got, qsSorted(qsSids(t, e, "s1"))) {
		t.Errorf("saved upper-case: %v", got)
	}
	// Saved tag fills an empty tag, an explicit tag wins.
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/a", "stable_ids": qsSids(t, e, "s1", "x1")})
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/b", "stable_ids": qsSids(t, e, "s2")})
	ok[map[string]any](t, cs, "save_query", map[string]any{"account": "acct", "name": "tagged", "tag": "case/a", "group_by": "message"})
	if got := ids(map[string]any{"saved": "tagged"}); !slices.Equal(got, qsSorted(qsSids(t, e, "s1", "x1"))) {
		t.Errorf("saved tag: %v", got)
	}
	if got := ids(map[string]any{"saved": "tagged", "tag": "case/b"}); !slices.Equal(got, qsSorted(qsSids(t, e, "s2"))) {
		t.Errorf("explicit tag over saved tag: %v", got)
	}
	// list_saved_queries shows it.
	type lsq struct {
		Count   int `json:"count"`
		Queries []struct {
			Name string `json:"name"`
		} `json:"queries"`
	}
	if l, _ := ok[lsq](t, e.connect(Read, nil), "list_saved_queries", map[string]any{"account": "acct"}); l.Count != 2 {
		t.Errorf("list_saved_queries: %+v", l)
	}
}

func TestQATagAndSavedRejectedWithServerTrueAndSavedNeedsAccount(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "a1", "Alice <alice@x.example>", "hi")
	e.refresh("acct")
	cs := e.admin()
	ok[map[string]any](t, cs, "save_query", map[string]any{"account": "acct", "name": "s", "query": "hi"})
	e.log.reset()
	qsToolErr(t, cs, "search", map[string]any{"account": "acct", "query": "hi", "tag": "case/a", "server": true}, "")
	qsToolErr(t, cs, "search", map[string]any{"account": "acct", "query": "hi", "saved": "s", "server": true}, "")
	qsToolErr(t, cs, "search", map[string]any{"query": "hi", "saved": "s"}, "account")
	qsToolErr(t, cs, "search", map[string]any{"saved": "s"}, "account")
	qsToolErr(t, cs, "search", map[string]any{"account": "acct", "saved": "missing"}, "")
	qsToolErr(t, cs, "search", map[string]any{"account": "other", "saved": "s"}, "") // saved queries are per account
	if v := e.log.verbs(); len(v) != 0 {
		t.Errorf("rejected searches reached the mail server: %v", v)
	}
}

func TestQASearchCursorBindsTheTag(t *testing.T) {
	e := qsEnv(t)
	for i := 0; i < 4; i++ {
		e.add("INBOX", fmt.Sprintf("c%d", i), "A <a@x.example>", fmt.Sprintf("note %d", i))
	}
	e.refresh("acct")
	cs := e.admin()
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/a", "query": "note"})
	ok[qsTag](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/b", "query": "note"})
	p1, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/a", "limit": 2})
	if p1.NextCursor == "" || len(p1.Hits) != 2 {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "case/a", "limit": 2, "cursor": p1.NextCursor})
	if len(p2.Hits) != 2 || slices.Equal(p1.ids(), p2.ids()) {
		t.Errorf("page 2: %v after %v", p2.ids(), p1.ids())
	}
	qsToolErr(t, cs, "search", map[string]any{"account": "acct", "tag": "case/b", "limit": 2, "cursor": p1.NextCursor}, "cursor")
	qsToolErr(t, cs, "search", map[string]any{"account": "acct", "limit": 2, "cursor": p1.NextCursor}, "cursor")
	// Case of the tag does not change the cursor's key.
	ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "tag": "CASE/A", "limit": 2, "cursor": p1.NextCursor})
}

func TestQANoImapCommandsFromNewTools(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "a1", "Shop <no-reply@shop.example>", "Your order shipped")
	e.add("INBOX", "a2", "Alice <alice@x.example>", "lunch")
	e.refresh("acct")
	e.log.reset()
	ad := e.admin()
	rd := e.connect(Read, nil, WithHistory(e.hist), WithOrganiser(e.org))
	e.log.reset()
	ok[qsSenders](t, rd, "senders", map[string]any{"account": "acct"})
	ok[qsTags](t, rd, "list_tags", map[string]any{"account": "acct"})
	ok[map[string]any](t, rd, "list_saved_queries", map[string]any{"account": "acct"})
	set, _ := ok[map[string]any](t, ad, "set_sender_kind", map[string]any{"account": "acct", "addr": "alice@x.example", "kind": "list"})
	tg, _ := ok[qsTag](t, ad, "tag_messages", map[string]any{"account": "acct", "tag": "t", "query": "order"})
	ok[qsTag](t, ad, "tag_messages", map[string]any{"account": "acct", "tag": "t2", "stable_ids": []string{"a1"}, "dry_run": true})
	ok[map[string]any](t, ad, "save_query", map[string]any{"account": "acct", "name": "q", "tag": "t"})
	ok[qsSearch](t, ad, "search", map[string]any{"account": "acct", "saved": "q"})
	un, _ := ok[qsTag](t, ad, "untag_messages", map[string]any{"account": "acct", "tag": "t", "query": "order"})
	ok[map[string]any](t, ad, "undo", map[string]any{"history_id": un.HistoryID})
	ok[map[string]any](t, ad, "undo", map[string]any{"history_id": tg.HistoryID})
	ok[map[string]any](t, ad, "undo", map[string]any{"history_id": set["history_id"]})
	if lines := e.log.lines(); len(lines) != 0 {
		t.Errorf("local tools sent IMAP traffic: %q", lines)
	}
	e.noWrites(t)
}

// ---- safety ----

func TestQASendersLikeMetacharactersAreLiteral(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "p1", `"100% Off" <promo@x.example>`, "sale")
	e.add("INBOX", "p2", "A B <a_b@x.example>", "under")
	e.add("INBOX", "p3", "AxB <axb@x.example>", "plain")
	e.add("INBOX", "p4", `"back\\slash" <bs@x.example>`, "bs")
	e.refresh("acct")
	cs := e.connect(Read, nil)
	for q, want := range map[string][]string{
		"%":      {"promo@x.example"},
		"100%":   {"promo@x.example"},
		"_":      {"a_b@x.example"},
		"a_b":    {"a_b@x.example"},
		"\\":     {"bs@x.example"},
		"%%":     nil,
		"a%b":    nil,
		"x.exam": {"a_b@x.example", "axb@x.example", "bs@x.example", "promo@x.example"},
	} {
		if got := qsSendersOf(t, cs, map[string]any{"query": q}).addrs(); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("query %q: %v, want %v", q, got, want)
		}
	}
}

func TestQASendersOutputIsSanitised(t *testing.T) {
	e := qsEnv(t)
	e.addAt("acct", "INBOX", "ev1", "\"Evil‮ Name​\u0007\" <evil@x.example>", "hello", t0,
		"List-Id: Weekly <ne​ws.l‮ists.example>")
	e.refresh("acct")
	cs := e.connect(Read, nil)
	s := qsSendersOf(t, cs, map[string]any{"query": "evil"})
	if len(s.Senders) != 1 {
		t.Fatalf("senders: %+v", s)
	}
	r := s.Senders[0]
	if r.Name == "" || r.ListID == "" {
		t.Fatalf("name %q list_id %q: the test message was not parsed", r.Name, r.ListID)
	}
	for what, v := range map[string]string{"name": r.Name, "list_id": r.ListID, "addr": r.Addr} {
		if i := strings.IndexFunc(v, func(c rune) bool { return unicode.IsControl(c) || unicode.Is(unicode.Cf, c) }); i >= 0 {
			t.Errorf("%s %q contains a control or format character", what, v)
		}
	}
}

func TestQASendersCursorFromDifferentQueryRejected(t *testing.T) {
	e := qsEnv(t)
	for _, f := range []string{"a", "b", "c"} {
		e.add("INBOX", "m-"+f, fmt.Sprintf("%s <%s@x.example>", f, f), "hi "+f)
	}
	e.refresh("acct")
	cs := e.connect(Read, nil)
	p1 := qsSendersOf(t, cs, map[string]any{"limit": 1})
	if p1.NextCursor == "" || p1.Total != 3 {
		t.Fatalf("page 1: %+v", p1)
	}
	p2 := qsSendersOf(t, cs, map[string]any{"limit": 1, "cursor": p1.NextCursor})
	if len(p2.Senders) != 1 || p2.Senders[0].Addr == p1.Senders[0].Addr {
		t.Errorf("page 2: %+v after %+v", p2, p1)
	}
	for name, args := range map[string]map[string]any{
		"other query":   {"query": "a", "limit": 1},
		"other kind":    {"kind": "list", "limit": 1},
		"other sort":    {"sort": "last_at", "limit": 1},
		"never_replied": {"never_replied": true, "limit": 1},
		"min_msgs":      {"min_msgs": 1, "limit": 1},
		"since":         {"since": "2020-01-01", "limit": 1},
		"other account": {"account": "other", "limit": 1},
		"garbage":       {"limit": 1},
	} {
		args["account"] = qsStr(args["account"], "acct")
		args["cursor"] = p1.NextCursor
		if name == "garbage" {
			args["cursor"] = "not-a-cursor"
		}
		if !call(t, cs, "senders", args).IsError {
			t.Errorf("%s: cursor accepted", name)
		}
	}
}

func qsStr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func TestQASaveQueryCapsThroughTool(t *testing.T) {
	e := qsEnv(t)
	e.add("INBOX", "a1", "Alice <alice@x.example>", "hi")
	e.refresh("acct")
	cs := e.admin()
	// Note: 500 characters pass, 501 do not.
	ok[map[string]any](t, cs, "save_query", map[string]any{"account": "acct", "name": "n500", "query": "x", "note": strings.Repeat("n", 500)})
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "n501", "query": "x", "note": strings.Repeat("n", 501)}, "")
	// The JSON cap of 4KB: a folder name is not otherwise bounded.
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "bigfolder", "query": "x", "folder": strings.Repeat("f", 5000)}, "too large")
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "bigquery", "query": strings.Repeat("q", 600)}, "")
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "Bad Name", "query": "x"}, "")
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "g", "query": "x", "group_by": "bogus"}, "")
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "d", "query": "x", "since": "yesterday"}, "")
	type lsq struct {
		Queries []struct {
			Name string `json:"name"`
		} `json:"queries"`
	}
	l, _ := ok[lsq](t, cs, "list_saved_queries", map[string]any{"account": "acct"})
	if len(l.Queries) != 1 || l.Queries[0].Name != "n500" {
		t.Errorf("rejected queries were stored: %+v", l)
	}
}
