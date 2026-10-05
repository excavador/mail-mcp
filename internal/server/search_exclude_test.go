package server

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// excEnv has two owner-less humans, a shop, GitHub and a list in INBOX, every
// body starting "body".
func excEnv(t *testing.T) *wenv {
	t.Helper()
	e := qsEnv(t)
	for i := 0; i < 3; i++ {
		e.add("INBOX", fmt.Sprintf("al%d", i), "Alice <alice@x.example>", fmt.Sprintf("lunch %d", i))
	}
	e.add("INBOX", "bo0", "Bob <bob@y.example>", "dinner")
	e.add("INBOX", "shop0", "Shop <no-reply@shop.example>", "Your order 42 has shipped")
	e.add("INBOX", "gh0", "GitHub <notifications@github.com>", "[r] PR")
	e.addAt("acct", "INBOX", "list0", "News <news@lists.example>", "weekly", t0, "List-Id: <weekly.lists.example>")
	e.refresh("acct")
	return e
}

func facetValues(s qsSearch, name string) map[string]int {
	out := map[string]int{}
	for _, f := range s.Facets[name] {
		out[f.Value] = f.Count
	}
	return out
}

func TestSearchExcludeFromAndKindOverTheTool(t *testing.T) {
	e := excEnv(t)
	cs := e.admin()
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{"account": "acct", "query": "body", "group_by": "message", "facets": []string{"sender"}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	all, _ := ok[qsSearch](t, cs, "search", base(nil))
	if all.Total != 7 {
		t.Fatalf("fixture: %d messages", all.Total)
	}
	// exclude_from: totals, hits and facets agree.
	r, _ := ok[qsSearch](t, cs, "search", base(map[string]any{"exclude_from": []string{"ALICE@"}}))
	if r.Total != 4 || len(r.Hits) != 4 || facetValues(r, "sender")["alice@x.example"] != 0 || facetValues(r, "sender")["bob@y.example"] != 1 {
		t.Errorf("exclude_from: total %d hits %d facets %v", r.Total, len(r.Hits), facetValues(r, "sender"))
	}
	// exclude_kind: everything the rules call automated goes.
	r, _ = ok[qsSearch](t, cs, "search", base(map[string]any{"exclude_kind": []string{"list", "transactional", "notification"}}))
	if r.Total != 4 || !slices.Equal(slices.Sorted(func(y func(string) bool) {
		for a := range facetValues(r, "sender") {
			y(a)
		}
	}), []string{"alice@x.example", "bob@y.example"}) {
		t.Errorf("exclude_kind: total %d facets %v", r.Total, facetValues(r, "sender"))
	}
	// Thread mode: the same exclusion, counted in threads.
	th, _ := ok[qsSearch](t, cs, "search", base(map[string]any{"group_by": "thread", "exclude_from": []string{"alice@", "shop"}}))
	if th.Total != 3 {
		t.Errorf("thread mode total %d, want 3 (bob, github, list)", th.Total)
	}
	// Bad input is refused with fixed texts.
	qsToolErr(t, cs, "search", base(map[string]any{"exclude_kind": []string{"spam"}}), "exclude_kind must list only")
	qsToolErr(t, cs, "search", base(map[string]any{"exclude_from": []string{""}}), "empty")
	long := make([]string, 21)
	for i := range long {
		long[i] = "x"
	}
	qsToolErr(t, cs, "search", base(map[string]any{"exclude_from": long}), "more than 20")
}

func TestSearchExcludeIsRefusedWithServerTrue(t *testing.T) {
	e := excEnv(t)
	cs := e.admin()
	for _, args := range []map[string]any{
		{"exclude_from": []string{"a"}},
		{"exclude_kind": []string{"list"}},
	} {
		args["account"], args["query"], args["server"] = "acct", "hi", true
		qsToolErr(t, cs, "search", args, "server=true")
	}
}

func TestSearchCursorBindsTheExclusions(t *testing.T) {
	e := excEnv(t)
	cs := e.admin()
	args := func(from ...string) map[string]any {
		return map[string]any{"account": "acct", "query": "body", "group_by": "message", "limit": 2, "exclude_from": from}
	}
	p1, _ := ok[qsSearch](t, cs, "search", args("shop", "gh"))
	if p1.NextCursor == "" || len(p1.Hits) != 2 {
		t.Fatalf("page 1: %+v", p1)
	}
	next := func(a map[string]any) map[string]any { a["cursor"] = p1.NextCursor; return a }
	p2, _ := ok[qsSearch](t, cs, "search", next(args("shop", "gh")))
	if len(p2.Hits) != 2 || slices.Equal(p1.ids(), p2.ids()) {
		t.Errorf("page 2: %v after %v", p2.ids(), p1.ids())
	}
	// Order, case and spacing do not change the key.
	ok[qsSearch](t, cs, "search", next(args("GH", " shop")))
	qsToolErr(t, cs, "search", next(args("shop")), "cursor")
	qsToolErr(t, cs, "search", next(args("shop", "gh", "news")), "cursor")
	qsToolErr(t, cs, "search", next(args()), "cursor")
	// And a cursor from a search without exclusions does not fit one with.
	q1, _ := ok[qsSearch](t, cs, "search", args())
	a := args("shop")
	a["cursor"] = q1.NextCursor
	qsToolErr(t, cs, "search", a, "cursor")
	// exclude_kind binds too.
	k := args()
	k["exclude_kind"] = []string{"list"}
	k["cursor"] = q1.NextCursor
	qsToolErr(t, cs, "search", k, "cursor")
}

func TestSavedQueryStoresExclusions(t *testing.T) {
	e := excEnv(t)
	cs := e.admin()
	ok[map[string]any](t, cs, "save_query", map[string]any{"account": "acct", "name": "people", "query": "body", "group_by": "message",
		"exclude_kind": []string{"list", "transactional", "notification"}, "exclude_from": []string{"bob@"}})
	r, _ := ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "saved": "people"})
	if r.Total != 3 {
		t.Errorf("saved search total %d, want 3 (Alice only)", r.Total)
	}
	// A given input replaces the saved one.
	r, _ = ok[qsSearch](t, cs, "search", map[string]any{"account": "acct", "saved": "people", "exclude_from": []string{"zzz"}})
	if r.Total != 4 {
		t.Errorf("override total %d, want 4 (kinds still saved, bob back)", r.Total)
	}
	qsToolErr(t, cs, "save_query", map[string]any{"account": "acct", "name": "bad", "exclude_kind": []string{"spam"}}, "exclude_kind")
	_, raw := ok[map[string]any](t, cs, "list_saved_queries", map[string]any{"account": "acct"})
	if !strings.Contains(raw, `"exclude_from":["bob@"]`) || !strings.Contains(raw, `"exclude_kind":["list","transactional","notification"]`) {
		t.Errorf("list_saved_queries does not show the exclusions: %s", raw)
	}
}
