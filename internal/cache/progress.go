package cache

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// refreshCeiling is the absolute upper bound on one refresh, however
	// much progress it keeps making: the first fill of a very large mailbox
	// may take hours, but not forever.
	refreshCeiling = 6 * time.Hour
	// progressLogEvery is the most often a long fill logs its progress.
	progressLogEvery = 60 * time.Second
)

// ErrStalled is the cancellation cause of a refresh that made no progress
// within its stall window.
var ErrStalled = errors.New("refresh stalled: no batch completed within the stall window")

// Progress is one report from a running refresh.
type Progress struct {
	Folder string
	// Done and Total count the folder's new UIDs resolved so far, and all
	// that this refresh set out to resolve.
	Done, Total int
	// NewBodies is bodies stored in this folder so far.
	NewBodies int
}

type progressKey struct{}

// WithProgress returns a context whose refreshes call fn after every batch
// (and once when a folder's scan starts). fn runs on the refresh goroutine,
// so it must be quick.
func WithProgress(ctx context.Context, fn func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// progressFrom returns the reporter in ctx, or a no-op.
func progressFrom(ctx context.Context) func(Progress) {
	if fn, ok := ctx.Value(progressKey{}).(func(Progress)); ok {
		return fn
	}
	return func(Progress) {}
}

// withStallTimeout returns a context that is cancelled (with cause ErrStalled)
// only if touch is not called for stall, and unconditionally after ceiling.
// Every call to touch pushes the stall deadline out again, so a refresh that
// keeps completing batches is never cut off by the stall timer. The returned
// cancel must be called to release the timers.
func withStallTimeout(parent context.Context, stall, ceiling time.Duration) (ctx context.Context, touch func(), cancel context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	ceil := time.AfterFunc(ceiling, func() { cancelCause(context.DeadlineExceeded) })
	t := time.AfterFunc(stall, func() { cancelCause(ErrStalled) })
	touch = func() { t.Reset(stall) }
	return ctx, touch, func() {
		t.Stop()
		ceil.Stop()
		cancelCause(context.Canceled)
	}
}

// progressLogger returns a reporter that logs at INFO at most once per
// progressLogEvery. It is used from one goroutine only.
func progressLogger(log *slog.Logger, account string, every time.Duration) func(Progress) {
	var last time.Time
	return func(p Progress) {
		if p.Total == 0 || time.Since(last) < every {
			return
		}
		last = time.Now()
		log.Info("cache fill progress", "account", account, "folder", p.Folder,
			"done_uids", p.Done, "total_uids", p.Total, "new_bodies", p.NewBodies)
	}
}
