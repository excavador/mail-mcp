package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// backfillName names the message_fts2 job in the backfill table.
const backfillName = "fts2"

// Backfill pacing. The batch is listed and parsed with no transaction, for at
// most backfillSlice of wall time; the results are then written in
// transactions of at most writeRows rows or writeSlice of work (scheduler.go),
// with a rest between them, so refresh and live tools get the write lock.
const (
	backfillBatch    = 500
	backfillSlice    = 200 * time.Millisecond
	backfillLogEvery = time.Minute
)

// backfillMinYield is the least rest between write transactions.
var backfillMinYield = 50 * time.Millisecond

// BackfillStatus is the progress of the message_fts2 backfill.
type BackfillStatus struct {
	Done     int  `json:"done"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
	// State is "complete", "running", or "pending" (not started: the jobs run
	// one after another: fts2, threads, senders, fts2 entities).
	State string `json:"state"`
}

func jobStateName(complete bool, j *jobState) string {
	switch {
	case complete:
		return "complete"
	case j.running.Load():
		return "running"
	}
	return "pending"
}

// initBackfill creates the job row on first start: it snapshots the highest
// messages rowid and the message count, because messages that arrive later
// are written to message_fts2 by refresh itself. It runs in Open, before any
// refresh can insert, so the snapshot is exact. On a database with no
// messages the job is complete immediately.
func (c *Cache) initBackfill() error {
	now := c.now().Unix()
	if _, err := c.db.Exec(`
INSERT OR IGNORE INTO backfill (name, last_rowid, done, updated_at, max_rowid, total, processed)
SELECT ?, 0, CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END, ?, COALESCE(MAX(rowid), 0), COUNT(*), 0 FROM messages`,
		backfillName, now); err != nil {
		return fmt.Errorf("init backfill: %w", err)
	}
	var done int
	if err := c.db.QueryRow(`SELECT done FROM backfill WHERE name = ?`, backfillName).Scan(&done); err != nil {
		return fmt.Errorf("init backfill: %w", err)
	}
	c.fts2Ready.Store(done == 1)
	return nil
}

// BackfillStatus reports the progress of the message_fts2 backfill.
func (c *Cache) BackfillStatus(ctx context.Context) (BackfillStatus, error) {
	var (
		st   BackfillStatus
		done int
	)
	err := c.db.QueryRowContext(ctx, `SELECT processed, total, done FROM backfill WHERE name = ?`, backfillName).Scan(&st.Done, &st.Total, &done)
	if err != nil {
		return st, fmt.Errorf("cache: backfill status: %w", err)
	}
	st.Complete = done == 1
	if st.Complete {
		st.Done = st.Total
	}
	st.State = jobStateName(st.Complete, &c.fts2Job)
	return st, nil
}

// indexText2Tx writes the message_fts2 row and the attachment rows of one
// message inside tx. rid is the messages rowid.
func indexText2Tx(ctx context.Context, tx *sql.Tx, rid int64, account, stableID string, p parsed) error {
	full := p.Body
	if full == p.BodyNew {
		full = "" // the same words are already in body_new
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO message_fts2 (rowid, subject, from_addr, to_addr, cc_addr, body_new, body_full, account, stable_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rid, p.Subject, p.From, p.To, p.Cc, p.BodyNew, full, account, stableID); err != nil {
		return fmt.Errorf("index text: %w", err)
	}
	return insertAttachmentsTx(ctx, tx, account, stableID, p.Atts)
}

// RunBackfill indexes, into message_fts2 and attachments, every message that
// existed when the job was created, re-parsing each from its blob on disk. It
// never touches IMAP. It is resumable (progress commits with each batch),
// yields between batches, and returns when the job is complete or ctx ends.
// A message is parsed and written one at a time, so memory stays at one
// message plus its PDFs regardless of batch size.
func (c *Cache) RunBackfill(ctx context.Context, log *slog.Logger) {
	if c.fts2Ready.Load() {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	c.fts2Job.running.Store(true)
	defer c.fts2Job.running.Store(false)
	start := time.Now()
	var rl *rateLog
	if st, err := c.BackfillStatus(ctx); err == nil {
		rl = newRateLog("fts2 backfill", log, st.Done)
		log.Info("fts2 backfill starting", "done", st.Done, "total", st.Total)
	} else {
		rl = newRateLog("fts2 backfill", log, 0)
	}
	missing := 0
	for {
		if ctx.Err() != nil {
			return
		}
		var complete bool
		err := c.retryBusy(ctx, log, "fts2 backfill", func() error {
			var miss int
			var err error
			complete, miss, err = c.backfillBatch(ctx)
			missing += miss
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("fts2 backfill batch failed; retrying", "error", err.Error())
			if !sleepCtx(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if complete {
			c.fts2Ready.Store(true)
			st, _ := c.BackfillStatus(ctx)
			log.Info("fts2 backfill complete", "total", st.Total, "missing_blobs", missing, "took", time.Since(start).Round(time.Second).String())
			return
		}
		if time.Since(rl.last) >= backfillLogEvery {
			st, _ := c.BackfillStatus(ctx)
			rl.tick(st.Done, st.Total)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// backfillBatch processes one batch in one transaction. It reports whether
// the job is now complete and how many messages had no readable blob.
func (c *Cache) backfillBatch(ctx context.Context) (complete bool, missing int, err error) {
	var last, maxRow int64
	if err := c.db.QueryRowContext(ctx, `SELECT last_rowid, max_rowid FROM backfill WHERE name = ?`, backfillName).Scan(&last, &maxRow); err != nil {
		return false, 0, fmt.Errorf("read backfill: %w", err)
	}
	type row struct {
		rid               int64
		account, id, blob string
		from, to, cc, sub string
	}
	// The ids of one batch are listed first, then each message is handled
	// alone: the slice stays small and no cursor is held open across the
	// (slow) parsing.
	rows, err := c.db.QueryContext(ctx, `
SELECT rowid, account, stable_id, blob_sha256, from_addr, to_addr, cc_addr, subject
FROM messages WHERE rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?`, last, maxRow, backfillBatch)
	if err != nil {
		return false, 0, fmt.Errorf("list messages: %w", err)
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rid, &r.account, &r.id, &r.blob, &r.from, &r.to, &r.cc, &r.sub); err != nil {
			_ = rows.Close()
			return false, 0, fmt.Errorf("list messages: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, 0, fmt.Errorf("list messages: %w", err)
	}
	_ = rows.Close()

	if len(batch) == 0 {
		_, err := c.db.ExecContext(ctx, `UPDATE backfill SET done = 1, updated_at = ? WHERE name = ?`, c.now().Unix(), backfillName)
		return err == nil, 0, err
	}

	// Phase 1, no transaction: skip rows already indexed, read and parse the
	// rest (PDF extraction can take seconds). It ends at backfillSlice of wall
	// time or maxHeldParsed bytes of parsed text, so memory stays bounded.
	// Phase 2 writes the results in short transactions.
	type item struct {
		rid         int64
		account, id string
		skip        bool
		p           parsed
	}
	var items []item
	began := time.Now()
	held := 0
	for _, r := range batch {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		var one int
		err := c.db.QueryRowContext(ctx, `SELECT 1 FROM message_fts2 WHERE rowid = ?`, r.rid).Scan(&one)
		switch {
		case err == nil:
			// already indexed (a retry after a partial failure)
			items = append(items, item{rid: r.rid, skip: true})
		case errors.Is(err, sql.ErrNoRows):
			p := parsed{From: r.from, To: r.to, Cc: r.cc, Subject: r.sub}
			if path := c.BlobPath(r.blob); path != "" {
				if raw, rerr := os.ReadFile(path); rerr == nil {
					p = parseMessage(raw)
				} else {
					missing++
				}
			} else {
				missing++
			}
			held += parsedSize(p)
			items = append(items, item{r.rid, r.account, r.id, false, p})
		default:
			return false, 0, fmt.Errorf("check fts2 row: %w", err)
		}
		if time.Since(began) >= backfillSlice || held >= maxHeldParsed {
			break
		}
	}
	for i := 0; i < len(items); {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return false, 0, fmt.Errorf("begin: %w", err)
		}
		locked := time.Now()
		j := i
		for j < len(items) && (j == i || (j-i < writeRows && time.Since(locked) < writeSlice)) {
			it := &items[j]
			if !it.skip {
				if err := indexText2Tx(ctx, tx, it.rid, it.account, it.id, it.p); err != nil {
					_ = tx.Rollback()
					return false, 0, err
				}
				it.p = parsed{}
			}
			j++
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE backfill SET last_rowid = ?, processed = processed + ?, updated_at = ? WHERE name = ?`,
			items[j-1].rid, j-i, c.now().Unix(), backfillName); err != nil {
			_ = tx.Rollback()
			return false, 0, fmt.Errorf("record backfill progress: %w", err)
		}
		hold, err := c.commitHeld(tx, locked)
		if err != nil {
			return false, 0, err
		}
		i = j
		if !c.yield(ctx, hold, backfillMinYield) {
			return false, missing, ctx.Err()
		}
	}
	return false, missing, nil
}

// maxHeldParsed caps the parsed text held between parsing and writing, in
// refresh batches and in the backfill alike.
const maxHeldParsed = 32 << 20

// parsedSize is the approximate memory a parsed message holds.
func parsedSize(p parsed) int {
	n := len(p.Body) + len(p.BodyNew) + len(p.Subject) + len(p.From) + len(p.To) + len(p.Cc)
	for _, a := range p.Atts {
		n += len(a.Filename) + len(a.Mime) + 128
	}
	return n
}
