package cache

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"strings"
	"testing"
	"time"
)

// entPlain is the text of a message as a person would read it.
const entPlain = `<ul>widgets</ul> & "gizmos" it's`

// entAdd stores a message whose text/plain part is HTML-escaped, then puts the
// index row back the way an older version wrote it: with the escaped text.
// It returns the messages rowid.
func entAdd(t *testing.T, c *Cache, id string, stale bool, body string, when time.Time) int64 {
	t.Helper()
	thrAdd(t, c, "A", id, "", thrMsg(id+"@x", "N <n@x.example>", "note "+id, body, when), when)
	var rid int64
	if err := c.db.QueryRow(`SELECT rowid FROM messages WHERE stable_id = ?`, id).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	if stale {
		if _, err := c.db.Exec(`UPDATE message_fts2 SET body_new = ?, body_full = '' WHERE rowid = ?`, body, rid); err != nil {
			t.Fatal(err)
		}
	}
	return rid
}

func restartEntities(t *testing.T, c *Cache) {
	t.Helper()
	if _, err := c.db.Exec(`DELETE FROM backfill WHERE name = ?`, entitiesJobName); err != nil {
		t.Fatal(err)
	}
	if err := c.initEntities(); err != nil {
		t.Fatal(err)
	}
}

func entBody(t *testing.T, c *Cache, rid int64) string {
	t.Helper()
	var n, f string
	if err := c.db.QueryRow(`SELECT body_new, body_full FROM message_fts2 WHERE rowid = ?`, rid).Scan(&n, &f); err != nil {
		t.Fatal(err)
	}
	return n + "\x00" + f
}

func entHits(t *testing.T, c *Cache, phrase string) (int, string) {
	t.Helper()
	res, err := c.SearchV2(context.Background(), SearchOptions{
		SearchQuery: SearchQuery{Text: `"` + phrase + `"`, FTSSyntax: true, Limit: 10}, GroupBy: "message",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) == 0 {
		return 0, ""
	}
	return res.Total, res.Messages[0].Snippet
}

func TestReindexRewritesEscapedRowsAndLeavesCleanOnes(t *testing.T) {
	c := thrOpen(t)
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	esc := html.EscapeString(entPlain)
	stale := entAdd(t, c, "old", true, esc, t0)
	// Candidates by token that carry no reference, and a clean row.
	num := entAdd(t, c, "num", true, "order 39 shipped on 34 pallets", t0.Add(time.Hour))
	clean := entAdd(t, c, "clean", false, "plain widgets, nothing escaped", t0.Add(2*time.Hour))
	numBefore, cleanBefore := entBody(t, c, num), entBody(t, c, clean)

	if n, _ := entHits(t, c, "ul widgets"); n != 0 {
		t.Fatalf("before: the escaped row already matches the decoded phrase")
	}
	if n, sn := entHits(t, c, "ul gt widgets"); n != 1 || !strings.Contains(sn, "&") {
		t.Fatalf("before: fixture is not stale (hits %d, snippet %q)", n, sn)
	}

	restartEntities(t, c)
	st, err := c.runEntities(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rewritten != 1 || st.Candidates < 2 || st.Missing != 0 {
		t.Errorf("first run: %+v, want 1 rewritten, at least 2 candidates", st)
	}
	n, sn := entHits(t, c, "ul widgets")
	if n != 1 {
		t.Fatalf("after: the decoded phrase finds %d messages, want 1", n)
	}
	for _, e := range []string{"&lt;", "&gt;", "&amp;", "&quot;", "&#39;"} {
		if strings.Contains(sn, e) {
			t.Errorf("snippet %q still holds %s", sn, e)
		}
	}
	if want := entBody(t, c, stale); !strings.Contains(want, `"gizmos" it's`) {
		t.Errorf("stored text %q is not decoded", want)
	}
	if entBody(t, c, num) != numBefore || entBody(t, c, clean) != cleanBefore {
		t.Error("a row without a reference was rewritten")
	}
	if s, err := c.EntitiesStatus(context.Background()); err != nil || !s.Complete || s.State != "complete" {
		t.Errorf("status %+v %v", s, err)
	}

	// A second run over the same rows finds nothing to change.
	afterFirst := entBody(t, c, stale)
	restartEntities(t, c)
	st, err = c.runEntities(context.Background(), nil)
	if err != nil || st.Rewritten != 0 {
		t.Errorf("second run: %+v %v, want 0 rewritten", st, err)
	}
	if entBody(t, c, stale) != afterFirst {
		t.Error("second run changed a row")
	}
	// A finished job does nothing, not even a scan.
	if st, err = c.runEntities(context.Background(), nil); err != nil || st != (entityStats{}) {
		t.Errorf("finished job: %+v %v", st, err)
	}
}

func TestReindexCountsMissingBlobsAndSkipsThem(t *testing.T) {
	c := thrOpen(t)
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	esc := html.EscapeString(entPlain)
	gone := entAdd(t, c, "gone", true, esc, t0)
	kept := entAdd(t, c, "kept", true, esc, t0.Add(time.Hour))
	var sum string
	if err := c.db.QueryRow(`SELECT blob_sha256 FROM messages WHERE rowid = ?`, gone).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.BlobPath(sum)); err != nil {
		t.Fatal(err)
	}
	before := entBody(t, c, gone)
	restartEntities(t, c)
	st, err := c.runEntities(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Missing != 1 || st.Rewritten != 1 {
		t.Errorf("stats %+v, want 1 missing and 1 rewritten", st)
	}
	if entBody(t, c, gone) != before {
		t.Error("the row without a blob was changed")
	}
	if strings.Contains(entBody(t, c, kept), "&lt;") {
		t.Error("the readable row was not rewritten")
	}
}

func TestReindexResumesAfterCancelWithoutDoubleWork(t *testing.T) {
	c := thrOpen(t)
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	esc := html.EscapeString(entPlain)
	// The number row comes first and stays a candidate forever: re-examining it
	// after the resume would show up in the candidate count.
	entAdd(t, c, "num", true, "order 39 shipped", t0)
	var rids []int64
	for i := 0; i < 5; i++ {
		rids = append(rids, entAdd(t, c, fmt.Sprint("e", i), true, esc, t0.Add(time.Duration(i+1)*time.Hour)))
	}
	var want int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM message_fts2 WHERE message_fts2 MATCH ?`, entityMatch).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if want != 6 {
		t.Fatalf("fixture: %d candidates, want 6", want)
	}
	restartEntities(t, c)

	oldRows := entitiesBatchRows
	entitiesBatchRows = 2
	ctx, cancel := context.WithCancel(context.Background())
	entitiesHook = cancel
	t.Cleanup(func() { entitiesBatchRows, entitiesHook = oldRows, nil; cancel() })

	first, err := c.runEntities(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v, want a cancel", err)
	}
	if first.Candidates != 2 {
		t.Fatalf("first run examined %d candidates, want one batch of 2", first.Candidates)
	}
	if s, _ := c.EntitiesStatus(context.Background()); s.Complete || s.Done != 2 {
		t.Errorf("status after the cancel: %+v", s)
	}

	entitiesHook = nil
	second, err := c.runEntities(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Candidates + second.Candidates; got != want {
		t.Errorf("candidates over both runs = %d, want %d: rows were examined twice or skipped", got, want)
	}
	if got := first.Rewritten + second.Rewritten; got != len(rids) {
		t.Errorf("rewritten over both runs = %d, want %d", got, len(rids))
	}
	if n, _ := entHits(t, c, "ul widgets"); n != len(rids) {
		t.Errorf("the decoded phrase finds %d of %d", n, len(rids))
	}
	if s, _ := c.EntitiesStatus(context.Background()); !s.Complete {
		t.Errorf("status at the end: %+v", s)
	}
}

func TestReindexNeverBlanksARowAndWritesOnlyIfUnchanged(t *testing.T) {
	c := thrOpen(t)
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	// "&nbsp;" decodes to a no-break space, which trims to an empty body.
	blank := entAdd(t, c, "blank", true, "&nbsp;", t0)
	before := entBody(t, c, blank)
	restartEntities(t, c)
	st, err := c.runEntities(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.SkippedEmpty != 1 || st.Rewritten != 0 || entBody(t, c, blank) != before {
		t.Errorf("stats %+v, row %q -> %q: an emptying rewrite must be skipped", st, before, entBody(t, c, blank))
	}
	// The update is a compare-and-swap on the text that was read.
	rid := entAdd(t, c, "cas", true, html.EscapeString(entPlain), t0.Add(time.Hour))
	cur := entBody(t, c, rid)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	n, err := rewriteBodyTx(context.Background(), tx, rid, "new", "", "changed meanwhile", "")
	if err != nil || n != 0 {
		t.Errorf("stale compare wrote %d rows (%v)", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if entBody(t, c, rid) != cur {
		t.Error("a row that changed since it was read was overwritten")
	}
}
