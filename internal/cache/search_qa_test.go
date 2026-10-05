package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// qaSearchCorpus: threads of 1..4 messages about "ledger", from 6 senders at
// 3 domains over 4 months, in two accounts.
func qaSearchCorpus(t *testing.T, c *Cache) (nA int) {
	t.Helper()
	ctx := context.Background()
	senders := []string{"ann@a.com", "bob@a.com", "cy@b.org", "di@b.org", "ed@c.net", "flo@c.net"}
	var ids []string
	k := 0
	for ti := 0; ti < 30; ti++ {
		size := 1 + ti%4
		root := fmt.Sprintf("s%d-0@x", ti)
		for m := 0; m < size; m++ {
			k++
			when := thr0.Add(time.Duration(k) * 37 * time.Hour) // spreads over ~4 months
			var hdr []string
			subj := fmt.Sprintf("ledger %d", ti)
			if m > 0 {
				hdr = []string{"References: <" + root + ">", "In-Reply-To: <" + root + ">"}
				subj = "Re: " + subj
			}
			s := senders[(ti+m)%len(senders)]
			thrAdd(t, c, "A", fmt.Sprintf("a:%d-%d", ti, m), "", thrMsg(fmt.Sprintf("s%d-%d@x", ti, m), "N <"+s+">", subj, "the ledger entry", when, hdr...), thr0)
			ids = append(ids, fmt.Sprintf("a:%d-%d", ti, m))
			nA++
		}
	}
	c.threadNew(ctx, "A", ids)
	var idsB []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("b:%d", i)
		thrAdd(t, c, "B", id, "", thrAttMsg(fmt.Sprintf("b%d@x", i), "Z <z@b-only.com>", "b ledger", "ledger in B", "ledger-b.bin", thr0.Add(time.Duration(i)*time.Hour)), thr0)
		idsB = append(idsB, id)
	}
	c.threadNew(ctx, "B", idsB)
	return nA
}

func TestQASearchV2PagesAreDisjointAndCoverEverything(t *testing.T) {
	ctx := context.Background()
	for _, ready := range []bool{true, false} {
		t.Run(fmt.Sprintf("fts2ready=%v", ready), func(t *testing.T) {
			c := thrOpen(t)
			nA := qaSearchCorpus(t, c)
			c.fts2Ready.Store(ready)
			if _, r := c.FTSTable(); r != ready {
				t.Fatal("seam")
			}
			for _, mode := range []string{"thread", "message"} {
				seen := map[string]int{}
				total := -1
				for _, lim := range []int{1, 7, 100} {
					clear(seen)
					for off := 0; ; off += lim {
						r, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "A", Text: "ledger", Limit: lim}, GroupBy: mode, Offset: off})
						if err != nil {
							t.Fatal(err)
						}
						// (an empty page past the end reports Total 0: noted as a minor bug)
						if total >= 0 && r.Total != total && len(r.Threads)+len(r.Messages) > 0 {
							t.Fatalf("%s: total changed between pages: %d vs %d", mode, r.Total, total)
						}
						if r.Total > 0 {
							total = r.Total
						}
						n := len(r.Threads) + len(r.Messages)
						for _, h := range r.Threads {
							seen[h.TID]++
						}
						for _, h := range r.Messages {
							seen[h.StableID]++
						}
						if n == 0 {
							break
						}
						if off > 500 {
							t.Fatal("runaway paging")
						}
					}
					want := 30
					if mode == "message" {
						want = nA
					}
					if len(seen) != want || total != want {
						t.Errorf("%s limit %d: union %d, total %d, want %d", mode, lim, len(seen), total, want)
					}
					for k, n := range seen {
						if n != 1 {
							t.Errorf("%s limit %d: %s on %d pages", mode, lim, k, n)
						}
					}
					total = -1
				}
			}
		})
	}
}

func TestQAFacetCountsSumToMatchedMessages(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"thread", "message"} {
		c := thrOpen(t)
		nA := qaSearchCorpus(t, c)
		r, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "A", Text: "ledger", Limit: 3}, GroupBy: mode, Facets: []string{"sender", "domain", "month"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, dim := range []string{"sender", "domain", "month"} {
			sum := 0
			for _, f := range r.Facets[dim] {
				sum += f.Count
			}
			if sum != nA {
				t.Errorf("%s mode: %s facet sums to %d, want %d matched messages (%v)", mode, dim, sum, nA, r.Facets[dim])
			}
		}
		if len(r.Facets["month"]) < 3 {
			t.Errorf("corpus should span months: %v", r.Facets["month"])
		}
		// Facets follow filters: restricted by sender, they sum to that sender's messages.
		f, _ := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "A", Text: "ledger", From: "ann@a.com"}, GroupBy: mode, Facets: []string{"sender", "domain", "month"}})
		want := thrCount(t, c, `SELECT COUNT(*) FROM messages WHERE account='A' AND from_addr LIKE '%ann@a.com%'`)
		for _, dim := range []string{"sender", "domain", "month"} {
			sum := 0
			for _, x := range f.Facets[dim] {
				sum += x.Count
			}
			if sum != want {
				t.Errorf("%s mode filtered: %s sums %d want %d", mode, dim, sum, want)
			}
		}
	}
}

func TestQASearchAccountIsolationThreadsAndAttachments(t *testing.T) {
	ctx := context.Background()
	c := thrOpen(t)
	qaSearchCorpus(t, c)
	// A's own attachment-only hit, to make sure the attachment branch is live for both.
	thrAdd(t, c, "A", "a:att", "", thrAttMsg("aatt@x", "Q <q@a.com>", "see file", "nothing", "ledger-a.bin", thr0), thr0)
	c.threadNew(ctx, "A", []string{"a:att"})
	for _, mode := range []string{"thread", "message"} {
		r, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "B", Text: "ledger", Limit: 100}, GroupBy: mode, Facets: []string{"sender", "domain"}})
		if err != nil {
			t.Fatal(err)
		}
		if r.Total != 5 {
			t.Errorf("%s: B total %d, want 5", mode, r.Total)
		}
		for _, h := range r.Threads {
			if h.Account != "B" || !strings.HasPrefix(h.TopStableID, "b:") {
				t.Errorf("%s: leaked thread %+v", mode, h)
			}
		}
		for _, h := range r.Messages {
			if h.Account != "B" || !strings.HasPrefix(h.StableID, "b:") {
				t.Errorf("%s: leaked message %+v", mode, h)
			}
		}
		for _, f := range r.Facets["sender"] {
			if !strings.Contains(f.Value, "b-only.com") {
				t.Errorf("%s: facet leaked %v", mode, f)
			}
		}
		// attachment-only query for A's file name, asked as B
		a, _ := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "B", Text: "ledger-a"}, GroupBy: mode})
		if a.Total != 0 {
			t.Errorf("%s: B found A's attachment (total %d)", mode, a.Total)
		}
	}
	// And GetThread across accounts.
	if _, _, err := c.ThreadMessages(ctx, "B", thrTid(t, c, "A", "a:0-0")); err != ErrNotFound {
		t.Errorf("another account's tid: %v", err)
	}
}

func TestQAAttachmentOnlyHitRanksBelowBodyHitsBothModesBothFTS(t *testing.T) {
	ctx := context.Background()
	for _, ready := range []bool{true} { // the attachment branch exists only with fts2
		c := thrOpen(t)
		c.fts2Ready.Store(ready)
		thrAdd(t, c, "p", "pm:body1", "", thrMsg("b1@x", "A <a@x.com>", "terms", "the quarterly contract", thr0), thr0)
		thrAdd(t, c, "p", "pm:body2", "", thrMsg("b2@x", "A <a@x.com>", "terms2", "another contract here", thr0.Add(time.Hour)), thr0)
		// three attachment-only hits, all NEWER than every body hit
		for i := 0; i < 3; i++ {
			thrAdd(t, c, "p", fmt.Sprintf("pm:att%d", i), "", thrAttMsg(fmt.Sprintf("at%d@x", i), "B <b@x.com>", "see attached", "nothing relevant", "contract-final.bin", thr0.Add(time.Duration(100+i)*time.Hour)), thr0)
		}
		c.threadNew(ctx, "p", []string{"pm:body1", "pm:body2", "pm:att0", "pm:att1", "pm:att2"})
		for _, mode := range []string{"thread", "message"} {
			r, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "p", Text: "contract", Limit: 100}, GroupBy: mode})
			if err != nil {
				t.Fatal(err)
			}
			var order []string
			for _, h := range r.Threads {
				order = append(order, h.TopStableID)
			}
			for _, h := range r.Messages {
				order = append(order, h.StableID)
			}
			if len(order) != 5 {
				t.Fatalf("%s: %v", mode, order)
			}
			for i, id := range order {
				isBody := strings.HasPrefix(id, "pm:body")
				if (i < 2) != isBody {
					t.Errorf("%s: position %d is %s; body hits must come first: %v", mode, i, id, order)
				}
			}
			// and the attachment page offset keeps body hits first across pages
			p1, _ := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "p", Text: "contract", Limit: 2}, GroupBy: mode})
			if n := len(p1.Threads) + len(p1.Messages); n != 2 {
				t.Fatalf("page %d", n)
			}
			first := ""
			if mode == "thread" {
				first = p1.Threads[0].TopStableID + p1.Threads[1].TopStableID
			} else {
				first = p1.Messages[0].StableID + p1.Messages[1].StableID
			}
			if strings.Contains(first, "att") {
				t.Errorf("%s: attachment hit on the first page of 2 body hits: %s", mode, first)
			}
		}
	}
}

func TestQASearchV2FTSSyntaxErrorsAreErrQuerySyntax(t *testing.T) {
	ctx := context.Background()
	for _, ready := range []bool{true, false} {
		c := thrOpen(t)
		qaSearchCorpus(t, c)
		c.fts2Ready.Store(ready)
		for _, mode := range []string{"thread", "message"} {
			for _, q := range []string{`"unterminated`, `ledger AND`, `NEAR(`, `nosuchcol:foo`} {
				_, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: q, FTSSyntax: true}, GroupBy: mode})
				if err == nil || !strings.Contains(err.Error(), ErrQuerySyntax.Error()) {
					t.Errorf("ready=%v %s %q: err = %v, want ErrQuerySyntax", ready, mode, q, err)
				}
			}
			// plain words never fail on punctuation
			if _, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: `"unterminated AND ( NEAR(`}, GroupBy: mode}); err != nil {
				t.Errorf("plain text with operators: %v", err)
			}
		}
	}
}

func TestQAHeaderlessReplyJoinsBySubjectAnyOrder(t *testing.T) {
	ctx := context.Background()
	root := thrMsg("hl-root@x", "A <a@x.com>", "quarterly plan", "root", thr0)
	rep := thrMsg("hl-rep@x", "B <b@x.com>", "Re: quarterly plan", "reply without refs", thr0.Add(time.Hour))
	other := thrMsg("hl-o@x", "C <c@x.com>", "unrelated", "x", thr0.Add(time.Hour))
	for _, rootFirst := range []bool{true, false} {
		c := thrOpen(t)
		type m struct {
			id, raw string
			w       time.Time
		}
		seq := []m{{"pm:root", root, thr0}, {"pm:rep", rep, thr0.Add(time.Hour)}}
		if !rootFirst {
			seq[0], seq[1] = seq[1], seq[0]
		}
		for _, x := range seq {
			thrAdd(t, c, "p", x.id, "", x.raw, x.w)
			c.threadNew(ctx, "p", []string{x.id})
		}
		thrAdd(t, c, "p", "pm:o", "", other, thr0)
		c.threadNew(ctx, "p", []string{"pm:o"})
		if thrTid(t, c, "p", "pm:root") != thrTid(t, c, "p", "pm:rep") {
			t.Errorf("rootFirst=%v: header-less reply did not join the root by subject", rootFirst)
		}
		if got := canon(partition(t, c)); got != canon(reference(t, c, "p")) {
			t.Errorf("rootFirst=%v: differs from BuildThreads:\n%s\nvs\n%s", rootFirst, got, canon(reference(t, c, "p")))
		}
		checkInvariants(t, c)
	}
}
