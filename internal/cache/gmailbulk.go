package cache

// The Gmail bulk pass. A Gmail message already names its thread
// (gm_thread_id), so its place needs no JWZ, no blob read and no message_ref
// rows: "g:"+gm_thread_id, depth 0, no parent. The per-message threads
// backfill spends ~80ms of blob IO and planning on each of them for nothing,
// so the scheduler runs this set-based pass first and the backfill then skips
// the rows it covers.
//
// message_id, in_reply_to and references_json stay NULL for these rows, and
// no message_ref rows are written: those exist only so JWZ can link a late
// parent to its children, and threadGmail/planThreads never consult them for
// a message with a gm_thread_id. Nothing else reads them (get_thread shows
// the Message-ID parsed from the blob).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// gmailChunkRows is how many messages rowids one insert transaction covers.
const gmailChunkRows = 4000

// BulkThreadGmail threads every message that has a gm_thread_id and no
// message_thread row, then refreshes the threads rows (and outsider flags) of
// the Gmail threads whose row is missing or stale. It is idempotent and
// resumable: a run cut short finds the rest on the next run. It returns how
// many messages it threaded.
func (c *Cache) BulkThreadGmail(ctx context.Context, log *slog.Logger) (int, error) {
	start := c.now()
	total, err := c.bulkGmailAssign(ctx, log)
	if err != nil {
		return total, err
	}
	nt, err := c.bulkGmailAggregates(ctx, log)
	if err != nil {
		return total, err
	}
	if log != nil && (total > 0 || nt > 0) {
		log.Info("gmail bulk threading done", "messages", total, "threads", nt,
			"took", c.now().Sub(start).Round(time.Millisecond).String(), "max_write_hold", c.MaxBackfillHold().String())
	}
	return total, nil
}

// bulkGmailAssign inserts the message_thread rows, one rowid range per
// transaction. The threads job's processed counter is raised in the same
// transaction (when that job is under way) so its done/total stay true.
func (c *Cache) bulkGmailAssign(ctx context.Context, log *slog.Logger) (int, error) {
	var lo, hi sql.NullInt64
	if err := c.db.QueryRowContext(ctx, `SELECT MIN(rowid), MAX(rowid) FROM messages WHERE gm_thread_id <> ''`).Scan(&lo, &hi); err != nil {
		return 0, fmt.Errorf("gmail bulk: bounds: %w", err)
	}
	if !lo.Valid {
		return 0, nil
	}
	total := 0
	for from := lo.Int64 - 1; from < hi.Int64; from += gmailChunkRows {
		to := min(from+gmailChunkRows, hi.Int64)
		var n int
		var hold time.Duration
		err := c.retryBusy(ctx, log, "gmail bulk threading", func() error {
			tx, err := c.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("gmail bulk: begin: %w", err)
			}
			defer func() { _ = tx.Rollback() }()
			began := time.Now()
			res, err := tx.ExecContext(ctx, `
INSERT INTO message_thread (account, stable_id, tid, parent_stable_id, depth)
SELECT m.account, m.stable_id, 'g:' || m.gm_thread_id, '', 0 FROM messages m
WHERE m.rowid > ? AND m.rowid <= ? AND m.gm_thread_id <> ''
  AND NOT EXISTS (SELECT 1 FROM message_thread t WHERE t.account = m.account AND t.stable_id = m.stable_id)
ORDER BY m.account, m.stable_id`, from, to)
			if err != nil {
				return fmt.Errorf("gmail bulk: insert: %w", err)
			}
			k, _ := res.RowsAffected()
			if k > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE backfill SET processed = processed + ? WHERE name = ? AND done = 0`, k, threadBackfillName); err != nil {
					return fmt.Errorf("gmail bulk: progress: %w", err)
				}
			}
			if hold, err = c.commitHeld(tx, began); err != nil {
				return err
			}
			n = int(k)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if !c.yield(ctx, hold, backfillPause) {
			return total, ctx.Err()
		}
	}
	return total, nil
}

type gmailTid struct{ account, tid string }

// bulkGmailAggregates refreshes the threads row of every Gmail thread whose
// row is missing or whose n_msgs differs from its member count.
func (c *Cache) bulkGmailAggregates(ctx context.Context, log *slog.Logger) (int, error) {
	rs, err := c.db.QueryContext(ctx, `
SELECT g.account, g.tid FROM
  (SELECT account, tid, COUNT(*) AS n FROM message_thread WHERE tid >= 'g:' AND tid < 'g;' GROUP BY account, tid) g
LEFT JOIN threads t ON t.account = g.account AND t.tid = g.tid
WHERE t.tid IS NULL OR t.n_msgs <> g.n ORDER BY g.account, g.tid`)
	if err != nil {
		return 0, fmt.Errorf("gmail bulk: stale threads: %w", err)
	}
	byAcct := map[string][]string{}
	var accts []string
	seen := map[gmailTid]bool{} // each thread is refreshed at most once per run
	for rs.Next() {
		var a, t string
		if err := rs.Scan(&a, &t); err != nil {
			_ = rs.Close()
			return 0, fmt.Errorf("gmail bulk: stale threads: %w", err)
		}
		if seen[gmailTid{a, t}] {
			continue
		}
		seen[gmailTid{a, t}] = true
		if _, ok := byAcct[a]; !ok {
			accts = append(accts, a)
		}
		byAcct[a] = append(byAcct[a], t)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return 0, err
	}
	_ = rs.Close()

	done := 0
	size := 200 // threads per transaction, adapted to keep a hold near writeSlice
	for _, a := range accts {
		owners := c.threadOpts(a).Owners
		tids := byAcct[a]
		for i := 0; i < len(tids); {
			j := min(i+size, len(tids))
			var hold time.Duration
			err := c.retryBusy(ctx, log, "gmail bulk threading", func() error {
				tx, err := c.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("gmail bulk: begin: %w", err)
				}
				defer func() { _ = tx.Rollback() }()
				began := time.Now()
				if err := refreshGmailThreads(ctx, tx, a, tids[i:j], owners); err != nil {
					return err
				}
				hold, err = c.commitHeld(tx, began)
				return err
			})
			if err != nil {
				return done, err
			}
			if j <= i { // no progress: never loop
				return done, fmt.Errorf("gmail bulk: no progress")
			}
			done += j - i
			i = j
			switch {
			case hold > writeSlice:
				size = max(size/2, 10)
			case hold < writeSlice/4:
				size = min(size*2, 1000)
			}
			if !c.yield(ctx, hold, backfillPause) {
				return done, ctx.Err()
			}
		}
	}
	return done, nil
}

// refreshGmailThreads is refreshAggregates for "g:" threads, set-wise: one
// query reads the members of all tids ordered by tid and arrival, and the
// threads rows and outsider flags are written from that. Time and root follow
// arrival; every member sits at depth 0, so the root is the earliest. An
// outsider is, as in JWZ threads, a sender (not the owner) who is not the
// From, To or Cc of any earlier message of the thread.
func refreshGmailThreads(ctx context.Context, tx *sql.Tx, account string, tids []string, owners []string) error {
	own := map[string]bool{}
	for _, o := range owners {
		own[o] = true
	}
	for _, part := range chunks(tids, idChunk) {
		rows, err := tx.QueryContext(ctx, `
SELECT t.tid, m.stable_id, m.from_addr, m.to_addr, m.cc_addr, m.subject, `+arrivalCol+`
FROM message_thread t JOIN messages m ON m.account = t.account AND m.stable_id = t.stable_id
WHERE t.account = ? AND t.tid IN (`+inList(len(part))+`) ORDER BY t.tid, `+arrivalCol+`, m.stable_id`, strArgs(account, part)...)
		if err != nil {
			return fmt.Errorf("gmail aggregate: %w", err)
		}
		type agg struct {
			n           int
			first, last int64
			root, subj  string
			people      []string
			havePeople  map[string]bool
			spoke       map[string]bool
			outsiders   []string
		}
		aggs := map[string]*agg{}
		for rows.Next() {
			var tid, sid, from, to, cc, subj string
			var d int64
			if err := rows.Scan(&tid, &sid, &from, &to, &cc, &subj, &d); err != nil {
				_ = rows.Close()
				return fmt.Errorf("gmail aggregate: %w", err)
			}
			a := aggs[tid]
			if a == nil {
				a = &agg{first: d, root: sid, subj: subj, havePeople: map[string]bool{}, spoke: map[string]bool{}}
				aggs[tid] = a
			}
			a.last = max(a.last, d)
			if name := personName(from); name != "" && !a.havePeople[name] && len(a.people) < maxParticipants {
				a.havePeople[name] = true
				a.people = append(a.people, name)
			}
			sender := strings.ToLower(BareAddr(from))
			if a.n > 0 && sender != "" && !a.spoke[sender] && !own[sender] {
				a.outsiders = append(a.outsiders, sid)
			}
			if sender != "" {
				a.spoke[sender] = true
			}
			for _, r := range recipientAddrs(to, cc) {
				a.spoke[r] = true
			}
			a.n++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("gmail aggregate: %w", err)
		}
		_ = rows.Close()

		if _, err := tx.ExecContext(ctx, `UPDATE message_thread SET outsider = 0 WHERE account = ? AND outsider = 1 AND tid IN (`+inList(len(part))+`)`,
			strArgs(account, part)...); err != nil {
			return fmt.Errorf("gmail aggregate: %w", err)
		}
		for _, tid := range part {
			a := aggs[tid]
			if a == nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE account = ? AND tid = ?`, account, tid); err != nil {
					return fmt.Errorf("gmail aggregate: %w", err)
				}
				continue
			}
			pj, _ := json.Marshal(a.people)
			if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO threads (account, tid, root_stable_id, subject_norm, first_at, last_at, n_msgs, participants_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, account, tid, a.root, normSubject(a.subj), a.first, a.last, a.n, string(pj)); err != nil {
				return fmt.Errorf("gmail aggregate: %w", err)
			}
			for _, sid := range a.outsiders {
				if _, err := tx.ExecContext(ctx, `UPDATE message_thread SET outsider = 1 WHERE account = ? AND stable_id = ?`, account, sid); err != nil {
					return fmt.Errorf("gmail aggregate: %w", err)
				}
			}
		}
	}
	return nil
}
