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
		}
	}()
	client, err := imapx.Dial(ctx, a)
	if err != nil {
		return st, err
	}
	defer func() { _ = client.Logout().Wait(); _ = client.Close() }()
	return c.Refresh(ctx, a, client)
}

// Run refreshes the account now and then every interval until ctx ends. A
// failed refresh is logged and retried on the next tick; it never returns.
// An interval of zero or less does nothing.
func (c *Cache) Run(ctx context.Context, log *slog.Logger, a accounts.Account, interval time.Duration) {
	if interval <= 0 {
		return
	}
	refresh := func() {
		start := time.Now()
		st, err := c.RefreshOnce(ctx, a)
		attrs := []any{
			"account", a.Name, "folders", st.Folders, "new_uids", st.NewUIDs,
			"new_ids", st.NewIDs, "new_bodies", st.NewBodies, "removed", st.Removed,
			"took", time.Since(start).Round(time.Millisecond).String(),
		}
		if err != nil {
			log.Error("cache refresh failed", append(attrs, "error", err.Error())...)
			return
		}
		log.Info("cache refreshed", attrs...)
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
