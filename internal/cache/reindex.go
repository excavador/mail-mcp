package cache

// The fts2_entities job: message_fts2 rows indexed before decodeEntities
// existed hold HTML-escaped text/plain ("&lt;ul&gt;", "&amp;", "&quot;",
// "&#39;"), so a search for the decoded words misses them and the snippets
// show entities. The job finds those rows, re-extracts each from its blob with
// the current extractor and rewrites the row only when the text changed. It
// never touches IMAP, runs last (after senders) and is resumable.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// entitiesJobName names the job in the backfill table.
const entitiesJobName = "fts2_entities"

// entityMatch finds the rows that may hold escaped text. The unicode61 tokenizer
// turns "&lt;" into the token lt, "&amp;" into amp, "&quot;" into quot,
// "&#39;" into 39 and "&#x27;" into x27, so an FTS5 MATCH on those tokens, in
// the two body columns only, lists the candidates from the index without
// reading a blob or the messages table. Numeric references share tokens with
// ordinary numbers ("39", "34"); that costs a few false candidates, which
// entityRE on the stored text dismisses before any blob is read.
const entityMatch = `{body_new body_full} : (lt OR gt OR amp OR quot OR apos OR nbsp OR 34 OR 39 OR x22 OR x27)`

// entitiesBatchRows is how many candidates one batch examines (a test lowers it).
var entitiesBatchRows = writeRows

// entitiesHook, when set (tests), runs after every batch.
var entitiesHook func()

// entityStats is what a run of the job did.
type entityStats struct {
	Candidates int // rows the MATCH listed
	Rewritten  int // rows whose text changed
	Missing    int // candidates whose blob could not be read
}

// EntitiesStatus is the progress of the fts2_entities job, like BackfillStatus.
// Done counts candidates examined, Total the messages the job started with, so
// Done is only a lower bound of progress until the job completes.
func (c *Cache) EntitiesStatus(ctx context.Context) (BackfillStatus, error) {
	var (
		st   BackfillStatus
		done int
	)
	err := c.db.QueryRowContext(ctx, `SELECT processed, total, done FROM backfill WHERE name = ?`, entitiesJobName).Scan(&st.Done, &st.Total, &done)
	if err != nil {
		return st, fmt.Errorf("cache: entities status: %w", err)
	}
	st.Complete = done == 1
	if st.Complete {
		st.Done = st.Total
	}
	st.State = jobStateName(st.Complete, &c.entitiesJob)
	return st, nil
}

// initEntities creates the job row on first start, snapshotting the highest
// messages rowid: rows indexed later were written by the current extractor.
// On a database with no messages the job is complete immediately.
func (c *Cache) initEntities() error {
	if _, err := c.db.Exec(`
INSERT OR IGNORE INTO backfill (name, last_rowid, done, updated_at, max_rowid, total, processed)
SELECT ?, 0, CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END, ?, COALESCE(MAX(rowid), 0), COUNT(*), 0 FROM messages`,
		entitiesJobName, c.now().Unix()); err != nil {
		return fmt.Errorf("init entities: %w", err)
	}
	return nil
}

// RunEntities runs the fts2_entities job to completion. It returns when the
// job is done or ctx ends; a later call resumes after the last examined row.
func (c *Cache) RunEntities(ctx context.Context, log *slog.Logger) error {
	_, err := c.runEntities(ctx, log)
	return err
}

func (c *Cache) runEntities(ctx context.Context, log *slog.Logger) (entityStats, error) {
	var total entityStats
	if log == nil {
		log = slog.Default()
	}
	var done int
	if err := c.db.QueryRowContext(ctx, `SELECT done FROM backfill WHERE name = ?`, entitiesJobName).Scan(&done); err != nil {
		return total, fmt.Errorf("entities state: %w", err)
	}
	if done == 1 {
		return total, nil
	}
	c.entitiesJob.running.Store(true)
	defer c.entitiesJob.running.Store(false)
	start := time.Now()
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		var (
			finished bool
			st       entityStats
			held     time.Duration
		)
		err := c.retryBusy(ctx, log, "fts2 entities", func() error {
			var err error
			finished, st, held, err = c.entitiesBatch(ctx)
			return err
		})
		total.Candidates += st.Candidates
		total.Rewritten += st.Rewritten
		total.Missing += st.Missing
		if err != nil {
			return total, err
		}
		if finished {
			log.Info("fts2 entities done", "candidates", total.Candidates, "rewritten", total.Rewritten, "missing", total.Missing,
				"took", time.Since(start).Round(time.Millisecond).String())
			return total, nil
		}
		if entitiesHook != nil {
			entitiesHook()
		}
		if !c.yield(ctx, held, backfillMinYield) {
			return total, ctx.Err()
		}
	}
}

// entitiesBatch examines the next candidates after last_rowid and writes their
// rewrites with the new position. Candidates are listed and re-extracted with
// no transaction, for at most backfillSlice of wall time or maxHeldParsed
// bytes; the writes then go in transactions of at most writeRows rows or
// writeSlice of work, each recording its own position, so a cancel or a crash
// loses nothing and repeats nothing.
func (c *Cache) entitiesBatch(ctx context.Context) (finished bool, st entityStats, held time.Duration, err error) {
	var last, maxRow int64
	if err := c.db.QueryRowContext(ctx, `SELECT last_rowid, max_rowid FROM backfill WHERE name = ?`, entitiesJobName).Scan(&last, &maxRow); err != nil {
		return false, st, 0, fmt.Errorf("read entities state: %w", err)
	}
	type item struct {
		rid          int64
		body, bodyNo string
		write        bool
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT message_fts2.rowid, message_fts2.body_new, message_fts2.body_full, m.blob_sha256
FROM message_fts2 JOIN messages m ON m.rowid = message_fts2.rowid
WHERE message_fts2 MATCH ? AND message_fts2.rowid > ? AND message_fts2.rowid <= ?
ORDER BY message_fts2.rowid LIMIT ?`, entityMatch, last, maxRow, entitiesBatchRows)
	if err != nil {
		return false, st, 0, fmt.Errorf("list entity candidates: %w", err)
	}
	var items []item
	began := time.Now()
	heldBytes := 0
	for rows.Next() {
		var (
			rid                 int64
			bodyNew, full, blob string
		)
		if err := rows.Scan(&rid, &bodyNew, &full, &blob); err != nil {
			_ = rows.Close()
			return false, st, 0, fmt.Errorf("list entity candidates: %w", err)
		}
		st.Candidates++
		it := item{rid: rid}
		// A false candidate (a bare number, say) has no reference in its text.
		if entityRE.MatchString(bodyNew) || entityRE.MatchString(full) {
			var (
				raw  []byte
				rerr error
			)
			if path := c.BlobPath(blob); path != "" {
				raw, rerr = os.ReadFile(path)
			}
			if raw == nil || rerr != nil {
				st.Missing++
			} else if b, bn, ok := parseBody(raw); ok {
				if b == bn {
					b = "" // the same words are already in body_new
				}
				if bn != bodyNew || b != full {
					it.body, it.bodyNo, it.write = bn, b, true
					heldBytes += len(bn) + len(b)
				}
			}
		}
		items = append(items, it)
		if time.Since(began) >= backfillSlice || heldBytes >= maxHeldParsed {
			break
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, st, 0, fmt.Errorf("list entity candidates: %w", err)
	}
	if len(items) == 0 {
		_, err := c.db.ExecContext(ctx, `UPDATE backfill SET done = 1, updated_at = ? WHERE name = ?`, c.now().Unix(), entitiesJobName)
		return err == nil, st, 0, err
	}
	for i := 0; i < len(items); {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return false, st, held, fmt.Errorf("begin: %w", err)
		}
		locked := time.Now()
		j, wrote := i, 0
		for j < len(items) && (j == i || (j-i < writeRows && time.Since(locked) < writeSlice)) {
			if it := items[j]; it.write {
				if err := rewriteBodyTx(ctx, tx, it.rid, it.body, it.bodyNo); err != nil {
					_ = tx.Rollback()
					return false, st, held, err
				}
				wrote++
			}
			j++
		}
		if err := setBackfillTx(ctx, tx, entitiesJobName, items[j-1].rid, false, c.now(), j-i); err != nil {
			_ = tx.Rollback()
			return false, st, held, fmt.Errorf("record entities progress: %w", err)
		}
		if held, err = c.commitHeld(tx, locked); err != nil {
			return false, st, held, err
		}
		st.Rewritten += wrote
		i = j
	}
	return false, st, held, nil
}

// rewriteBodyTx replaces the two body columns of a message_fts2 row.
func rewriteBodyTx(ctx context.Context, tx *sql.Tx, rid int64, bodyNew, bodyFull string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE message_fts2 SET body_new = ?, body_full = ? WHERE rowid = ?`, bodyNew, bodyFull, rid); err != nil {
		return fmt.Errorf("rewrite entities: %w", err)
	}
	return nil
}
