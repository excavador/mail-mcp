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

// Backfill pacing. A batch ends at backfillBatch messages or backfillSlice of
// wall time, whichever comes first: the batch holds the database write lock
// (refresh waits on it), and a PDF-heavy stretch must not hold it for minutes.
// Between batches the worker sleeps as long as the batch took, floored at
// backfillMinYield, so it uses at most about half of the write capacity.
const (
	backfillBatch    = 500
	backfillSlice    = 750 * time.Millisecond
	backfillMinYield = 50 * time.Millisecond
	backfillLogEvery = time.Minute
	backfillMaxHeld  = 32 << 20 // parsed text held between parse and write
)

// BackfillStatus is the progress of the message_fts2 backfill.
type BackfillStatus struct {
	Done     int  `json:"done"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
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

// FTSTable names the full-text table search should use and whether it is the
// new one. It is message_fts2 once the backfill has indexed every message that
// existed before it, otherwise message_fts (the original, still written).
func (c *Cache) FTSTable() (table string, ready bool) {
	if c.fts2Ready.Load() {
		return "message_fts2", true
	}
	return "message_fts", false
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
	start := time.Now()
	lastLog := start
	var startProcessed int
	if st, err := c.BackfillStatus(ctx); err == nil {
		startProcessed = st.Done
		log.Info("fts2 backfill starting", "done", st.Done, "total", st.Total)
	}
	missing := 0
	for {
		if ctx.Err() != nil {
			return
		}
		began := time.Now()
		complete, miss, err := c.backfillBatch(ctx)
		missing += miss
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
		if time.Since(lastLog) >= backfillLogEvery {
			lastLog = time.Now()
			st, _ := c.BackfillStatus(ctx)
			rate := float64(st.Done-startProcessed) / max(time.Since(start).Seconds(), 1)
			log.Info("fts2 backfill progress", "done", st.Done, "total", st.Total, "per_second", int(rate), "missing_blobs", missing)
		}
		if !sleepCtx(ctx, max(time.Since(began), backfillMinYield)) {
			return
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
	// time or backfillMaxHeld bytes of parsed text, so memory stays bounded.
	// Phase 2 holds the write lock only for the inserts.
	type item struct {
		rid         int64
		account, id string
		p           parsed
	}
	var items []item
	began := time.Now()
	held, n, lastDone := 0, 0, last
	for _, r := range batch {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		var one int
		err := c.db.QueryRowContext(ctx, `SELECT 1 FROM message_fts2 WHERE rowid = ?`, r.rid).Scan(&one)
		switch {
		case err == nil:
			// already indexed (a retry after a partial failure)
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
			held += len(p.Body) + len(p.BodyNew)
			for _, a := range p.Atts {
				held += len(a.Text)
			}
			items = append(items, item{r.rid, r.account, r.id, p})
		default:
			return false, 0, fmt.Errorf("check fts2 row: %w", err)
		}
		n++
		lastDone = r.rid
		if time.Since(began) >= backfillSlice || held >= backfillMaxHeld {
			break
		}
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range items {
		it := &items[i]
		if err := indexText2Tx(ctx, tx, it.rid, it.account, it.id, it.p); err != nil {
			return false, 0, err
		}
		it.p = parsed{}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE backfill SET last_rowid = ?, processed = processed + ?, updated_at = ? WHERE name = ?`,
		lastDone, n, c.now().Unix(), backfillName); err != nil {
		return false, 0, fmt.Errorf("record backfill progress: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit: %w", err)
	}
	return false, missing, nil
}
