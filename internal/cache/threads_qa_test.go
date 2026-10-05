package cache

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// qaMsg is one generated message of a corpus.
type qaMsg struct {
	id, gm, raw string
	when        time.Time
}

// qaCorpus builds trees of replies (full References chains), single messages
// and orphan replies whose parents never arrive. Every message has a distinct
// date, so BuildThreads is fully determined.
func qaCorpus(seed int64, threads int) []qaMsg {
	r := rand.New(rand.NewSource(seed))
	var out []qaMsg
	n := 0
	next := func() time.Time { n++; return thr0.Add(time.Duration(n) * time.Minute) }
	for ti := 0; ti < threads; ti++ {
		size := 1 + r.Intn(6)
		subj := fmt.Sprintf("topic-%d-%d", seed, ti)
		mids := make([]string, 0, size)
		for k := 0; k < size; k++ {
			mid := fmt.Sprintf("t%d-%d-%d@x", seed, ti, k)
			var hdr []string
			s := subj
			if k > 0 {
				p := r.Intn(k)
				// the chain of ancestors of p, then p
				chain := []string{mids[p]}
				_ = p
				hdr = append(hdr, "References: <"+strings.Join(append([]string{mids[0]}, chain...), "> <")+">", "In-Reply-To: <"+mids[p]+">")
				s = "Re: " + subj
			} else if ti%5 == 4 {
				// orphan: this one answers a message that never arrives
				hdr = append(hdr, "References: <missing-"+subj+"@x>", "In-Reply-To: <missing-"+subj+"@x>")
				s = "Re: " + subj
			}
			when := next()
			mids = append(mids, mid)
			out = append(out, qaMsg{id: fmt.Sprintf("pm:%d-%d-%d", seed, ti, k),
				raw: thrMsg(mid, fmt.Sprintf("P%d <p%d@x.com>", k, k), s, "body "+mid, when, hdr...), when: when})
		}
	}
	return out
}

// partition reads message_thread as stable_id -> tid.
func partition(t *testing.T, c *Cache) map[string]string {
	t.Helper()
	rows, err := c.db.Query(`SELECT account, stable_id, tid FROM message_thread`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, s, tid string
		if err := rows.Scan(&a, &s, &tid); err != nil {
			t.Fatal(err)
		}
		out[a+"/"+s] = tid
	}
	return out
}

// canon turns a stable_id -> tid map into a set of sorted member lists.
func canon(p map[string]string) string {
	g := map[string][]string{}
	for s, tid := range p {
		g[tid] = append(g[tid], s)
	}
	var parts []string
	for _, m := range g {
		sort.Strings(m)
		parts = append(parts, strings.Join(m, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// reference is the partition BuildThreads gives over every non-Gmail message.
func reference(t *testing.T, c *Cache, account string) map[string]string {
	t.Helper()
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT stable_id FROM messages WHERE account = ? AND gm_thread_id = ''`, account)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		ids = append(ids, s)
	}
	rows.Close()
	trs, err := loadThreadRows(context.Background(), tx, account, ids)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make([]ThreadMsg, len(trs))
	for i, r := range trs {
		msgs[i] = r.msg()
	}
	out := map[string]string{}
	for _, a := range BuildThreads(msgs) {
		out[account+"/"+a.StableID] = a.TID
	}
	return out
}

// checkInvariants: n_msgs = member rows, no threads row without members, no
// member row without a threads row.
func checkInvariants(t *testing.T, c *Cache) {
	t.Helper()
	if n := thrCount(t, c, `SELECT COUNT(*) FROM threads th WHERE n_msgs != (SELECT COUNT(*) FROM message_thread t WHERE t.account = th.account AND t.tid = th.tid)`); n != 0 {
		t.Errorf("%d threads rows whose n_msgs differs from their message_thread rows", n)
	}
	if n := thrCount(t, c, `SELECT COUNT(*) FROM threads th WHERE NOT EXISTS (SELECT 1 FROM message_thread t WHERE t.account = th.account AND t.tid = th.tid)`); n != 0 {
		t.Errorf("%d threads rows with no members", n)
	}
	if n := thrCount(t, c, `SELECT COUNT(*) FROM message_thread t WHERE NOT EXISTS (SELECT 1 FROM threads th WHERE t.account = th.account AND t.tid = th.tid)`); n != 0 {
		t.Errorf("%d members whose thread has no threads row", n)
	}
	if n := thrCount(t, c, `SELECT COUNT(*) FROM message_thread t WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE t.account = m.account AND t.stable_id = m.stable_id)`); n != 0 {
		t.Errorf("%d message_thread rows for missing messages", n)
	}
}

func TestQAThreadNewAnyArrivalOrderSamePartition(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 6; seed++ {
		corpus := qaCorpus(seed, 14)
		// the expectation: one pass over everything, in order, one batch
		base := thrOpen(t)
		for _, m := range corpus {
			thrAdd(t, base, "p", m.id, "", m.raw, m.when)
		}
		ids := make([]string, len(corpus))
		for i, m := range corpus {
			ids[i] = m.id
		}
		base.threadNew(ctx, "p", ids)
		want := canon(partition(t, base))
		if got := canon(reference(t, base, "p")); got != want {
			t.Fatalf("seed %d: threadNew in one batch differs from BuildThreads over all messages", seed)
		}
		checkInvariants(t, base)

		for shuffle := 0; shuffle < 4; shuffle++ {
			r := rand.New(rand.NewSource(seed*100 + int64(shuffle)))
			order := r.Perm(len(corpus))
			c := thrOpen(t)
			for i := 0; i < len(order); {
				k := 1 + r.Intn(7) // a flush of 1..7 messages
				var batch []string
				for _, j := range order[i:min(i+k, len(order))] {
					m := corpus[j]
					thrAdd(t, c, "p", m.id, "", m.raw, m.when)
					batch = append(batch, m.id)
				}
				c.threadNew(ctx, "p", batch)
				checkInvariants(t, c)
				i += k
			}
			if got := canon(partition(t, c)); got != want {
				t.Errorf("seed %d shuffle %d: partition depends on arrival order\n got: %s\nwant: %s", seed, shuffle, got, want)
			}
			if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread`); got != len(corpus) {
				t.Errorf("seed %d shuffle %d: %d threaded of %d", seed, shuffle, got, len(corpus))
			}
			// n_msgs of each thread, again, against the reference partition
			if canon(reference(t, c, "p")) != want {
				t.Errorf("seed %d shuffle %d: reference differs", seed, shuffle)
			}
		}
	}
}

func TestQAThreadTxIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := thrOpen(t)
	corpus := qaCorpus(9, 12)
	var ids []string
	for _, m := range corpus {
		thrAdd(t, c, "p", m.id, "", m.raw, m.when)
		ids = append(ids, m.id)
	}
	thrAdd(t, c, "p", "gm:1", "42", thrMsg("g1@x", "G <g@x.com>", "gmail", "x", thr0), thr0)
	ids = append(ids, "gm:1")
	c.threadNew(ctx, "p", ids)
	dump := func() string {
		rows, err := c.db.Query(`SELECT 'm', stable_id, tid, parent_stable_id, depth, '' FROM message_thread
UNION ALL SELECT 't', tid, root_stable_id, subject_norm, first_at, last_at || '/' || n_msgs || '/' || participants_json FROM threads ORDER BY 1, 2, 3`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var a, b, cc, d, f, g any
			_ = rows.Scan(&a, &b, &cc, &d, &f, &g)
			fmt.Fprintln(&sb, a, b, cc, d, f, g)
		}
		return sb.String()
	}
	first := dump()
	for i := 0; i < 2; i++ {
		tx, _ := c.db.Begin()
		if err := threadTx(ctx, tx, "p", ids); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := dump(); got != first {
			t.Fatalf("run %d of threadTx changed the result", i+2)
		}
	}
	checkInvariants(t, c)
}

func TestQAGmailMessagesAreNeverRethreadedByJWZ(t *testing.T) {
	ctx := context.Background()
	c := thrOpen(t)
	// Two Gmail messages in different Gmail threads that JWZ headers would join,
	// plus a non-Gmail reply naming both, plus a header-less "Re:" with their subject.
	thrAdd(t, c, "g", "gm:1", "100", thrMsg("a@x", "A <a@x.com>", "plan", "one", thr0), thr0)
	thrAdd(t, c, "g", "gm:2", "200", thrMsg("b@x", "B <b@x.com>", "Re: plan", "two", thr0.Add(time.Hour), "References: <a@x>", "In-Reply-To: <a@x>"), thr0)
	thrAdd(t, c, "g", "j:1", "", thrMsg("c@x", "C <c@x.com>", "Re: plan", "three", thr0.Add(2*time.Hour), "References: <a@x> <b@x>", "In-Reply-To: <b@x>"), thr0)
	thrAdd(t, c, "g", "j:2", "", thrMsg("d@x", "D <d@x.com>", "Re: plan", "four", thr0.Add(3*time.Hour)), thr0)
	all := []string{"gm:1", "gm:2", "j:1", "j:2"}
	for round := 0; round < 3; round++ {
		c.threadNew(ctx, "g", all)
		c.threadNew(ctx, "g", []string{"j:1"})
		c.threadNew(ctx, "g", []string{"gm:2"})
		if got := thrTid(t, c, "g", "gm:1"); got != "g:100" {
			t.Fatalf("round %d: gm:1 in %s", round, got)
		}
		if got := thrTid(t, c, "g", "gm:2"); got != "g:200" {
			t.Fatalf("round %d: gm:2 in %s", round, got)
		}
		if got := thrTid(t, c, "g", "j:1"); !strings.HasPrefix(got, "j:") {
			t.Fatalf("round %d: j:1 in %s", round, got)
		}
		for _, id := range []string{"gm:1", "gm:2"} {
			var parent string
			var depth int
			_ = c.db.QueryRow(`SELECT parent_stable_id, depth FROM message_thread WHERE stable_id = ?`, id).Scan(&parent, &depth)
			if parent != "" || depth != 0 {
				t.Errorf("%s was given a JWZ shape: parent %q depth %d", id, parent, depth)
			}
		}
	}
	if n := thrCount(t, c, `SELECT n_msgs FROM threads WHERE tid = 'g:100'`); n != 1 {
		t.Errorf("g:100 n_msgs = %d", n)
	}
	checkInvariants(t, c)
}

func TestQALateRootRelinksManyChildrenNoOrphanRows(t *testing.T) {
	ctx := context.Background()
	c := thrOpen(t)
	// Five children of a root that has not arrived, each in its own thread
	// until a sibling names them; then the root arrives last.
	var ids []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("pm:c%d", i)
		thrAdd(t, c, "p", id, "", thrMsg(fmt.Sprintf("c%d@x", i), "K <k@x.com>", "Re: root", "child", thr0.Add(time.Duration(i+1)*time.Minute),
			"References: <root@x>", "In-Reply-To: <root@x>"), thr0)
		c.threadNew(ctx, "p", []string{id})
		ids = append(ids, id)
	}
	checkInvariants(t, c)
	before := map[string]bool{}
	for _, id := range ids {
		before[thrTid(t, c, "p", id)] = true
	}
	thrAdd(t, c, "p", "pm:root", "", thrMsg("root@x", "R <r@x.com>", "root", "root body", thr0), thr0)
	c.threadNew(ctx, "p", []string{"pm:root"})
	tid := thrTid(t, c, "p", "pm:root")
	for _, id := range ids {
		if got := thrTid(t, c, "p", id); got != tid {
			t.Errorf("%s in %s, want %s", id, got, tid)
		}
		var parent string
		_ = c.db.QueryRow(`SELECT parent_stable_id FROM message_thread WHERE stable_id = ?`, id).Scan(&parent)
		if parent != "pm:root" {
			t.Errorf("%s parent %q, want pm:root", id, parent)
		}
	}
	if n := thrCount(t, c, `SELECT COUNT(*) FROM threads`); n != 1 {
		t.Errorf("%d threads rows, want 1", n)
	}
	for old := range before {
		if old != tid {
			if n := thrCount(t, c, `SELECT COUNT(*) FROM threads WHERE tid = ?`, old); n != 0 {
				t.Errorf("old tid %s left behind", old)
			}
		}
	}
	if n := thrCount(t, c, `SELECT n_msgs FROM threads WHERE tid = ?`, tid); n != 6 {
		t.Errorf("n_msgs = %d, want 6", n)
	}
	checkInvariants(t, c)
}

// qaOldRows makes a cache look like one cached before threads existed.
func qaOldRows(t *testing.T, c *Cache) {
	t.Helper()
	if _, err := c.db.Exec(`UPDATE messages SET message_id = NULL, in_reply_to = NULL, references_json = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`DELETE FROM message_ref`); err != nil {
		t.Fatal(err)
	}
}

func qaFill(t *testing.T, c *Cache, corpus []qaMsg) {
	t.Helper()
	for _, m := range corpus {
		thrAdd(t, c, "p", m.id, "", m.raw, m.when)
	}
	qaOldRows(t, c)
}

func qaSetPause(t *testing.T, d time.Duration) {
	old := backfillPause
	backfillPause = d
	t.Cleanup(func() { backfillPause = old })
}

func TestQABackfillResumedEqualsUninterrupted(t *testing.T) {
	qaSetPause(t, 0)
	ctx := context.Background()
	corpus := qaCorpus(21, 340) // > 3 batches of 500
	if len(corpus) < 1010 {
		t.Fatalf("corpus too small: %d", len(corpus))
	}
	a := thrOpen(t)
	qaFill(t, a, corpus)
	if err := a.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	want := partition(t, a)
	if len(want) != len(corpus) {
		t.Fatalf("uninterrupted run threaded %d of %d", len(want), len(corpus))
	}
	checkInvariants(t, a)
	if got := canon(reference(t, a, "p")); got != canon(want) {
		t.Error("uninterrupted backfill differs from BuildThreads over all messages")
	}
	for k := 1; k <= 2; k++ {
		b := thrOpen(t)
		qaFill(t, b, corpus)
		if err := b.startThreadBackfill(ctx); err != nil {
			t.Fatal(err)
		}
		last := int64(0)
		for i := 0; i < k; i++ { // k batches, then "crash"
			_, next, fin, err := b.backfillThreadBatch(ctx, last)
			if err != nil || fin {
				t.Fatalf("batch %d: fin=%v err=%v", i, fin, err)
			}
			last = next
		}
		if st, done, _ := b.backfillState(ctx, threadBackfillName); st != last || done {
			t.Fatalf("persisted position %d done=%v, want %d", st, done, last)
		}
		if err := b.BackfillThreads(ctx, nil); err != nil { // restart: resumes
			t.Fatal(err)
		}
		if got := partition(t, b); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("k=%d: resumed result differs from the uninterrupted run (%d vs %d rows, canon equal %v)",
				k, len(got), len(want), canon(got) == canon(want))
		}
		checkInvariants(t, b)
		st, _ := b.ThreadsBackfillStatus(ctx)
		if !st.Complete || st.Total != len(corpus) {
			t.Errorf("k=%d status %+v", k, st)
		}
		// message_thread detail (parent, depth) too
		q := `SELECT COUNT(*) FROM message_thread`
		if thrCount(t, b, q) != thrCount(t, a, q) {
			t.Errorf("k=%d row counts differ", k)
		}
	}
}

func TestQABackfillStopsOnCancelMidRunAndResumes(t *testing.T) {
	qaSetPause(t, 30*time.Millisecond)
	c := thrOpen(t)
	corpus := qaCorpus(31, 260)
	qaFill(t, c, corpus)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.BackfillThreads(ctx, nil) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		last, _, _ := c.backfillState(context.Background(), threadBackfillName)
		if last > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled backfill returned nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("backfill did not stop on cancel")
	}
	last, done, _ := c.backfillState(context.Background(), threadBackfillName)
	if done || last == 0 {
		t.Fatalf("after cancel: last=%d done=%v (want partial progress)", last, done)
	}
	qaSetPause(t, 0)
	if err := c.BackfillThreads(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread`); got != len(corpus) {
		t.Errorf("%d threaded of %d after resume", got, len(corpus))
	}
	checkInvariants(t, c)
}

func TestQABackfillBesideConcurrentRefreshLosesAndDuplicatesNothing(t *testing.T) {
	qaSetPause(t, 0)
	ctx := context.Background()
	old := qaCorpus(41, 160)
	fresh := qaCorpus(42, 60)
	c := thrOpen(t)
	qaFill(t, c, old)
	var wg sync.WaitGroup
	wg.Add(1)
	var berr error
	go func() { defer wg.Done(); berr = c.BackfillThreads(ctx, nil) }()
	// "Refresh": new messages arrive and are threaded while the backfill runs.
	r := rand.New(rand.NewSource(7))
	order := r.Perm(len(fresh))
	for i := 0; i < len(order); i += 4 {
		var batch []string
		for _, j := range order[i:min(i+4, len(order))] {
			m := fresh[j]
			thrAdd(t, c, "p", m.id, "", m.raw, m.when)
			batch = append(batch, m.id)
		}
		c.threadNew(ctx, "p", batch)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("backfill did not finish")
	}
	if berr != nil {
		t.Fatal(berr)
	}
	// A message inserted after the backfill passed its rowid is picked up by
	// the next run (the "done" re-check), so run once more as the job would.
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	total := len(old) + len(fresh)
	if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread`); got != total {
		t.Errorf("%d members, want %d", got, total)
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM (SELECT account, stable_id FROM message_thread GROUP BY 1, 2 HAVING COUNT(*) > 1)`); got != 0 {
		t.Errorf("%d duplicated members", got)
	}
	checkInvariants(t, c)
	if canon(partition(t, c)) != canon(reference(t, c, "p")) {
		t.Error("partition after concurrent backfill + refresh differs from BuildThreads over all messages")
	}
}

func TestQABackfillMissingBlobDoesNotStall(t *testing.T) {
	qaSetPause(t, 0)
	ctx := context.Background()
	c := thrOpen(t)
	corpus := qaCorpus(51, 30)
	qaFill(t, c, corpus)
	// Remove the blobs of a few messages.
	rows, _ := c.db.Query(`SELECT blob_sha256 FROM messages WHERE stable_id IN (?, ?, ?)`, corpus[0].id, corpus[3].id, corpus[7].id)
	var removed int
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		if err := os.Remove(c.BlobPath(s)); err == nil {
			removed++
		}
	}
	rows.Close()
	if removed != 3 {
		t.Fatalf("removed %d blobs", removed)
	}
	done := make(chan error, 1)
	go func() { done <- c.BackfillThreads(ctx, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("backfill error with missing blobs: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("backfill stalled on a missing blob")
	}
	if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread`); got != len(corpus) {
		t.Errorf("%d threaded of %d (a message without headers must thread alone)", got, len(corpus))
	}
	if _, d, _ := c.backfillState(ctx, threadBackfillName); !d {
		t.Error("not done")
	}
	checkInvariants(t, c)
}

func TestQAThreadsAndFts2JobsBothProgress(t *testing.T) {
	qaSetPause(t, 0)
	ctx := context.Background()
	c := thrOpen(t)
	corpus := qaCorpus(61, 40)
	for _, m := range corpus {
		thrAdd(t, c, "p", m.id, "", m.raw, m.when)
	}
	// Two independent rows.
	if n := thrCount(t, c, `SELECT COUNT(DISTINCT name) FROM backfill WHERE name IN ('fts2')`); n != 1 {
		t.Fatalf("fts2 row missing")
	}
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := thrCount(t, c, `SELECT COUNT(*) FROM backfill WHERE name IN ('fts2', 'threads')`); n != 2 {
		t.Errorf("want two backfill rows, got %d", n)
	}
	ft, _ := c.BackfillStatus(ctx)
	th, _ := c.ThreadsBackfillStatus(ctx)
	if !th.Complete || th.Total != len(corpus) {
		t.Errorf("threads status %+v", th)
	}
	// The fts2 status is its own row: finishing threads neither completes nor resets it.
	var before, after string
	_ = c.db.QueryRow(`SELECT last_rowid || '/' || done || '/' || processed FROM backfill WHERE name = 'fts2'`).Scan(&before)
	_ = c.BackfillThreads(ctx, nil)
	_ = c.db.QueryRow(`SELECT last_rowid || '/' || done || '/' || processed FROM backfill WHERE name = 'fts2'`).Scan(&after)
	if before != after {
		t.Errorf("fts2 row moved: %s -> %s (fts status %+v)", before, after, ft)
	}
	// And the fts2 job can still run to completion beside it.
	if _, err := c.db.Exec(`UPDATE backfill SET done = 0, last_rowid = 0 WHERE name = 'fts2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE backfill SET max_rowid = (SELECT MAX(rowid) FROM messages), total = (SELECT COUNT(*) FROM messages) WHERE name = 'fts2'`); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		complete, _, err := c.backfillBatch(cctx)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
	}
	if _, d, _ := c.backfillState(ctx, backfillName); !d {
		t.Error("fts2 job did not complete")
	}
	if _, d, _ := c.backfillState(ctx, threadBackfillName); !d {
		t.Error("threads row lost done after the fts2 job ran")
	}
}
