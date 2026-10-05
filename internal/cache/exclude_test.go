package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// exclAdd stores a message from `from` in account A whose body says "ledger".
func exclAdd(t *testing.T, c *Cache, id, from string, when time.Time, hdr ...string) {
	t.Helper()
	thrAdd(t, c, "A", id, "", thrMsg(id+"@x", from, "ledger "+id, "ledger body", when, hdr...), when)
}

func exclSearch(t *testing.T, c *Cache, group string, exFrom, exKind []string, facets ...string) SearchResult {
	t.Helper()
	res, err := c.SearchV2(context.Background(), SearchOptions{
		SearchQuery: SearchQuery{Text: "ledger", ExcludeFrom: exFrom, ExcludeKind: exKind, Limit: 100},
		GroupBy:     group, Facets: facets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func exclIDs(res SearchResult) []string {
	var out []string
	for _, m := range res.Messages {
		out = append(out, m.StableID)
	}
	slices.Sort(out)
	return out
}

func TestExcludeFromIsLiteralAndCaseInsensitive(t *testing.T) {
	c := thrOpen(t)
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	exclAdd(t, c, "u", "A B <a_b@x.example>", t0)
	exclAdd(t, c, "x", "A X <axb@x.example>", t0.Add(time.Hour))
	exclAdd(t, c, "p", "Pct <100%@y.example>", t0.Add(2*time.Hour))
	exclAdd(t, c, "o", "Other <other@y.example>", t0.Add(3*time.Hour))
	for _, tc := range []struct {
		name string
		ex   []string
		want []string
	}{
		{"none", nil, []string{"o", "p", "u", "x"}},
		{"underscore is literal, case folded", []string{"A_B@X"}, []string{"o", "p", "x"}},
		{"percent is literal", []string{"%"}, []string{"o", "u", "x"}},
		{"percent inside", []string{"100%@"}, []string{"o", "u", "x"}},
		{"any of several", []string{"axb", "OTHER@"}, []string{"p", "u"}},
		{"substring of the domain", []string{"@y.example"}, []string{"u", "x"}},
		{"no match drops nothing", []string{"nobody"}, []string{"o", "p", "u", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := exclSearch(t, c, "message", tc.ex, nil)
			if got := exclIDs(res); !slices.Equal(got, tc.want) || res.Total != len(tc.want) {
				t.Errorf("got %v (total %d), want %v", got, res.Total, tc.want)
			}
		})
	}
}

func TestExcludeLimitsAndValidation(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprint("s", i)
	}
	for name, o := range map[string]SearchQuery{
		"too many":     {Text: "x", ExcludeFrom: many},
		"too long":     {Text: "x", ExcludeFrom: []string{strings.Repeat("a", 321)}},
		"empty":        {Text: "x", ExcludeFrom: []string{"  "}},
		"unknown kind": {Text: "x", ExcludeKind: []string{"human", "spam"}},
	} {
		_, err := c.SearchV2(ctx, SearchOptions{SearchQuery: o})
		if !errors.Is(err, ErrQueryLimit) {
			t.Errorf("%s: err %v, want ErrQueryLimit", name, err)
		} else if strings.Contains(err.Error(), "spam") {
			t.Errorf("%s: error echoes the input: %v", name, err)
		}
	}
	// 20 entries of exactly 320 bytes are accepted.
	ok := slices.Repeat([]string{strings.Repeat("a", 320)}, 20)
	if _, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "x", ExcludeFrom: ok}}); err != nil {
		t.Errorf("at the limits: %v", err)
	}
	if ExclusionKey([]string{"B", "a", "A"}, []string{"list"}) != ExclusionKey([]string{" a", "b"}, []string{"list"}) {
		t.Error("ExclusionKey is not canonical")
	}
	if ExclusionKey([]string{"a"}, nil) == ExclusionKey([]string{"b"}, nil) || ExclusionKey(nil, []string{"list"}) == ExclusionKey(nil, []string{"human"}) || ExclusionKey(nil, nil) != "" {
		t.Error("ExclusionKey does not tell exclusions apart")
	}
}

func TestExcludeKindUsesTheSendersTable(t *testing.T) {
	c := sndFixture(t)
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	search := func(group string, kinds []string, facets ...string) SearchResult {
		res, err := c.SearchV2(context.Background(), SearchOptions{
			SearchQuery: SearchQuery{Text: "body", ExcludeKind: kinds, Limit: 100}, GroupBy: group, Facets: facets,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	froms := func(res SearchResult) []string {
		var out []string
		for _, m := range res.Messages {
			out = append(out, BareAddr(m.From))
		}
		slices.Sort(out)
		return out
	}
	all := search("message", nil)
	if all.Total != 9 {
		t.Fatalf("fixture: %d messages", all.Total)
	}
	res := search("message", []string{KindList, KindNotification, KindTransactional})
	if want := []string{"alice@x.example", "alice@x.example", "mallory@evil.example", "me@home.test", "me@home.test", "me@home.test"}; !slices.Equal(froms(res), want) || res.Total != 6 {
		t.Errorf("humans only: %v (total %d)", froms(res), res.Total)
	}
	// Totals and facets follow the exclusion.
	res = search("message", []string{KindHuman}, "sender", "domain")
	if res.Total != 3 {
		t.Errorf("total %d, want 3 (list, github, shop)", res.Total)
	}
	var senders []string
	for _, f := range res.Facets["sender"] {
		senders = append(senders, f.Value)
	}
	slices.Sort(senders)
	if want := []string{"news@lists.example", "no-reply@shop.example", "notifications@github.com"}; !slices.Equal(senders, want) {
		t.Errorf("sender facet %v, want %v", senders, want)
	}
	// A sender the table does not hold (yet) is not excluded.
	if _, err := c.db.Exec(`DELETE FROM senders WHERE addr = 'alice@x.example'`); err != nil {
		t.Fatal(err)
	}
	res = search("message", []string{KindHuman})
	if n := slices.Index(froms(res), "alice@x.example"); n < 0 || res.Total != 5 {
		t.Errorf("unknown sender excluded: %v (total %d)", froms(res), res.Total)
	}
	// The owner's decision counts like a rule's.
	if _, err := c.SetSenderKind(context.Background(), "acc", "no-reply@shop.example", KindList); err != nil {
		t.Fatal(err)
	}
	if got := search("message", []string{KindTransactional}).Total; got != 9 {
		t.Errorf("shop re-kinded to list, still excluded as transactional: total %d", got)
	}
}

func TestExcludeInThreadModeDropsAThreadOnlyWhenAllItsMatchesAreExcluded(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	re := func(root string) []string {
		return []string{"References: <" + root + "@x>", "In-Reply-To: <" + root + "@x>"}
	}
	exclAdd(t, c, "t1a", "Alice <alice@x.example>", t0)
	exclAdd(t, c, "t1b", "Bob <bob@x.example>", t0.Add(time.Hour), re("t1a")...)
	exclAdd(t, c, "t2a", "Alice <alice@x.example>", t0.Add(2*time.Hour))
	exclAdd(t, c, "t3a", "Bob <bob@x.example>", t0.Add(3*time.Hour))
	c.threadNew(ctx, "A", []string{"t1a", "t1b", "t2a", "t3a"})
	if thrTid(t, c, "A", "t1a") != thrTid(t, c, "A", "t1b") {
		t.Fatal("fixture: t1a and t1b are not one thread")
	}

	res := exclSearch(t, c, "thread", nil, nil, "sender")
	if res.Total != 3 || len(res.Threads) != 3 {
		t.Fatalf("no exclusion: total %d, %d threads", res.Total, len(res.Threads))
	}
	res = exclSearch(t, c, "thread", []string{"alice@"}, nil, "sender")
	if res.Total != 2 || len(res.Threads) != 2 {
		t.Fatalf("total %d, %d threads, want 2 (the Alice-only thread is gone)", res.Total, len(res.Threads))
	}
	for _, th := range res.Threads {
		if th.Matched != 1 {
			t.Errorf("thread %s matched %d, want 1 (counts reflect the remaining messages)", th.TopStableID, th.Matched)
		}
		if th.TopStableID != "t1b" && th.TopStableID != "t3a" {
			t.Errorf("top %s is an excluded message", th.TopStableID)
		}
		if th.Subject == "" || th.NMsgs < 1 {
			t.Errorf("thread info lost: %+v", th)
		}
	}
	if f := res.Facets["sender"]; len(f) != 1 || f[0].Value != "bob@x.example" || f[0].Count != 2 {
		t.Errorf("sender facet %+v, want bob 2", f)
	}
	// Excluding both senders leaves nothing.
	if res = exclSearch(t, c, "thread", []string{"alice@", "bob@"}, nil); res.Total != 0 || len(res.Threads) != 0 {
		t.Errorf("everything excluded: %+v", res)
	}
	// Message mode counts messages.
	if res = exclSearch(t, c, "message", []string{"alice@"}, nil); res.Total != 2 {
		t.Errorf("message mode total %d, want 2", res.Total)
	}
}
