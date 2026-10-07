package cache

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

var thr0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// thrAdd stores one message the way a refresh does (blob, index row, FTS row,
// id index), without threading it: tests thread explicitly.
func thrAdd(t *testing.T, c *Cache, account, id, gm string, raw string, when time.Time) {
	t.Helper()
	b := []byte(raw)
	sum, err := c.putBlob(b)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	info := headerInfo{stableID: id, gmThreadID: gm, size: int64(len(b)), internal: when}
	if err := insertMessageTx(context.Background(), tx, account, info, sum, parseMessage(b)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func thrMsg(mid, from, subj, body string, when time.Time, hdr ...string) string {
	h := fmt.Sprintf("From: %s\r\nTo: me@example.com\r\nSubject: %s\r\nDate: %s\r\n", from, subj, when.Format(time.RFC1123Z))
	if mid != "" {
		h += "Message-ID: <" + mid + ">\r\n"
	}
	for _, x := range hdr {
		h += x + "\r\n"
	}
	return h + "\r\n" + body + "\r\n"
}

func thrOpen(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func thrTid(t *testing.T, c *Cache, account, id string) string {
	t.Helper()
	var tid string
	if err := c.db.QueryRow(`SELECT tid FROM message_thread WHERE account = ? AND stable_id = ?`, account, id).Scan(&tid); err != nil {
		t.Fatalf("tid of %s: %v", id, err)
	}
	return tid
}

func thrCount(t *testing.T, c *Cache, q string, args ...any) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestThreadNewIsIncrementalLateParentRelinks(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	// The reply and the grandchild arrive first, the parent last.
	thrAdd(t, c, "p", "pm:c", "", thrMsg("c@x", "Carol <c@x.com>", "Re: plan", "third", thr0.Add(3*time.Hour), "References: <a@x> <b@x>", "In-Reply-To: <b@x>"), thr0)
	c.threadNew(ctx, "p", []string{"pm:c"})
	thrAdd(t, c, "p", "pm:a", "", thrMsg("a@x", "Alice <a@x.com>", "plan", "first", thr0), thr0)
	c.threadNew(ctx, "p", []string{"pm:a"})
	if thrTid(t, c, "p", "pm:a") != thrTid(t, c, "p", "pm:c") {
		t.Fatalf("a and c must share a thread (c names a in References)")
	}
	tid := thrTid(t, c, "p", "pm:a")
	thrAdd(t, c, "p", "pm:b", "", thrMsg("b@x", "Bob <b@x.com>", "Re: plan", "second", thr0.Add(time.Hour), "References: <a@x>", "In-Reply-To: <a@x>"), thr0)
	c.threadNew(ctx, "p", []string{"pm:b"})
	if got := thrTid(t, c, "p", "pm:b"); got != tid {
		t.Errorf("b joined %s, want %s", got, tid)
	}
	var parent string
	var depth int
	_ = c.db.QueryRow(`SELECT parent_stable_id, depth FROM message_thread WHERE account='p' AND stable_id='pm:c'`).Scan(&parent, &depth)
	if parent != "pm:b" || depth != 2 {
		t.Errorf("c: parent %q depth %d, want pm:b 2 (re-linked under the late parent)", parent, depth)
	}
	var n int
	var people string
	_ = c.db.QueryRow(`SELECT n_msgs, participants_json FROM threads WHERE account='p' AND tid=?`, tid).Scan(&n, &people)
	if n != 3 || !strings.Contains(people, "Alice") || !strings.Contains(people, "Carol") {
		t.Errorf("thread row: n=%d people=%s", n, people)
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM threads`); got != 1 {
		t.Errorf("%d thread rows, want 1 (no orphan row after the merge)", got)
	}
}

func TestThreadMergeRemovesStaleThreadRows(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	// Two separate threads that a later message joins.
	thrAdd(t, c, "p", "pm:1", "", thrMsg("r1@x", "A <a@x.com>", "Re: q", "x", thr0, "References: <root@x> <m1@x>"), thr0)
	thrAdd(t, c, "p", "pm:2", "", thrMsg("r2@x", "B <b@x.com>", "Re: q", "y", thr0.Add(time.Hour), "References: <other@x> <m2@x>"), thr0)
	c.threadNew(ctx, "p", []string{"pm:1", "pm:2"})
	if thrTid(t, c, "p", "pm:1") == thrTid(t, c, "p", "pm:2") {
		t.Fatal("must start as two threads")
	}
	thrAdd(t, c, "p", "pm:3", "", thrMsg("j@x", "C <c@x.com>", "Re: q", "z", thr0.Add(2*time.Hour), "References: <root@x> <other@x>"), thr0)
	c.threadNew(ctx, "p", []string{"pm:3"})
	if thrTid(t, c, "p", "pm:1") != thrTid(t, c, "p", "pm:2") {
		t.Errorf("a message naming both roots must merge the threads")
	}
	if n := thrCount(t, c, `SELECT COUNT(*) FROM threads`); n != 1 {
		t.Errorf("%d thread rows after merge, want 1", n)
	}
	if n := thrCount(t, c, `SELECT n_msgs FROM threads`); n != 3 {
		t.Errorf("n_msgs = %d", n)
	}
}

func TestGmailThreadsUseGmThreadID(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	thrAdd(t, c, "g", "gm:1", "777", thrMsg("a@x", "A <a@x.com>", "hi", "one", thr0), thr0)
	thrAdd(t, c, "g", "gm:2", "777", thrMsg("b@x", "B <b@x.com>", "unrelated subject", "two", thr0.Add(time.Hour)), thr0)
	thrAdd(t, c, "g", "gm:3", "", thrMsg("c@x", "C <c@x.com>", "no thrid", "three", thr0), thr0)
	c.threadNew(ctx, "g", []string{"gm:1", "gm:2", "gm:3"})
	if thrTid(t, c, "g", "gm:1") != "g:777" || thrTid(t, c, "g", "gm:2") != "g:777" {
		t.Errorf("gmail tids: %s %s", thrTid(t, c, "g", "gm:1"), thrTid(t, c, "g", "gm:2"))
	}
	if got := thrTid(t, c, "g", "gm:3"); !strings.HasPrefix(got, "j:") {
		t.Errorf("gmail message without a thread id must use JWZ, got %s", got)
	}
}

func TestBackfillFillsHeadersThreadsAndResumes(t *testing.T) {
	backfillPause = 0
	c := thrOpen(t)
	ctx := context.Background()
	const n = 1300 // more than two batches
	for i := 0; i < n/2; i++ {
		root := fmt.Sprintf("root%d@x", i)
		thrAdd(t, c, "p", fmt.Sprintf("pm:r%d", i), "", thrMsg(root, "A <a@x.com>", fmt.Sprintf("topic %d", i), "q", thr0.Add(time.Duration(i)*time.Minute)), thr0)
		thrAdd(t, c, "p", fmt.Sprintf("pm:s%d", i), "", thrMsg(fmt.Sprintf("re%d@x", i), "B <b@x.com>", fmt.Sprintf("Re: topic %d", i), "a", thr0.Add(time.Duration(i)*time.Minute+time.Second),
			"References: <"+root+">", "In-Reply-To: <"+root+">"), thr0)
	}
	// Make them look like rows cached before threads existed.
	if _, err := c.db.Exec(`UPDATE messages SET message_id = NULL, in_reply_to = NULL, references_json = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`DELETE FROM message_ref`); err != nil {
		t.Fatal(err)
	}
	// One batch, then "restart".
	last, finished, err := func() (int64, bool, error) { _, l, f, err := c.backfillThreadBatch(ctx, 0); return l, f, err }()
	if err != nil || finished || last == 0 {
		t.Fatalf("first batch: last=%d finished=%v err=%v", last, finished, err)
	}
	if st, _, _ := c.backfillState(ctx, threadBackfillName); st != last {
		t.Errorf("progress not persisted: %d vs %d", st, last)
	}
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, done, _ := c.backfillState(ctx, threadBackfillName); !done {
		t.Error("not marked done")
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread`); got != n {
		t.Errorf("%d threaded, want %d", got, n)
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM threads`); got != n/2 {
		t.Errorf("%d threads, want %d (each reply joins its root)", got, n/2)
	}
	if thrTid(t, c, "p", "pm:r7") != thrTid(t, c, "p", "pm:s7") {
		t.Error("reply not in its root's thread")
	}
	// Done: a second run does nothing; a new unthreaded message re-arms it.
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	thrAdd(t, c, "p", "pm:late", "", thrMsg("late@x", "Z <z@x.com>", "late", "x", thr0), thr0)
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread WHERE stable_id = 'pm:late'`); got != 1 {
		t.Error("unthreaded message not picked up after done")
	}
}

func TestBackfillStopsOnCancel(t *testing.T) {
	backfillPause = 0
	c := thrOpen(t)
	for i := 0; i < 5; i++ {
		thrAdd(t, c, "p", fmt.Sprintf("pm:%d", i), "", thrMsg(fmt.Sprintf("m%d@x", i), "A <a@x.com>", "s", "b", thr0), thr0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.BackfillThreads(ctx, nil); err == nil {
		t.Error("cancelled context must stop the backfill with an error")
	}
}

func TestSearchV2ThreadsFacetsPaging(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ { // five threads of two messages about "invoice"
		root := fmt.Sprintf("t%d@x", i)
		a, b := fmt.Sprintf("pm:a%d", i), fmt.Sprintf("pm:b%d", i)
		thrAdd(t, c, "p", a, "", thrMsg(root, "Billing <bill@acme.com>", fmt.Sprintf("Invoice %d", i), "please pay the invoice", thr0.Add(time.Duration(i)*24*time.Hour)), thr0)
		thrAdd(t, c, "p", b, "", thrMsg(fmt.Sprintf("r%d@x", i), "Me <me@example.com>", fmt.Sprintf("Re: Invoice %d", i), "invoice paid", thr0.Add(time.Duration(i)*24*time.Hour+time.Hour),
			"References: <"+root+">"), thr0)
		ids = append(ids, a, b)
	}
	thrAdd(t, c, "p", "pm:z", "", thrMsg("z@x", "Other <o@other.org>", "lunch", "no money talk", thr0), thr0)
	ids = append(ids, "pm:z")
	c.threadNew(ctx, "p", ids)

	res, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "invoice", Limit: 2}, Facets: []string{"sender", "domain", "month", "list_id"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 || len(res.Threads) != 2 {
		t.Fatalf("total %d threads %d, want 5 and 2", res.Total, len(res.Threads))
	}
	h := res.Threads[0]
	if h.NMsgs != 2 || h.Matched != 2 || h.TopStableID != "pm:b4" || !strings.Contains(h.Subject, "Invoice 4") || strings.HasPrefix(h.Subject, "Re:") {
		t.Errorf("thread hit: %+v", h)
	}
	if !strings.Contains(h.Snippet, "[invoice]") {
		t.Errorf("snippet = %q", h.Snippet)
	}
	// Facets are over all 10 matching messages, not the page.
	got := map[string]int{}
	for _, f := range res.Facets["sender"] {
		got[f.Value] = f.Count
	}
	if got["bill@acme.com"] != 5 || got["me@example.com"] != 5 {
		t.Errorf("sender facet = %v", res.Facets["sender"])
	}
	if d := res.Facets["domain"]; len(d) != 2 || d[0].Count != 5 {
		t.Errorf("domain facet = %v", d)
	}
	if m := res.Facets["month"]; len(m) == 0 || m[0].Value != "2026-03" || m[0].Count != 10 {
		t.Errorf("month facet = %v", m)
	}
	if l := res.Facets["list_id"]; len(l) != 0 {
		t.Errorf("list_id facet = %v", l)
	}
	// Paging: offset continues where the page ended and covers all threads once.
	seen := map[string]bool{}
	for _, th := range res.Threads {
		seen[th.TID] = true
	}
	for off := 2; off < 5; off += 2 {
		p, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "invoice", Limit: 2}, Offset: off, Facets: []string{"sender"}})
		if err != nil {
			t.Fatal(err)
		}
		if p.Facets != nil {
			t.Error("facets only on the first page")
		}
		for _, th := range p.Threads {
			if seen[th.TID] {
				t.Errorf("thread %s on two pages", th.TID)
			}
			seen[th.TID] = true
		}
	}
	if len(seen) != 5 {
		t.Errorf("paged over %d threads, want 5", len(seen))
	}
	// Message mode keeps one hit per message.
	mm, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "invoice", Limit: 100}, GroupBy: "message"})
	if err != nil || mm.Total != 10 || len(mm.Messages) != 10 {
		t.Fatalf("message mode: %+v err %v", mm.Total, err)
	}
	// A message not yet threaded is a thread of one, "u:".
	thrAdd(t, c, "p", "pm:new", "", thrMsg("new@x", "N <n@x.com>", "Invoice new", "invoice again", thr0.Add(100*24*time.Hour)), thr0)
	r2, _ := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "invoice", Limit: 1}})
	if r2.Total != 6 || r2.Threads[0].TID != "u:pm:new" || r2.Threads[0].NMsgs != 1 {
		t.Errorf("unthreaded: %+v", r2.Threads)
	}
	// No text: filters only.
	nt, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{From: "other.org"}})
	if err != nil || nt.Total != 1 {
		t.Errorf("filter-only: %+v %v", nt.Total, err)
	}
	// Nothing searchable matches nothing.
	if e, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "!!!"}}); err != nil || e.Total != 0 {
		t.Errorf("empty expr: %v %v", e.Total, err)
	}
}

func TestThreadMessagesAndBodyNewTolerance(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	thrAdd(t, c, "p", "pm:a", "", thrMsg("a@x", "A <a@x.com>", "s", "one", thr0), thr0)
	thrAdd(t, c, "p", "pm:b", "", thrMsg("b@x", "B <b@x.com>", "Re: s", "two", thr0.Add(time.Hour), "References: <a@x>"), thr0)
	c.threadNew(ctx, "p", []string{"pm:a", "pm:b"})
	ms, subj, err := c.ThreadMessages(ctx, "p", thrTid(t, c, "p", "pm:a"))
	if err != nil || len(ms) != 2 || ms[0].StableID != "pm:a" || ms[1].Depth != 1 || subj != "s" {
		t.Fatalf("members %+v subj %q err %v", ms, subj, err)
	}
	if _, _, err := c.ThreadMessages(ctx, "p", "j:nope"); err != ErrNotFound {
		t.Errorf("unknown tid: %v", err)
	}
	if _, _, err := c.ThreadMessages(ctx, "other", thrTid(t, c, "p", "pm:a")); err != ErrNotFound {
		t.Errorf("another account's tid: %v", err)
	}
	if one, _, err := c.ThreadMessages(ctx, "p", "u:pm:a"); err != nil || len(one) != 1 {
		t.Errorf("u: tid: %v %v", one, err)
	}
	// body_new comes from message_fts2 when it has the row, else from the blob.
	got := c.BodyNew(ctx, "p", []string{"pm:a", "pm:b", "pm:none"})
	if got["pm:a"] != "one" || got["pm:b"] != "two" || len(got) != 2 {
		t.Errorf("body_new = %v", got)
	}
	if _, err := c.db.Exec(`UPDATE message_fts2 SET body_new = 'stripped' WHERE stable_id = 'pm:b'`); err != nil {
		t.Fatal(err)
	}
	if got := c.BodyNew(ctx, "p", []string{"pm:b"}); got["pm:b"] != "stripped" {
		t.Errorf("fts2 value not preferred: %v", got)
	}
	if _, err := c.db.Exec(`DELETE FROM message_fts2 WHERE stable_id = 'pm:a'`); err != nil {
		t.Fatal(err)
	}
	if got := c.BodyNew(ctx, "p", []string{"pm:a"}); got["pm:a"] != "one" {
		t.Errorf("no fts2 row must fall back to the blob: %v", got)
	}
}

func TestSearchLogFetchedReformulatedAndStats(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	log := func(q string, refs ...ResultRef) int64 {
		id, err := c.LogSearch(ctx, SearchRecord{Account: "p", Query: q, Key: QueryKey("p", q), Mode: "thread", Hits: len(refs), Total: len(refs), Refs: refs})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fetched := func(id int64) int {
		return thrCount(t, c, `SELECT fetched FROM search_log WHERE id = ?`, id)
	}
	reform := func(id int64) (int64, bool) {
		var r *int64
		_ = c.db.QueryRow(`SELECT reformulated_of FROM search_log WHERE id = ?`, id).Scan(&r)
		if r == nil {
			return 0, false
		}
		return *r, true
	}
	s1 := log("invoice acme", ResultRef{"p", "pm:1", "j:aa"})
	c.NoteFetch(ctx, "p", "", "pm:1")
	if fetched(s1) != 1 {
		t.Error("fetching a returned message must mark the search")
	}
	// A different search, nothing fetched, then a similar one within 2 minutes.
	s2 := log("billing statement", ResultRef{"p", "pm:2", "j:bb"})
	now = now.Add(30 * time.Second)
	s3 := log("billing statement march")
	if r, ok := reform(s3); !ok || r != s2 {
		t.Errorf("s3 reformulated_of = %d,%v want %d", r, ok, s2)
	}
	if _, ok := reform(s2); ok {
		t.Error("s2 must not be a reformulation")
	}
	// A repeat of the same query (or its next page) is not a reformulation.
	s4 := log("billing statement march")
	if _, ok := reform(s4); ok {
		t.Error("same query repeated counted as reformulation")
	}
	// Fetching a thread marks the search that returned it, within 10 minutes only.
	s5 := log("zebra", ResultRef{"p", "pm:9", "j:zz"})
	now = now.Add(11 * time.Minute)
	c.NoteFetch(ctx, "p", "j:zz")
	if fetched(s5) != 0 {
		t.Error("a fetch after 10 minutes must not count")
	}
	s6 := log("unrelated words here", ResultRef{"p", "pm:7", "j:yy"})
	c.NoteFetch(ctx, "p", "j:yy")
	if fetched(s6) != 1 {
		t.Error("get_thread by tid must mark the search")
	}
	// Too late for reformulation.
	s7 := log("billing statement april")
	now = now.Add(3 * time.Minute)
	s8 := log("billing statement april 2")
	if _, ok := reform(s8); ok {
		t.Errorf("reformulation after more than 2 minutes (prev %d)", s7)
	}
	// A page of a search is logged but is not a search.
	id, err := c.LogSearch(ctx, SearchRecord{Account: "p", Query: "x", Key: QueryKey("p", "x"), Mode: "thread", Page: true})
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	st, err := c.SearchStatsSince(ctx, 14)
	if err != nil {
		t.Fatal(err)
	}
	if st.Searches != 8 || st.Verdict == "" || st.VectorSearch {
		t.Errorf("stats = %+v", st)
	}
	if st.AbandonedRe != 1 || st.Reformulate != 1 {
		t.Errorf("abandoned-then-reformulated = %d reformulated = %d, want 1 and 1", st.AbandonedRe, st.Reformulate)
	}
	// Ring is bounded at 50 searches.
	for i := 0; i < 80; i++ {
		log(fmt.Sprintf("q%d", i), ResultRef{"p", fmt.Sprintf("pm:x%d", i), ""})
	}
	if n := len(c.searches.ring); n != searchRingSize {
		t.Errorf("ring holds %d, want %d", n, searchRingSize)
	}
}

func TestSearchStatsVectorTrigger(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	for i := 0; i < 12; i++ { // six abandon-and-reformulate pairs
		q := fmt.Sprintf("topic%d detail", i/2)
		if i%2 == 1 {
			q += " more"
		}
		now = now.Add(time.Duration(5-i%2*4) * time.Minute / 2)
		if _, err := c.LogSearch(ctx, SearchRecord{Account: "p", Query: q, Key: QueryKey(q), Mode: "thread"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ { // and some that were read
		q := fmt.Sprintf("solo%d", i)
		now = now.Add(5 * time.Minute)
		if _, err := c.LogSearch(ctx, SearchRecord{Account: "p", Query: q, Key: QueryKey(q), Mode: "thread", Refs: []ResultRef{{"p", fmt.Sprintf("pm:%d", i), ""}}}); err != nil {
			t.Fatal(err)
		}
		c.NoteFetch(ctx, "p", "", fmt.Sprintf("pm:%d", i))
	}
	st, err := c.SearchStatsSince(ctx, 14)
	if err != nil {
		t.Fatal(err)
	}
	if st.Searches != 24 || !st.VectorSearch || st.AbandonedRe != 6 {
		t.Errorf("stats = %+v", st)
	}
}

func TestSchemaUpgradeAddsColumnsWithoutWipe(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	thrAdd(t, c, "p", "pm:a", "", thrMsg("a@x", "A <a@x.com>", "s", "one", thr0), thr0)
	// Pretend this database predates the columns: drop them by rebuilding messages.
	if _, err := c.db.Exec(`ALTER TABLE messages DROP COLUMN message_id`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if n := thrCount(t, c2, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Errorf("reopen wiped the index: %d messages", n)
	}
	if n := thrCount(t, c2, `SELECT COUNT(*) FROM messages WHERE message_id IS NULL`); n != 1 {
		t.Errorf("column not re-added as NULL: %d", n)
	}
}

func thrAttMsg(mid, from, subj, body, filename string, when time.Time) string {
	return fmt.Sprintf("From: %s\r\nTo: me@example.com\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=QQ\r\n\r\n"+
		"--QQ\r\nContent-Type: text/plain\r\n\r\n%s\r\n--QQ\r\nContent-Type: application/octet-stream; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\n"+
		"Content-Transfer-Encoding: base64\r\n\r\nAAEC\r\n--QQ--\r\n", from, subj, when.Format(time.RFC1123Z), mid, body, filename, filename)
}

func TestSearchV2UsesFTS2AndAttachmentFilenames(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	if !c.fts2Ready.Load() {
		t.Fatal("fresh cache must be fts2-ready")
	}
	thrAdd(t, c, "p", "pm:old", "", thrMsg("old@x", "A <a@x.com>", "terms", "the contract terms are attached", thr0), thr0)
	// Newer, but found only through a file name: ranks below the body hit.
	thrAdd(t, c, "p", "pm:att", "", thrAttMsg("att@x", "B <b@x.com>", "see attached", "nothing relevant", "contract-final.bin", thr0.Add(48*time.Hour)), thr0)
	thrAdd(t, c, "q", "pm:other", "", thrAttMsg("o@x", "C <c@x.com>", "other account", "nothing", "contract-other.bin", thr0.Add(72*time.Hour)), thr0)
	c.threadNew(ctx, "p", []string{"pm:old", "pm:att"})
	c.threadNew(ctx, "q", []string{"pm:other"})

	res, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "p", Text: "contract"}})
	if err != nil || res.Total != 2 || len(res.Threads) != 2 {
		t.Fatalf("threads %+v err %v", res.Total, err)
	}
	if res.Threads[0].TopStableID != "pm:old" || !strings.Contains(res.Threads[0].Snippet, "[contract]") {
		t.Errorf("body hit must come first with a body snippet: %+v", res.Threads[0])
	}
	if res.Threads[1].TopStableID != "pm:att" || !strings.HasPrefix(res.Threads[1].Snippet, "attachment: ") {
		t.Errorf("attachment-only hit must rank below with an attachment snippet: %+v", res.Threads[1])
	}
	// Message mode, filters honoured, account isolation, facets over both kinds.
	mm, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "contract", From: "b@x.com"}, GroupBy: "message", Facets: []string{"sender"}})
	if err != nil || mm.Total != 1 || mm.Messages[0].StableID != "pm:att" || len(mm.Facets["sender"]) != 1 {
		t.Fatalf("from filter on the attachment branch: %+v err %v", mm.Total, err)
	}
	all, _ := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "contract"}, GroupBy: "message", Facets: []string{"sender"}})
	if all.Total != 3 || all.Messages[2].StableID != "pm:other" && all.Messages[2].StableID != "pm:att" || len(all.Facets["sender"]) != 3 {
		t.Errorf("all accounts: %d %+v", all.Total, all.Facets)
	}
	// fts_syntax: the attachment branch is left out, "body:" covers body_new and body_full.
	fx, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "p", Text: "body:contract", FTSSyntax: true}, GroupBy: "message"})
	if err != nil || fx.Total != 1 || fx.Messages[0].StableID != "pm:old" {
		t.Errorf("fts syntax: %+v %v", fx.Total, err)
	}
}

func TestThreadsAndFts2BackfillJobsCoexist(t *testing.T) {
	backfillPause = 0
	c := thrOpen(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		thrAdd(t, c, "p", fmt.Sprintf("pm:%d", i), "", thrMsg(fmt.Sprintf("m%d@x", i), "A <a@x.com>", "s", "b", thr0), thr0)
	}
	row := func(name string) string {
		var l, d, mr, tot, pr int
		if err := c.db.QueryRow(`SELECT last_rowid, done, max_rowid, total, processed FROM backfill WHERE name = ?`, name).Scan(&l, &d, &mr, &tot, &pr); err != nil {
			return "none"
		}
		return fmt.Sprint(l, d, mr, tot, pr)
	}
	before := row(backfillName)
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if after := row(backfillName); after != before {
		t.Errorf("threads job changed the fts2 row: %s -> %s", before, after)
	}
	if got := row("threads"); got != "5 1 0 5 5" {
		t.Errorf("threads row = %s, want last 5, done, total 5, processed 5", got)
	}
	st, err := c.ThreadsBackfillStatus(ctx)
	if err != nil || !st.Complete || st.Total != 5 || st.Done != 5 {
		t.Errorf("status = %+v %v", st, err)
	}
}

func TestThreadNewSurvivesDummyOnlyTopPair(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	thrAdd(t, c, "p", "pm:1", "", thrMsg("m1@x", "A <a@x.com>", "x", "one", thr0, "References: <p@x> <d2@x>"), thr0)
	thrAdd(t, c, "p", "pm:2", "", thrMsg("m2@x", "B <b@x.com>", "x", "two", thr0.Add(time.Hour), "References: <d1@x> <d2@x>"), thr0.Add(time.Hour))
	c.threadNew(ctx, "p", []string{"pm:1", "pm:2"})
	if n := thrCount(t, c, `SELECT COUNT(*) FROM message_thread`); n != 2 {
		t.Errorf("%d threaded, want 2", n)
	}
}

func TestStoredIDsAndReferencesAreCapped(t *testing.T) {
	long := strings.Repeat("x", 300)
	var refs []string
	for i := 0; i < 90; i++ {
		refs = append(refs, fmt.Sprintf("<%s%d@h>", strings.Repeat("r", 240), i))
	}
	raw := "Message-ID: <" + long + "@h>\r\nIn-Reply-To: <" + long + "@h>\r\nReferences: <ok@h> " + strings.Join(refs, " ") + "\r\n\r\nbody\r\n"
	th := parseMessage([]byte(raw)).Thr
	if th.MessageID != "" || th.InReplyTo != "" {
		t.Errorf("over-long ids kept: %d %d", len(th.MessageID), len(th.InReplyTo))
	}
	if len(th.refsJSON()) > maxRefsJSON {
		t.Errorf("references_json is %d bytes", len(th.refsJSON()))
	}
	if len(th.Refs) < 2 || th.Refs[0] != "ok@h" {
		t.Errorf("first reference lost: %d refs", len(th.Refs))
	}
	for _, r := range th.Refs {
		if len(r) > maxIDBytes {
			t.Fatalf("ref of %d bytes kept", len(r))
		}
	}
}

func TestSearchLogRetention(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if _, err := c.db.Exec(`INSERT INTO search_log (at, query, mode) VALUES (?, 'old', 'thread'), (?, 'new', 'thread')`,
		now.Add(-401*24*time.Hour).Unix(), now.Add(-399*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	n, err := c.PruneSearchLog(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d err %v", n, err)
	}
	var q string
	_ = c.db.QueryRow(`SELECT query FROM search_log`).Scan(&q)
	if q != "new" {
		t.Errorf("kept %q, want the query text of the recent row", q)
	}
}

func TestThreadOutlineUsesIndexesNotBlobs(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	thrAdd(t, c, "p", "pm:a", "", thrMsg("a@x", "A <a@x.com>", "s", "hello outline text", thr0), thr0)
	thrAdd(t, c, "p", "pm:b", "", thrAttMsg("b@x", "B <b@x.com>", "s2", "with a file", "f.bin", thr0), thr0)
	// Remove the blobs: the outline must still come from the index.
	for _, id := range []string{"pm:a", "pm:b"} {
		var sum string
		_ = c.db.QueryRow(`SELECT blob_sha256 FROM messages WHERE stable_id = ?`, id).Scan(&sum)
		_ = os.Chmod(c.BlobPath(sum), 0o600)
		_ = os.Remove(c.BlobPath(sum))
	}
	got := c.ThreadOutline(ctx, "p", []string{"pm:a", "pm:b", "pm:none"})
	if got["pm:a"].Text != "hello outline text" || got["pm:a"].HasAtt {
		t.Errorf("a = %+v", got["pm:a"])
	}
	if got["pm:b"].Text != "with a file" || !got["pm:b"].HasAtt {
		t.Errorf("b = %+v", got["pm:b"])
	}
	if _, ok := got["pm:none"]; ok {
		t.Error("unknown id present")
	}
}

func TestRootSubjectAndDuplicateOwnerFollowInternalDate(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	// A thread whose top is a message we never got: two messages name it. The
	// attacker's forges an early Date but arrived last.
	thrAdd(t, c, "p", "pm:m1", "", thrMsg("m1@x", "Alice <alice@x.com>", "Honest subject", "a", thr0.Add(10*time.Hour), "References: <gone@x>"), thr0.Add(time.Hour))
	thrAdd(t, c, "p", "pm:m2", "", thrMsg("m2@x", "Mallory <m@evil.test>", "Forged subject", "b", thr0.Add(-100*24*time.Hour), "References: <gone@x>"), thr0.Add(2*time.Hour))
	c.threadNew(ctx, "p", []string{"pm:m1", "pm:m2"})
	tid := thrTid(t, c, "p", "pm:m1")
	if thrTid(t, c, "p", "pm:m2") != tid {
		t.Fatal("both name the same missing parent: one thread")
	}
	var root, subj string
	var first int64
	_ = c.db.QueryRow(`SELECT root_stable_id, subject_norm, first_at FROM threads WHERE tid = ?`, tid).Scan(&root, &subj, &first)
	if root != "pm:m1" || subj != "honest subject" || first != thr0.Add(time.Hour).Unix() {
		t.Errorf("root %s subject %q first_at %d: must follow arrival, not the forged Date", root, subj, first)
	}
	// Duplicate Message-ID: the earliest arrival owns the id, though its Date is later.
	thrAdd(t, c, "p", "pm:real", "", thrMsg("dup@x", "Alice <alice@x.com>", "real one", "r", thr0.Add(50*time.Hour)), thr0.Add(3*time.Hour))
	thrAdd(t, c, "p", "pm:fake", "", thrMsg("dup@x", "Mallory <m@evil.test>", "fake one", "f", thr0.Add(-90*24*time.Hour)), thr0.Add(9*time.Hour))
	thrAdd(t, c, "p", "pm:rep", "", thrMsg("rep@x", "Alice <alice@x.com>", "Re: real one", "x", thr0.Add(60*time.Hour), "References: <dup@x>"), thr0.Add(10*time.Hour))
	c.threadNew(ctx, "p", []string{"pm:real", "pm:fake", "pm:rep"})
	var parent string
	_ = c.db.QueryRow(`SELECT parent_stable_id FROM message_thread WHERE stable_id = 'pm:rep'`).Scan(&parent)
	if parent != "pm:real" {
		t.Errorf("reply's parent = %q, want pm:real (first arrival owns the id)", parent)
	}
}

func TestOutsiderIsStoredAndOwnerExempt(t *testing.T) {
	c := thrOpen(t)
	c.SetOwners(map[string][]string{"p": {"Me@Home.test"}})
	ctx := context.Background()
	thrAdd(t, c, "p", "pm:a", "", thrMsg("a@x", "Alice <alice@x.com>", "s", "a", thr0), thr0)
	thrAdd(t, c, "p", "pm:me", "", thrMsg("me@x", "Me <me@home.test>", "Re: s", "b", thr0.Add(time.Hour), "References: <a@x>"), thr0.Add(time.Hour))
	thrAdd(t, c, "p", "pm:evil", "", thrMsg("e@x", "Eve <eve@evil.test>", "Re: s", "c", thr0.Add(2*time.Hour), "References: <a@x>"), thr0.Add(2*time.Hour))
	c.threadNew(ctx, "p", []string{"pm:a", "pm:me", "pm:evil"})
	flag := func(id string) int {
		return thrCount(t, c, `SELECT outsider FROM message_thread WHERE stable_id = ?`, id)
	}
	if flag("pm:a") != 0 || flag("pm:me") != 0 || flag("pm:evil") != 1 {
		t.Errorf("outsider flags a=%d me=%d evil=%d, want 0 0 1", flag("pm:a"), flag("pm:me"), flag("pm:evil"))
	}
	ms, _, err := c.ThreadMessages(ctx, "p", thrTid(t, c, "p", "pm:a"))
	if err != nil || len(ms) != 3 || !ms[2].Outsider || ms[1].Outsider {
		t.Errorf("ThreadMessages outsiders: %+v %v", ms, err)
	}
}

func TestCappedComponentIsNotRegatheredPerSeed(t *testing.T) {
	old := maxComponent
	maxComponent = 10
	defer func() { maxComponent = old }()
	c := thrOpen(t)
	ctx := context.Background()
	const n = 60
	var ids []string
	for i := 0; i < n; i++ { // one long chain, far over the cap
		hdr := []string{}
		if i > 0 {
			hdr = append(hdr, fmt.Sprintf("References: <c%d@x>", i-1))
		}
		id := fmt.Sprintf("pm:%d", i)
		thrAdd(t, c, "p", id, "", thrMsg(fmt.Sprintf("c%d@x", i), "A <a@x.com>", "chain", "b", thr0.Add(time.Duration(i)*time.Minute), hdr...), thr0.Add(time.Duration(i)*time.Minute))
		ids = append(ids, id)
	}
	gathers := 0
	gatherHook = func() { gathers++ }
	defer func() { gatherHook = nil }()
	tx, _ := c.db.Begin()
	defer func() { _ = tx.Rollback() }()
	if err := threadTx(ctx, tx, "p", ids); err != nil {
		t.Fatal(err)
	}
	if gathers > 2 {
		t.Errorf("%d gathers for %d seeds over a capped component: seeds beyond the cap must not each gather", gathers, n)
	}
	var threaded int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM message_thread`).Scan(&threaded)
	if threaded != n {
		t.Errorf("%d threaded, want %d", threaded, n)
	}
	// Run again on a settled component: reads only, no writes.
	var before, after int
	_ = tx.QueryRow(`SELECT total_changes()`).Scan(&before)
	if err := threadTx(ctx, tx, "p", ids[:5]); err != nil {
		t.Fatal(err)
	}
	_ = tx.QueryRow(`SELECT total_changes()`).Scan(&after)
	// The savepoint bookkeeping is not a change; rows written must be zero.
	if after != before {
		t.Errorf("re-threading an unchanged component wrote %d rows", after-before)
	}
}

func TestBackfillSurvivesPoisonComponentAndBatch(t *testing.T) {
	backfillPause = 0
	c := thrOpen(t)
	ctx := context.Background()
	thrAdd(t, c, "p", "pm:good", "", thrMsg("g@x", "A <a@x.com>", "good", "b", thr0), thr0)
	thrAdd(t, c, "p", "pm:bad", "", thrMsg("bad@x", "A <a@x.com>", "bad", "b", thr0), thr0)
	thrAdd(t, c, "p", "pm:bad2", "", thrMsg("bad2@x", "A <a@x.com>", "Re: bad", "b", thr0.Add(time.Hour), "References: <bad@x>"), thr0)
	componentHook = func(comp []threadRow) {
		for _, r := range comp {
			if r.stableID == "pm:bad" {
				panic("poison")
			}
		}
	}
	defer func() { componentHook = nil }()
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatalf("backfill died on a poison component: %v", err)
	}
	if thrCount(t, c, `SELECT COUNT(*) FROM message_thread`) != 3 {
		t.Error("every message, poisoned or not, must end up threaded")
	}
	if thrTid(t, c, "p", "pm:bad") != soloTID("pm:bad") || thrTid(t, c, "p", "pm:bad2") != soloTID("pm:bad2") {
		t.Error("poisoned component must be threaded alone")
	}
	if thrTid(t, c, "p", "pm:good") == soloTID("pm:good") {
		t.Error("the good component must be threaded normally")
	}
	if _, done, _ := c.backfillState(ctx, threadBackfillName); !done {
		t.Error("backfill must finish")
	}

	// A panic outside any component: the batch is stepped over, not retried.
	c2 := thrOpen(t)
	for i := 0; i < 3; i++ {
		thrAdd(t, c2, "p", fmt.Sprintf("pm:%d", i), "", thrMsg(fmt.Sprintf("m%d@x", i), "A <a@x.com>", "s", "b", thr0), thr0)
	}
	calls := 0
	batchHook = func() {
		calls++
		if calls == 1 {
			panic("poison batch")
		}
	}
	defer func() { batchHook = nil }()
	if err := c2.BackfillThreads(ctx, nil); err != nil {
		t.Fatalf("backfill died on a poison batch: %v", err)
	}
	if thrCount(t, c2, `SELECT COUNT(*) FROM message_thread`) != 3 {
		t.Error("stepped-over rows must count as threaded")
	}
}

func TestSearchV2TotalOnPageBeyondTheEnd(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		thrAdd(t, c, "p", fmt.Sprintf("pm:%d", i), "", thrMsg(fmt.Sprintf("m%d@x", i), "A <a@x.com>", fmt.Sprintf("s%d", i), "needle", thr0.Add(time.Duration(i)*time.Hour)), thr0)
	}
	c.threadNew(ctx, "p", []string{"pm:0", "pm:1", "pm:2"})
	for _, g := range []string{"thread", "message"} {
		r, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Text: "needle", Limit: 2}, GroupBy: g, Offset: 10})
		if err != nil || r.Total != 3 || len(r.Threads)+len(r.Messages) != 0 {
			t.Errorf("%s: total %d hits %d err %v, want total 3 and no hits", g, r.Total, len(r.Threads)+len(r.Messages), err)
		}
	}
}

func TestOutsiderToCcRecipientIsNotOutsiderInDatabase(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	thrAdd(t, c, "p", "pm:a", "", "From: Alice <alice@x.com>\r\nTo: Bob <bob@x.com>\r\nCc: carol@x.com, \"D, E\" <dee@x.com>\r\nSubject: s\r\nDate: "+thr0.Format(time.RFC1123Z)+"\r\nMessage-ID: <a@x>\r\n\r\nhi\r\n", thr0)
	thrAdd(t, c, "p", "pm:b", "", thrMsg("b@x", "Bob <bob@x.com>", "Re: s", "b", thr0.Add(time.Hour), "References: <a@x>"), thr0.Add(time.Hour))
	thrAdd(t, c, "p", "pm:c", "", thrMsg("c@x", "Dee <dee@x.com>", "Re: s", "c", thr0.Add(2*time.Hour), "References: <a@x>"), thr0.Add(2*time.Hour))
	thrAdd(t, c, "p", "pm:e", "", thrMsg("e@x", "Eve <eve@evil.test>", "Re: s", "e", thr0.Add(3*time.Hour), "References: <a@x>"), thr0.Add(3*time.Hour))
	c.threadNew(ctx, "p", []string{"pm:a", "pm:b", "pm:c", "pm:e"})
	for id, want := range map[string]int{"pm:a": 0, "pm:b": 0, "pm:c": 0, "pm:e": 1} {
		if got := thrCount(t, c, `SELECT outsider FROM message_thread WHERE stable_id = ?`, id); got != want {
			t.Errorf("%s outsider = %d, want %d", id, got, want)
		}
	}
}
