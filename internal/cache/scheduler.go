package cache

// The background jobs (fts2 index, threads) share one writer with refresh and
// every live tool: SQLite has a single writer, and a busy_timeout wait is
// dead time for whoever waits. So the jobs run one after another from one
// goroutine, each write transaction is short (bounded by rows and wall
// time, all reads and parsing done before it), the job rests between
// transactions, and it stands aside while a refresh or an apply is writing.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// writeSlice is the most wall time one backfill write transaction works
	// before it commits and rests; writeRows is its row cap.
	writeSlice = 200 * time.Millisecond
	writeRows  = 100

	busyMin       = 200 * time.Millisecond
	busyMax       = 2 * time.Second
	busyWarnAfter = 2 * time.Minute

	// fgWaitMax bounds how long a backfill stands aside for foreground
	// writers, so a long refresh cannot starve it for good.
	fgWaitMax = 15 * time.Second
)

// jobState is the in-process state of a backfill job.
type jobState struct {
	running atomic.Bool
}

// Busy and hold counters, for logs and for measurement.
type writeStats struct {
	busy    atomic.Int64 // SQLITE_BUSY seen by backfill batches
	maxHold atomic.Int64 // longest backfill write transaction, ns
}

// MaxBackfillHold is the longest write transaction a backfill has held.
func (c *Cache) MaxBackfillHold() time.Duration { return time.Duration(c.ws.maxHold.Load()) }

// BackfillBusyCount is how many SQLITE_BUSY errors backfill batches met.
func (c *Cache) BackfillBusyCount() int64 { return c.ws.busy.Load() }

func (c *Cache) noteHold(d time.Duration) {
	for {
		old := c.ws.maxHold.Load()
		if int64(d) <= old || c.ws.maxHold.CompareAndSwap(old, int64(d)) {
			return
		}
	}
}

// foreground marks the start of a refresh or an apply, which the backfills
// give way to. Call the returned func when it ends.
func (c *Cache) foreground() func() {
	c.fgWriters.Add(1)
	return func() { c.fgWriters.Add(-1) }
}

// commitHeld commits tx and records how long it held the write lock since
// began (BEGIN IMMEDIATE has returned by then, so that is the hold time).
func (c *Cache) commitHeld(tx *sql.Tx, began time.Time) (time.Duration, error) {
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	d := time.Since(began)
	c.noteHold(d)
	return d, nil
}

// yield rests between write transactions: it waits while a foreground
// writer is active (up to fgWaitMax), then sleeps long enough for others to
// take the lock, half the time the last transaction held it, at least min.
// It reports false if ctx ended.
func (c *Cache) yield(ctx context.Context, held, min time.Duration) bool {
	deadline := time.Now().Add(fgWaitMax)
	for c.fgWriters.Load() > 0 && time.Now().Before(deadline) {
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
	}
	d := max(min, held/2)
	if d <= 0 {
		return ctx.Err() == nil
	}
	return sleepCtx(ctx, d)
}

// isBusy reports whether err is SQLite's "database is locked".
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// retryBusy runs fn, retrying after a short jittered backoff (200ms doubling
// to 2s) while it fails with SQLITE_BUSY. BUSY is logged at debug with a
// counter, and at warn only once it has persisted for busyWarnAfter. Any
// other error, success, or the end of ctx returns.
func (c *Cache) retryBusy(ctx context.Context, log *slog.Logger, job string, fn func() error) error {
	if log == nil {
		log = slog.Default()
	}
	var first, lastWarn time.Time
	wait := busyMin
	for {
		err := fn()
		if !isBusy(err) || ctx.Err() != nil {
			return err
		}
		n := c.ws.busy.Add(1)
		now := time.Now()
		if first.IsZero() {
			first, lastWarn = now, now
		}
		log.Debug(job+" write busy; retrying", "busy_total", n, "backoff", wait.String(), "error", err.Error())
		if now.Sub(lastWarn) >= busyWarnAfter {
			lastWarn = now
			log.Warn(job+" write still busy", "for", now.Sub(first).Round(time.Second).String(), "busy_total", n)
		}
		if !sleepCtx(ctx, wait/2+rand.N(wait/2+1)) {
			return err
		}
		wait = min(wait*2, busyMax)
	}
}

// rateLog logs a job's throughput and ETA once a minute.
type rateLog struct {
	job         string
	log         *slog.Logger
	start, last time.Time
	startDone   int
	lastDone    int
}

func newRateLog(job string, log *slog.Logger, done int) *rateLog {
	now := time.Now()
	return &rateLog{job: job, log: log, start: now, last: now, startDone: done, lastDone: done}
}

// tick logs if a minute passed since the last line.
func (r *rateLog) tick(done, total int) {
	now := time.Now()
	if now.Sub(r.last) < backfillLogEvery {
		return
	}
	rate := float64(done-r.lastDone) / max(now.Sub(r.last).Seconds(), 1)
	r.last, r.lastDone = now, done
	eta := "unknown"
	if rate > 0 && total > done {
		eta = (time.Duration(float64(total-done)/rate) * time.Second).Round(time.Second).String()
	}
	r.log.Info(r.job+" progress", "done", done, "total", total, "msg_per_s", fmt.Sprintf("%.1f", rate),
		"avg_msg_per_s", fmt.Sprintf("%.1f", float64(done-r.startDone)/max(now.Sub(r.start).Seconds(), 1)), "eta", eta)
}

// RunBackfills is the one backfill scheduler: the fts2 index first (search
// quality depends on it), then Gmail bulk threading and the per-message threads job,
// then senders, then the fts2 entity re-index, then PDF text (when a sidecar is configured), each to completion and
// resumable. It returns when all are done or ctx ends. Writers other than these jobs
// (refresh, apply) are not queued behind it; the jobs yield to them.
func (c *Cache) RunBackfills(ctx context.Context, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("fts2 backfill panicked", "panic", fmt.Sprint(r))
			}
		}()
		c.RunBackfill(ctx, log)
	}()
	if ctx.Err() != nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("thread backfill panicked", "panic", fmt.Sprint(r))
			}
		}()
		if _, err := c.BulkThreadGmail(ctx, log); err != nil && ctx.Err() == nil {
			log.Error("gmail bulk threading failed; the per-message backfill covers the rest", "error", err.Error())
		}
		if ctx.Err() != nil {
			return
		}
		if err := c.BackfillThreads(ctx, log); err != nil && ctx.Err() == nil {
			log.Error("thread backfill failed", "error", err.Error())
		}
	}()
	if ctx.Err() != nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("senders job panicked", "panic", fmt.Sprint(r))
			}
		}()
		if err := c.RunSenders(ctx, log); err != nil && ctx.Err() == nil {
			log.Error("senders job failed", "error", err.Error())
		}
	}()
	if ctx.Err() != nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("fts2 entities job panicked", "panic", fmt.Sprint(r))
			}
		}()
		if err := c.RunEntities(ctx, log); err != nil && ctx.Err() == nil {
			log.Error("fts2 entities job failed", "error", err.Error())
		}
	}()
	if ctx.Err() != nil {
		return
	}
	// Last: PDF text through the sidecar is the slowest and least urgent job,
	// and it only runs when an extractor is configured. It then keeps
	// rescanning for new mail's PDFs until ctx ends.
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("pdf_text job panicked", "panic", fmt.Sprint(r))
			}
		}()
		c.RunPDFText(ctx, log)
	}()
}
