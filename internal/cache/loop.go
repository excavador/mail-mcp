package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

// RefreshOnce dials the account, refreshes its cache and logs out. A fresh
// connection per refresh, not a held one: Gmail and Bridge both drop idle
// sessions, and a refresh every quarter hour is not worth the reconnect logic
// a long-lived session would need. A panic inside the IMAP library is turned
// into an error, because one account's bad day must not take the process --
// and the other account's refresh -- down with it.
func (c *Cache) RefreshOnce(ctx context.Context, a accounts.Account) (st Stats, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: refresh panicked: %v", a.Name, r)
			c.recordRefresh(a.Name, st, false)
		}
	}()
	client, err := imapx.Dial(ctx, a)
	if err != nil {
		// Refresh never ran, so it recorded nothing: note the failure here,
		// or an account that cannot connect would be invisible to Status.
		c.recordRefresh(a.Name, Stats{}, false)
		return st, err
	}
	// imapclient's calls take no context, so a server that stalls mid-FETCH
	// would block a read forever. Closing the connection when ctx ends
	// (cancelled, or the per-refresh deadline) makes the blocked read return.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-done:
		}
	}()
	defer func() {
		if ctx.Err() == nil {
			_ = client.Logout().Wait()
		}
		_ = client.Close()
	}()
	return c.Refresh(ctx, a, client)
}

// Run refreshes the account now and then every interval until ctx ends. A
// failed refresh is logged and retried on the next tick; it never returns.
// An interval of zero or less does nothing.
func (c *Cache) Run(ctx context.Context, log *slog.Logger, a accounts.Account, interval time.Duration) {
	if interval <= 0 {
		return
	}
	// A refresh is cancelled if no batch completes within max(interval, 10m)
	// (a hang is bounded and the next tick starts clean), but one that keeps
	// making progress may run on, up to refreshCeiling: the first fill of a
	// large mailbox takes hours and must not be cut off and restarted.
	stall := max(interval, 10*time.Minute)
	// The run goes through the per-account guard shared with refresh_cache
	// (RefreshNow): if an on-demand refresh of this account is in flight when
	// the tick fires, the tick joins it instead of starting a second one. The
	// ticker is not reset: the next tick still comes at its usual time.
	refresh := func() {
		call := c.startRefresh(ctx, a.Name, func(rctx context.Context) (Stats, error) {
			return c.runLogged(rctx, log, a, stall)
		})
		select {
		case <-call.done:
		case <-ctx.Done():
		}
	}

	refresh()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// runLogged is one refresh of a: the stall/ceiling bounds, progress logging
// and the outcome log line shared by the background loop and refresh_cache.
func (c *Cache) runLogged(ctx context.Context, log *slog.Logger, a accounts.Account, stall time.Duration) (Stats, error) {
	start := time.Now()
	rctx, touch, cancel := withStallTimeout(ctx, stall, refreshCeiling)
	defer cancel()
	logProgress := progressLogger(log, a.Name, progressLogEvery)
	rctx = WithProgress(rctx, func(p Progress) {
		touch()
		logProgress(p)
	})
	st, err := c.RefreshOnce(rctx, a)
	attrs := []any{
		"account", a.Name, "folders", st.Folders, "folders_total", st.FoldersTotal,
		"folders_skipped_unchanged", st.FoldersSkipped, "folders_scanned", st.FoldersScanned, "new_uids", st.NewUIDs,
		"new_ids", st.NewIDs, "new_bodies", st.NewBodies, "removed", st.Removed, "skipped", st.Skipped,
		"took", time.Since(start).Round(time.Millisecond).String(),
	}
	if err != nil {
		if cause := context.Cause(rctx); cause != nil && rctx.Err() != nil {
			attrs = append(attrs, "cause", cause.Error())
		}
		log.Error("cache refresh failed", append(attrs, "error", err.Error())...)
		return st, err
	}
	log.Info("cache refreshed", attrs...)
	return st, nil
}
