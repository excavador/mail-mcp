package cache

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// perfCorpus bulk-loads n messages from amazon senders, each in 2-3 folders,
// all matching "order", with threads, straight into the tables (no blobs).
func perfCorpus(t *testing.T, c *Cache, n int) {
	t.Helper()
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mi, _ := tx.Prepare(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, list_id) VALUES ('P', ?, 'x', ?, ?, ?, '')`)
	fi, _ := tx.Prepare(`INSERT INTO message_fts2 (rowid, subject, from_addr, to_addr, cc_addr, body_new, body_full, account, stable_id) VALUES (?, ?, ?, '', '', 'your order has shipped', '', 'P', ?)`)
	ms, _ := tx.Prepare(`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('P', ?, ?, ?, 1)`)
	mt, _ := tx.Prepare(`INSERT INTO message_thread (account, stable_id, tid) VALUES ('P', ?, ?)`)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("p:%d", i)
		from := fmt.Sprintf("Amazon <ship-%d@amazon.nl>", i%50)
		subj := fmt.Sprintf("Your order %d", i)
		res, err := mi.Exec(id, from, subj, base+int64(i)*600)
		if err != nil {
			t.Fatal(err)
		}
		rid, _ := res.LastInsertId()
		if _, err := fi.Exec(rid, subj, from, id); err != nil {
			t.Fatal(err)
		}
		for k, f := range []string{"INBOX", "Archive", "All Mail"}[:2+i%2] {
			if _, err := ms.Exec(id, f, i+1, k); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := mt.Exec(id, fmt.Sprintf("t%d", i/2)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	c.fts2Ready.Store(true)
}

// TestSearchBroadMatchStaysInBudget: a query that matches tens of thousands of
// messages must answer within the search budget, with folders looked up for
// the returned page only.
func TestSearchBroadMatchStaysInBudget(t *testing.T) {
	n := 20000
	if v := os.Getenv("PERF_N"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	if testing.Short() {
		n = 2000
	}
	c := thrOpen(t)
	perfCorpus(t, c, n)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		q    SearchQuery
	}{
		{"text", SearchQuery{Text: "order", Limit: 20}},
		{"from", SearchQuery{From: "amazon", Limit: 20}},
		{"text+from", SearchQuery{Text: "order", From: "amazon", Limit: 20}},
	} {
		for _, group := range []string{"message", "thread"} {
			t.Run(tc.name+"/"+group, func(t *testing.T) {
				before := c.FolderQueries()
				start := time.Now()
				r, err := c.SearchV2(ctx, SearchOptions{SearchQuery: tc.q, GroupBy: group, Facets: []string{"sender", "domain", "month"}})
				el := time.Since(start)
				if err != nil {
					t.Fatalf("after %s: %v", el, err)
				}
				t.Logf("%s/%s: %s total=%d note=%q folderQueries=%d", tc.name, group, el, r.Total, r.FacetNote, c.FolderQueries()-before)
				if got := c.FolderQueries() - before; got > 2 {
					t.Errorf("folder lookups = %d, want one batch for the page", got)
				}
				if el > 2*time.Second {
					t.Errorf("took %s, want well under the %s budget", el, searchTimeout)
				}
				if group == "message" {
					if len(r.Messages) != 20 || len(r.Messages[0].Folders) < 2 {
						t.Errorf("page: %d messages, folders %v", len(r.Messages), r.Messages[0].Folders)
					}
				}
			})
		}
	}
}
