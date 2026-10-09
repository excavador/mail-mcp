package cache

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

const (
	// MinRefreshAge and MaxRefreshAge bound the max_age of RefreshNow.
	MinRefreshAge = 30 * time.Second
	MaxRefreshAge = 24 * time.Hour
	// onDemandGap is the least time between the starts of two on-demand
	// refreshes of one account in one process.
	onDemandGap = 30 * time.Second
	// onDemandStall is the stall window of a refresh started on demand.
	onDemandStall = 10 * time.Minute
)

// RefreshRun is the outcome of one finished refresh of one account.
type RefreshRun struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Stats      Stats
	// Err is the raw error; it may name hosts or users, so callers show
	// ErrText, never this.
	Err error
	// ErrText is Err reduced to a fixed phrase, safe to show a client.
	ErrText string
}

// refreshCall is one refresh in flight; done closes after run is set.
type refreshCall struct {
	done chan struct{}
	run  RefreshRun
}

// acctRefresh is the per-account guard: at most one refresh of an account
// runs in this process, whoever asked for it.
type acctRefresh struct {
	inflight *refreshCall
	last     *RefreshRun
	lastOD   time.Time // start of the last on-demand refresh
}

func (c *Cache) acct(name string) *acctRefresh {
	if c.rf == nil {
		c.rf = map[string]*acctRefresh{}
	}
	r := c.rf[name]
	if r == nil {
		r = &acctRefresh{}
		c.rf[name] = r
	}
	return r
}

// startRefresh runs do for account unless a refresh of it is already in
// flight, in which case it returns that one (singleflight). The refresh runs
// in its own goroutine under a context derived from parent, so a caller that
// stops waiting does not cancel it. Close waits for it.
func (c *Cache) startRefresh(parent context.Context, account string, do func(context.Context) (Stats, error)) *refreshCall {
	c.rfMu.Lock()
	defer c.rfMu.Unlock()
	r := c.acct(account)
	if r.inflight != nil {
		return r.inflight
	}
	call := &refreshCall{done: make(chan struct{})}
	r.inflight = call
	c.bgWG.Add(1)
	go func() {
		defer c.bgWG.Done()
		call.run.StartedAt = time.Now()
		st, err := safeDo(parent, do)
		call.run.Stats, call.run.Err = st, err
		call.run.FinishedAt = time.Now()
		call.run.ErrText = SanitizeRefreshErr(err)
		c.rfMu.Lock()
		r.inflight = nil
		run := call.run
		r.last = &run
		c.rfMu.Unlock()
		close(call.done)
	}()
	return call
}

func safeDo(ctx context.Context, do func(context.Context) (Stats, error)) (st Stats, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("refresh panicked")
		}
	}()
	return do(ctx)
}

// SanitizeRefreshErr reduces a refresh error to a fixed phrase: raw IMAP and
// SQLite errors carry hosts, usernames and paths a tool result must not.
func SanitizeRefreshErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, imapx.ErrLogin):
		return "login failed"
	case errors.Is(err, imapx.ErrTimeout):
		return "mail server did not answer in time"
	case errors.Is(err, ErrStalled):
		return "refresh stalled: no progress"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "refresh cancelled or ran out of time"
	default:
		return "refresh failed (see server log)"
	}
}

// Outcome states of RefreshNow.
const (
	StateRefreshed   = "refreshed"
	StateFresh       = "fresh"
	StateInProgress  = "in_progress"
	StateRateLimited = "rate_limited"
)

// RefreshOutcome is what RefreshNow reports.
type RefreshOutcome struct {
	State string
	// LastRefresh is the cache's last refresh time (success or not) and
	// LastOK whether it succeeded, read after the call.
	LastRefresh *time.Time
	LastOK      bool
	// Run is the refresh that finished; nil for fresh and in_progress.
	Run *RefreshRun
	// Joined: the call waited on a refresh somebody else started.
	Joined bool
}

// lastRefresh reads the refreshes row of one account.
func (c *Cache) lastRefresh(ctx context.Context, account string) (at time.Time, ok bool, found bool, err error) {
	var ts, o int64
	err = c.db.QueryRowContext(ctx, `SELECT at, ok FROM refreshes WHERE account = ?`, account).Scan(&ts, &o)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, false, nil
	}
	if err != nil {
		return time.Time{}, false, false, err
	}
	return time.Unix(ts, 0).UTC(), o == 1, true, nil
}

// RefreshNow refreshes the cache of account on demand, for refresh_cache.
//
// Coordination. Every refresh of an account in this process, the background
// loop's (Run) and this one, goes through startRefresh, a per-account
// singleflight. Called while the leader's loop is mid-refresh, RefreshNow
// joins that run and returns its result; called when the loop's tick fires
// mid-refresh, the loop joins this one. The loop's ticker is left alone, so
// its next scheduled run still happens.
//
// Standby pods. RefreshNow does not check the Lease: on a pod that is not the
// leader there is no loop, so it simply runs a one-shot refresh under the same
// guard. That is safe against the leader's refresh in another process because
// the cache DB is shared through SQLite in WAL mode with a busy_timeout and
// immediate write transactions: two writers queue for the write lock instead
// of failing, and a refresh is idempotent (it keys on folder/UID and stable
// id), so the worst case is some duplicated IMAP reads.
//
// Rate limit: at most one on-demand start per account per 30s per process; a
// call inside the window gets the last run's result (State rate_limited), or
// joins the run in flight.
//
// The refresh is not tied to ctx: if wait elapses first, State is in_progress
// and the refresh finishes in the background.
func (c *Cache) RefreshNow(ctx context.Context, log *slog.Logger, a accounts.Account, maxAge, wait time.Duration) (RefreshOutcome, error) {
	maxAge = min(max(maxAge, MinRefreshAge), MaxRefreshAge)
	var out RefreshOutcome
	finish := func() RefreshOutcome {
		if at, ok, found, err := c.lastRefresh(ctx, a.Name); err == nil && found {
			out.LastRefresh, out.LastOK = &at, ok
		}
		return out
	}

	if at, ok, found, err := c.lastRefresh(ctx, a.Name); err != nil {
		return out, err
	} else if found && ok && c.now().Sub(at) < maxAge {
		out.State = StateFresh
		return finish(), nil
	}

	c.rfMu.Lock()
	r := c.acct(a.Name)
	var call *refreshCall
	switch {
	case r.inflight != nil:
		call, out.Joined = r.inflight, true
	case r.last != nil && time.Since(r.lastOD) < onDemandGap:
		run := *r.last
		c.rfMu.Unlock()
		out.State, out.Run = StateRateLimited, &run
		return finish(), nil
	}
	c.rfMu.Unlock()

	if call == nil {
		call = c.startRefresh(c.bgCtx, a.Name, func(rctx context.Context) (Stats, error) {
			return c.runLogged(rctx, log, a, onDemandStall)
		})
		c.rfMu.Lock()
		r.lastOD = time.Now()
		c.rfMu.Unlock()
	}

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-call.done:
		run := call.run
		out.State, out.Run = StateRefreshed, &run
	case <-t.C:
		out.State = StateInProgress
	case <-ctx.Done():
		out.State = StateInProgress
	}
	return finish(), nil
}
