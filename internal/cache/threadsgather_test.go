package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// gatherComponentRef is gatherComponent as of v0.5.3, kept verbatim as the
// reference: SELECT DISTINCT ... LIMIT in SQL.
func gatherComponentRef(ctx context.Context, tx dbq, account, seed string, visited map[string]bool) ([]threadRow, bool, error) {
	if gatherHook != nil {
		gatherHook()
	}
	set := map[string]threadRow{}
	var order []string
	idsSeen := map[string]bool{}
	queue := []string{seed}
	for len(queue) > 0 && len(set) < maxComponent {
		rows, err := loadThreadRows(ctx, tx, account, queue)
		if err != nil {
			return nil, false, err
		}
		queue = nil
		var added []string
		var extra []string // more stable ids to load, found by subject
		for _, r := range rows {
			if _, ok := set[r.stableID]; ok || r.gm != "" || !r.filled || len(set) >= maxComponent {
				continue
			}
			set[r.stableID] = r
			order = append(order, r.stableID)
			added = append(added, r.stableID)
			visited[r.stableID] = true
		}
		var frontier []string
		for _, part := range chunks(added, idChunk) {
			rs, err := tx.QueryContext(ctx, `SELECT ref_id FROM message_ref WHERE account = ? AND stable_id IN (`+inList(len(part))+`)`, strArgs(account, part)...)
			if err != nil {
				return nil, false, fmt.Errorf("gather thread: %w", err)
			}
			for rs.Next() {
				var id string
				if err := rs.Scan(&id); err != nil {
					_ = rs.Close()
					return nil, false, fmt.Errorf("gather thread: %w", err)
				}
				frontier = append(frontier, id)
			}
			if err := rs.Err(); err != nil {
				_ = rs.Close()
				return nil, false, fmt.Errorf("gather thread: %w", err)
			}
			_ = rs.Close()
		}
		for _, id := range added {
			r := set[id]
			ns := normSubject(r.subject)
			if ns != "" {
				frontier = append(frontier, "subj:"+ns) // header-less replies to this subject
			}
			m := r.msg()
			if len(m.Refs) == 0 && isReplySubject(r.subject) && ns != "" {
				// A header-less reply: the top-level messages it may join.
				roots, err := rootsBySubject(ctx, tx, account, ns, r.date)
				if err != nil {
					return nil, false, err
				}
				extra = append(extra, roots...)
			}
		}
		var fresh []string
		for _, id := range frontier {
			if !idsSeen[id] {
				idsSeen[id] = true
				fresh = append(fresh, id)
			}
		}
		for _, part := range chunks(fresh, idChunk) {
			rs, err := tx.QueryContext(ctx, `SELECT DISTINCT stable_id FROM message_ref WHERE account = ? AND ref_id IN (`+inList(len(part))+`) LIMIT ?`,
				append(strArgs(account, part), maxComponent+1)...)
			if err != nil {
				return nil, false, fmt.Errorf("gather thread: %w", err)
			}
			for rs.Next() {
				var sid string
				if err := rs.Scan(&sid); err != nil {
					_ = rs.Close()
					return nil, false, fmt.Errorf("gather thread: %w", err)
				}
				if _, ok := set[sid]; !ok {
					queue = append(queue, sid)
				}
			}
			if err := rs.Err(); err != nil {
				_ = rs.Close()
				return nil, false, fmt.Errorf("gather thread: %w", err)
			}
			_ = rs.Close()
		}
		for _, sid := range extra {
			if _, ok := set[sid]; !ok {
				queue = append(queue, sid)
			}
		}
	}
	out := make([]threadRow, 0, len(order))
	for _, id := range order {
		out = append(out, set[id])
	}
	return out, len(set) >= maxComponent, nil
}

// The reworked id lookup must gather exactly the component the SELECT DISTINCT
// ... LIMIT query did, for every seed, with and without the size cap biting.
func TestGatherComponentMatchesReference(t *testing.T) {
	ctx := context.Background()
	c := thrOpen(t)
	loadSynth(t, c, "p", synthCorpus(1200, 7))
	for _, limit := range []int{3000, 10} {
		old := maxComponent
		maxComponent = limit
		func() {
			defer func() { maxComponent = old }()
			capped := 0
			for i := 0; i < 1200; i += 7 {
				seed := fmt.Sprintf("pm:%07d", i)
				v1, v2 := map[string]bool{}, map[string]bool{}
				got, gc, err1 := gatherComponent(ctx, c.db, "p", seed, v1)
				want, wc, err2 := gatherComponentRef(ctx, c.db, "p", seed, v2)
				if err1 != nil || err2 != nil {
					t.Fatal(err1, err2)
				}
				if gc != wc || len(got) != len(want) {
					t.Fatalf("limit %d seed %s: capped %v/%v, size %d/%d", limit, seed, gc, wc, len(got), len(want))
				}
				for k := range got {
					if got[k].stableID != want[k].stableID {
						t.Fatalf("limit %d seed %s: member %d is %s, want %s", limit, seed, k, got[k].stableID, want[k].stableID)
					}
				}
				if gc {
					capped++
				}
			}
			if limit < 100 && capped == 0 {
				t.Fatalf("limit %d: the cap never bit; the test checks nothing", limit)
			}
		}()
	}
}

// The id lookup and the thread aggregate must search by their indexes: with no
// table statistics SQLite otherwise walks a whole account per component (SQU-448).
func TestThreadQueriesUseIndexes(t *testing.T) {
	c := thrOpen(t)
	loadSynth(t, c, "p", synthCorpus(500, 3))
	plan := func(q string, args ...any) string {
		rs, err := c.db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out string
		for rs.Next() {
			var a, b, cc int
			var d string
			_ = rs.Scan(&a, &b, &cc, &d)
			out += d + "\n"
		}
		return out
	}
	if p := plan(refMembersSQL, "p", "a", 11); !strings.Contains(p, "USING PRIMARY KEY (account=? AND ref_id=?)") || strings.Contains(p, "TEMP B-TREE") || strings.Contains(p, "message_ref_by_msg") {
		t.Fatalf("per-id lookup is not an ordered primary-key search:\n%s", p)
	}
	agg := plan(`SELECT m.stable_id FROM message_thread t INDEXED BY message_thread_by_tid JOIN messages m ON m.account = t.account AND m.stable_id = t.stable_id WHERE t.account = ? AND t.tid = ?`, "p", "x")
	if !strings.Contains(agg, "message_thread_by_tid (account=? AND tid=?)") {
		t.Fatalf("thread aggregate does not search by tid:\n%s", agg)
	}
}

// One id shared by thousands of messages (a "subj:" key of automated mail)
// must gather the same component as the old query, reading at most
// maxComponent+1 rows of it.
func TestGatherHotRefMatchesReference(t *testing.T) {
	ctx := context.Background()
	c := thrOpen(t)
	var ms []synthMsg
	for i := 0; i < 5000; i++ {
		ms = append(ms, synthMsg{id: fmt.Sprintf("pm:%07d", i), msgID: fmt.Sprintf("h%d@x", i), irt: "hot@x", refs: []string{"hot@x"},
			subject: "Re: hot", from: "a@x.example", to: "me@example.com", arrival: int64(1000 + i)})
	}
	loadSynth(t, c, "p", ms)
	old := maxComponent
	maxComponent = 10
	defer func() { maxComponent = old }()
	for _, seed := range []string{"pm:0000000", "pm:0002500", "pm:0004999"} {
		got, gc, err1 := gatherComponent(ctx, c.db, "p", seed, map[string]bool{})
		want, wc, err2 := gatherComponentRef(ctx, c.db, "p", seed, map[string]bool{})
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if !gc || !wc || len(got) != len(want) {
			t.Fatalf("seed %s: capped %v/%v, size %d/%d", seed, gc, wc, len(got), len(want))
		}
		for k := range got {
			if got[k].stableID != want[k].stableID {
				t.Fatalf("seed %s: member %d is %s, want %s", seed, k, got[k].stableID, want[k].stableID)
			}
		}
	}
}
