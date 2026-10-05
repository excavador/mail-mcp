package cache

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// dumpGmail reads the Gmail-relevant derived rows as comparable strings.
func dumpGmail(t *testing.T, c *Cache) (mt, th []string) {
	t.Helper()
	rs, err := c.db.Query(`SELECT account, stable_id, tid, parent_stable_id, depth, outsider FROM message_thread ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var a, s, tid, p string
		var d, o int
		_ = rs.Scan(&a, &s, &tid, &p, &d, &o)
		mt = append(mt, fmt.Sprint(a, s, tid, p, d, o))
	}
	_ = rs.Close()
	rs, err = c.db.Query(`SELECT account, tid, root_stable_id, subject_norm, first_at, last_at, n_msgs, participants_json FROM threads ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var a, tid, r, sn, pj string
		var f, l, n int64
		_ = rs.Scan(&a, &tid, &r, &sn, &f, &l, &n, &pj)
		th = append(th, fmt.Sprint(a, tid, r, sn, f, l, n, pj))
	}
	_ = rs.Close()
	return
}

func addGmailCorpus(t *testing.T, c *Cache) []string {
	var ids []string
	add := func(id, gm, from, to, subj string, off time.Duration) {
		raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@x>\r\n\r\nbody\r\n", from, to, subj, thr0.Add(off).Format(time.RFC1123Z), id)
		thrAdd(t, c, "g", id, gm, raw, thr0.Add(off))
		ids = append(ids, id)
	}
	// thread 1: owner writes, A answers, stranger S joins late.
	add("m1", "100", "Me <me@example.com>", "A <a@x.com>", "Plan", 0)
	add("m2", "100", "A <a@x.com>", "me@example.com", "Re: Plan", time.Hour)
	add("m3", "100", "S <s@evil.com>", "me@example.com", "Re: Plan", 2*time.Hour)
	// thread 2: two senders; second was a recipient of the first.
	add("m4", "200", "B <b@x.com>", "C <c@x.com>", "Lunch", 3*time.Hour)
	add("m5", "200", "C <c@x.com>", "b@x.com", "Re: Lunch", 4*time.Hour)
	add("m6", "300", "D <d@x.com>", "me@example.com", "Solo", 5*time.Hour)
	add("m7", "", "E <e@x.com>", "me@example.com", "No thread id", 6*time.Hour)
	return ids
}

// The bulk pass must produce what the per-thread path (threadNew) produces.
func TestBulkThreadGmailMatchesThreadNew(t *testing.T) {
	ctx := context.Background()
	qaSetPause(t, 0)
	ref := thrOpen(t)
	ref.SetOwners(map[string][]string{"g": {"me@example.com"}})
	ids := addGmailCorpus(t, ref)
	ref.threadNew(ctx, "g", ids)

	c := thrOpen(t)
	c.SetOwners(map[string][]string{"g": {"me@example.com"}})
	addGmailCorpus(t, c)
	n, err := c.BulkThreadGmail(ctx, nil)
	if err != nil || n != 6 {
		t.Fatalf("bulk: n=%d err=%v", n, err)
	}
	// m7 (no thread id) is left for the per-message backfill.
	if got := thrCount(t, c, `SELECT COUNT(*) FROM message_thread WHERE stable_id = 'm7'`); got != 0 {
		t.Fatalf("m7 threaded by the bulk pass")
	}
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	wantMT, wantTH := dumpGmail(t, ref)
	gotMT, gotTH := dumpGmail(t, c)
	// m7 is a solo JWZ thread in both; compare everything.
	if fmt.Sprint(wantMT) != fmt.Sprint(gotMT) {
		t.Errorf("message_thread differs\nwant %v\ngot  %v", wantMT, gotMT)
	}
	if fmt.Sprint(wantTH) != fmt.Sprint(gotTH) {
		t.Errorf("threads differ\nwant %v\ngot  %v", wantTH, gotTH)
	}
	if o := thrCount(t, c, `SELECT outsider FROM message_thread WHERE stable_id = 'm3'`); o != 1 {
		t.Errorf("stranger m3 not flagged outsider")
	}
	if o := thrCount(t, c, `SELECT COUNT(*) FROM message_thread WHERE outsider = 1`); o != 1 {
		t.Errorf("outsiders = %d, want 1", o)
	}
	st, _ := c.ThreadsBackfillStatus(ctx)
	if !st.Complete || st.Done != st.Total || st.Total != 7 {
		t.Errorf("status %+v", st)
	}
	// Re-run is a no-op.
	if n, err := c.BulkThreadGmail(ctx, nil); err != nil || n != 0 {
		t.Errorf("rerun n=%d err=%v", n, err)
	}
}

// With the threads job already under way, the bulk pass keeps done/total true.
func TestBulkThreadGmailKeepsProgressMeaningful(t *testing.T) {
	ctx := context.Background()
	qaSetPause(t, 0)
	c := thrOpen(t)
	addGmailCorpus(t, c)
	if err := c.startThreadBackfill(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BulkThreadGmail(ctx, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := c.ThreadsBackfillStatus(ctx)
	if st.Done != 6 || st.Total != 7 {
		t.Errorf("after bulk: %+v", st)
	}
	if err := c.BackfillThreads(ctx, nil); err != nil {
		t.Fatal(err)
	}
	st, _ = c.ThreadsBackfillStatus(ctx)
	if !st.Complete || st.Total != 7 {
		t.Errorf("after backfill: %+v", st)
	}
}

// Set MAILMCP_BULK_N=100000 to measure: go test -run TestBulkThreadGmailMeasure -v
func TestBulkThreadGmailMeasure(t *testing.T) {
	ns := os.Getenv("MAILMCP_BULK_N")
	if ns == "" {
		t.Skip("set MAILMCP_BULK_N to measure")
	}
	n, _ := strconv.Atoi(ns)
	ctx := context.Background()
	c := thrOpen(t)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		acct := "g" + strconv.Itoa(i%2)
		_, err := tx.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, to_addr, subject, date_unix, internal_date, gm_thread_id)
VALUES (?, ?, 'x', ?, 'me@example.com', ?, ?, ?, ?)`, acct, fmt.Sprintf("gm:%08x", (i*2654435761)%4294967296),
			fmt.Sprintf("P%d <p%d@x.com>", i%7, i%7), fmt.Sprintf("Re: subject %d", i/3), 1700000000+i, 1700000000+i, strconv.Itoa(i/3))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	qaSetPause(t, 100*time.Millisecond)
	t0 := time.Now()
	got, err := c.BulkThreadGmail(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("messages=%d threaded=%d took=%s max_write_hold=%s threads=%d", n, got, time.Since(t0).Round(time.Millisecond), c.MaxBackfillHold(),
		thrCount(t, c, `SELECT COUNT(*) FROM threads`))
	backfillPause = 0 // restored by qaSetPause's cleanup
	t0 = time.Now()
	c.ws.maxHold.Store(0)
	got, _ = c.BulkThreadGmail(ctx, nil)
	t.Logf("rerun: threaded=%d took=%s", got, time.Since(t0).Round(time.Millisecond))
}
