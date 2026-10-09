package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
)

// refreshWait is how long refresh_cache waits for the refresh before it
// answers in_progress and leaves the refresh running. A variable for tests.
var refreshWait = 2 * time.Minute

const defaultRefreshMaxAge = 5 * time.Minute

type refreshIn struct {
	Account string `json:"account" jsonschema:"account name as in list_accounts (the account's email/username or an owner alias is also accepted)"`
	MaxAge  string `json:"max_age,omitempty" jsonschema:"Go duration such as 30s, 5m, 1h: refresh only if the last successful refresh is older than this. Default 5m, minimum 30s, maximum 24h."`
}

type refreshOut struct {
	Account string `json:"account"`
	// Refreshed is true, false, or "in_progress".
	Refreshed any    `json:"refreshed"`
	Reason    string `json:"reason,omitempty"`
	// MaxAge is the max_age applied, after clamping.
	MaxAge             string     `json:"max_age"`
	MaxAgeClamped      string     `json:"max_age_note,omitempty"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	Duration           string     `json:"duration,omitempty"`
	FoldersScanned     *int       `json:"folders_scanned,omitempty"`
	FoldersSkipped     *int       `json:"folders_skipped_unchanged,omitempty"`
	NewUIDs            *int       `json:"new_uids,omitempty"`
	NewMessages        *int       `json:"new_messages,omitempty"`
	LastRefresh        *time.Time `json:"last_refresh,omitempty"`
	LastRefreshOK      bool       `json:"last_refresh_ok"`
	JoinedRunningCycle bool       `json:"joined_running_refresh,omitempty"`
	Error              string     `json:"error,omitempty"`
}

// resolveAccount maps what a caller typed to an account: the name, else the
// login (email/username), else an exact owner alias; case-insensitive. Domain
// patterns ("@example.com") are not addresses and never match.
func resolveAccount(accts []accounts.Account, key string) (accounts.Account, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return accounts.Account{}, fmt.Errorf("account is required")
	}
	for _, pass := range []func(accounts.Account) []string{
		func(a accounts.Account) []string { return []string{a.Name} },
		func(a accounts.Account) []string { return []string{a.Username} },
		func(a accounts.Account) []string { return a.Aliases },
	} {
		var hit []accounts.Account
		for _, a := range accts {
			for _, n := range pass(a) {
				if strings.ToLower(strings.TrimSpace(n)) == key && !strings.HasPrefix(key, "@") {
					hit = append(hit, a)
					break
				}
			}
		}
		switch len(hit) {
		case 0:
		case 1:
			return hit[0], nil
		default:
			return accounts.Account{}, fmt.Errorf("%q matches more than one account; use the account name from list_accounts", key)
		}
	}
	return accounts.Account{}, fmt.Errorf("unknown account %q; see list_accounts", key)
}

// parseMaxAge returns the max_age to apply and a note when it was clamped.
func parseMaxAge(s string) (time.Duration, string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultRefreshMaxAge, "", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, "", fmt.Errorf("max_age %q is not a duration like 30s, 5m or 1h", s)
	}
	switch {
	case d < cache.MinRefreshAge:
		return cache.MinRefreshAge, fmt.Sprintf("max_age %s is below the 30s floor; clamped to 30s", s), nil
	case d > cache.MaxRefreshAge:
		return cache.MaxRefreshAge, fmt.Sprintf("max_age %s is above the 24h cap; clamped to 24h", s), nil
	}
	return d, "", nil
}

// addRefreshCache registers refresh_cache on both endpoints. It dials the
// mailbox read-only (LIST, STATUS, SELECT, FETCH) and writes only the local
// cache; it never changes the mailbox.
func addRefreshCache(s *mcp.Server, accts []accounts.Account, store *cache.Cache) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "refresh_cache",
		Description: "Refresh the local message cache of one account now, instead of waiting for the background refresh " +
			"(about every 15 minutes). Reads the mailbox over IMAP and writes only the local cache; it never changes the mailbox. " +
			"Runs only if the last successful refresh is older than max_age (default 5m, floor 30s, cap 24h), otherwise answers " +
			"refreshed:false, reason:\"fresh\". Folders unchanged since the last scan are skipped. Waits up to 2 minutes, then answers " +
			"refreshed:\"in_progress\" and the refresh finishes in the background (see cache_status). At most one refresh of an " +
			"account runs at a time, and at most one on-demand refresh per 30 seconds.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in refreshIn) (*mcp.CallToolResult, any, error) {
		a, err := resolveAccount(accts, in.Account)
		if err != nil {
			return nil, nil, err
		}
		maxAge, note, err := parseMaxAge(in.MaxAge)
		if err != nil {
			return nil, nil, err
		}
		o, err := store.RefreshNow(ctx, slog.Default(), a, maxAge, refreshWait)
		if err != nil {
			return nil, nil, fail("refresh_cache", "refresh failed", err, "account", a.Name)
		}
		out := refreshOut{Account: a.Name, MaxAge: maxAge.String(), MaxAgeClamped: note, LastRefresh: o.LastRefresh, LastRefreshOK: o.LastOK, JoinedRunningCycle: o.Joined}
		switch o.State {
		case cache.StateFresh:
			out.Refreshed, out.Reason = false, "fresh"
		case cache.StateInProgress:
			out.Refreshed, out.Reason = "in_progress", "still running; it finishes in the background, see cache_status"
		case cache.StateRateLimited:
			out.Refreshed, out.Reason = false, "rate_limited: an on-demand refresh ran less than 30s ago; this is its result"
		default:
			out.Refreshed = true
		}
		if r := o.Run; r != nil {
			st, fin, dur := r.StartedAt.UTC(), r.FinishedAt.UTC(), r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond).String()
			out.StartedAt, out.FinishedAt, out.Duration = &st, &fin, dur
			out.FoldersScanned, out.FoldersSkipped = &r.Stats.FoldersScanned, &r.Stats.FoldersSkipped
			out.NewUIDs, out.NewMessages = &r.Stats.NewUIDs, &r.Stats.NewIDs
			out.Error = r.ErrText
			out.LastRefreshOK = r.Err == nil
		}
		slog.Info("refresh_cache", "account", a.Name, "max_age", maxAge.String(), "refreshed", out.Refreshed,
			"state", o.State, "joined", o.Joined, "duration", out.Duration, "new_ids", derefInt(out.NewMessages))
		return nil, out, nil
	})
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
