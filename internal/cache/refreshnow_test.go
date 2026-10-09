package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

var discardLog = slog.New(slog.DiscardHandler)

const rfDeadline = 20 * time.Second

// waitParked blocks until n goroutines are parked in a select inside a frame
// matching frame (e.g. "Cache).RefreshNow"), so a test knows its callers have
// reached the join point without sleeping for a guessed time.
func waitParked(t *testing.T, frame string, n int) {
	t.Helper()
	deadline := time.Now().Add(rfDeadline)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		stacks := string(buf[:runtime.Stack(buf, true)])
		got := 0
		for _, g := range strings.Split(stacks, "\n\n") {
			if strings.Contains(g, "[select") && strings.Contains(g, frame) {
				got++
			}
		}
		if got >= n {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d goroutines parked in %s", n, frame)
}

func openRFCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// stub is a controllable refresh function.
type stub struct {
	calls   atomic.Int64
	started chan struct{} // closed on first call
	release chan struct{} // close to let calls finish
	once    sync.Once
	st      Stats
	err     error
}

func newStub() *stub {
	return &stub{started: make(chan struct{}), release: make(chan struct{})}
}

func (s *stub) do(ctx context.Context) (Stats, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return Stats{}, ctx.Err()
	}
	return s.st, s.err
}

func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(rfDeadline):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// blackhole is a TCP server that accepts and never answers, so a refresh
// dialling it blocks until its context ends.
func blackhole(t *testing.T) (accounts.Account, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acc := make(chan struct{}, 64)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			acc <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	return accounts.Account{Name: "bh", Host: "127.0.0.1", Port: port, TLS: accounts.Implicit, Username: "u"}, acc
}

func setRefreshRow(t *testing.T, c *Cache, account string, at time.Time, ok int) {
	t.Helper()
	_, err := c.db.Exec(`INSERT OR REPLACE INTO refreshes (account, at, ok, folders, new_uids, new_ids, new_bodies, removed) VALUES (?, ?, ?, 0, 0, 0, 0, 0)`,
		account, at.Unix(), ok)
	if err != nil {
		t.Fatal(err)
	}
}

func refreshRows(t *testing.T, c *Cache, account string) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM refreshes WHERE account = ?`, account).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 1. fresh / stale / ok=0 / no row.
func TestRefreshNowFreshStaleFailedNoRow(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	c := e.cache
	name := e.acct.Name
	imapLines := func() int { return len(e.log.lines()) }

	t.Run("fresh makes no IMAP call", func(t *testing.T) {
		setRefreshRow(t, c, name, time.Now().Add(-time.Minute), 1)
		before := imapLines()
		out, err := c.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != StateFresh || out.Run != nil || out.Joined || !out.LastOK || out.LastRefresh == nil {
			t.Fatalf("outcome = %+v", out)
		}
		if imapLines() != before {
			t.Fatalf("fresh answer touched IMAP: %v", e.log.lines()[before:])
		}
		if _, ok := c.rf[name]; ok && c.rf[name].inflight != nil {
			t.Fatal("fresh answer started a refresh")
		}
	})

	t.Run("max_age floor", func(t *testing.T) {
		// A max_age under the 30s floor is clamped: 20s old is still fresh
		// for a 1ns request.
		setRefreshRow(t, c, name, time.Now().Add(-20*time.Second), 1)
		out, err := c.RefreshNow(e.ctx(), discardLog, e.acct, time.Nanosecond, time.Second)
		if err != nil || out.State != StateFresh {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})

	t.Run("ok=0 is never fresh", func(t *testing.T) {
		setRefreshRow(t, c, name, time.Now(), 0)
		out, err := c.RefreshNow(e.ctx(), discardLog, e.acct, time.Hour, 15*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != StateRefreshed || out.Run == nil || out.Run.Err != nil || !out.LastOK {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("stale refreshes", func(t *testing.T) {
		c.rfMu.Lock()
		c.acct(name).lastOD = time.Time{} // lift the on-demand gap of the previous case
		c.rfMu.Unlock()
		setRefreshRow(t, c, name, time.Now().Add(-10*time.Minute), 1)
		out, err := c.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, 15*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != StateRefreshed || out.Run == nil || out.Run.Err != nil || out.Joined {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("no row refreshes", func(t *testing.T) {
		if _, err := c.db.Exec(`DELETE FROM refreshes WHERE account = ?`, name); err != nil {
			t.Fatal(err)
		}
		c.rfMu.Lock()
		c.acct(name).lastOD = time.Time{}
		c.rfMu.Unlock()
		out, err := c.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, 15*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != StateRefreshed || out.Run == nil || out.Run.Err != nil {
			t.Fatalf("outcome = %+v", out)
		}
		if refreshRows(t, c, name) != 1 {
			t.Fatal("no refreshes row after the run")
		}
	})
}

// 6. standby: no Run loop, RefreshNow runs once and records the row.
func TestRefreshNowStandbyRunsOnceAndRecords(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("<a@x>", "hello", "body"), time.Now())
	if refreshRows(t, e.cache, e.acct.Name) != 0 {
		t.Fatal("row before any refresh")
	}
	out, err := e.cache.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != StateRefreshed || out.Run == nil || out.Run.Err != nil || out.Joined {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Run.Stats.NewIDs != 1 {
		t.Fatalf("stats = %+v, want one new message", out.Run.Stats)
	}
	if refreshRows(t, e.cache, e.acct.Name) != 1 || out.LastRefresh == nil || !out.LastOK {
		t.Fatalf("refreshes row not recorded: %+v", out)
	}
	// A second call is answered from the row, without a second run.
	out2, err := e.cache.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, time.Second)
	if err != nil || out2.State != StateFresh {
		t.Fatalf("second call: %+v err=%v", out2, err)
	}
}

// 8. rate limit.
func TestRefreshNowRateLimit(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	c := e.cache
	first, err := c.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, 15*time.Second)
	if err != nil || first.State != StateRefreshed || first.Run == nil {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	lines := len(e.log.lines())
	// Make the row look stale so only the rate limit stands in the way.
	c.now = func() time.Time { return time.Now().Add(time.Hour) }

	second, err := c.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != StateRateLimited || second.Run == nil || second.Joined {
		t.Fatalf("second: %+v", second)
	}
	if !second.Run.StartedAt.Equal(first.Run.StartedAt) || !second.Run.FinishedAt.Equal(first.Run.FinishedAt) {
		t.Fatalf("rate-limited result is not the first run's: %+v vs %+v", second.Run, first.Run)
	}
	if got := len(e.log.lines()); got != lines {
		t.Fatalf("rate-limited call touched IMAP (%d -> %d lines)", lines, got)
	}

	// Outside the window the next call runs again.
	c.rfMu.Lock()
	c.acct(e.acct.Name).lastOD = time.Now().Add(-onDemandGap - time.Second)
	c.rfMu.Unlock()
	third, err := c.RefreshNow(e.ctx(), discardLog, e.acct, 5*time.Minute, 15*time.Second)
	if err != nil || third.State != StateRefreshed || third.Run == nil {
		t.Fatalf("third: %+v err=%v", third, err)
	}
	if !third.Run.StartedAt.After(first.Run.StartedAt) {
		t.Fatalf("third did not run anew")
	}
}

func TestRefreshNowInFlightJoinsInsteadOfRateLimiting(t *testing.T) {
	c := openRFCache(t)
	a := accounts.Account{Name: "acct"}
	// A recent on-demand run: the gap is open.
	c.rfMu.Lock()
	r := c.acct(a.Name)
	r.last = &RefreshRun{StartedAt: time.Now()}
	r.lastOD = time.Now()
	c.rfMu.Unlock()

	s := newStub()
	call := c.startRefresh(c.bgCtx, a.Name, s.do)
	recv(t, s.started, "stub start")
	out, err := c.RefreshNow(context.Background(), discardLog, a, 5*time.Minute, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != StateInProgress || !out.Joined || out.Run != nil {
		t.Fatalf("outcome = %+v, want a joined in_progress", out)
	}
	close(s.release)
	recv(t, call.done, "stub done")
	if s.calls.Load() != 1 {
		t.Fatalf("stub ran %d times", s.calls.Load())
	}
}

// 4. N concurrent callers share one run.
func TestStartRefreshSingleflight(t *testing.T) {
	c := openRFCache(t)
	s := newStub()
	const n = 16
	calls := make([]*refreshCall, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			calls[i] = c.startRefresh(c.bgCtx, "acct", s.do)
		}()
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if calls[i] != calls[0] {
			t.Fatalf("caller %d got a different call", i)
		}
	}
	recv(t, s.started, "stub start")
	close(s.release)
	recv(t, calls[0].done, "done")
	if got := s.calls.Load(); got != 1 {
		t.Fatalf("stub ran %d times, want 1", got)
	}
	// After it finished, the next request starts a new run.
	s2 := newStub()
	close(s2.release)
	call2 := c.startRefresh(c.bgCtx, "acct", s2.do)
	recv(t, call2.done, "second run")
	if call2 == calls[0] || s2.calls.Load() != 1 {
		t.Fatal("finished run was not cleared")
	}
	// Different accounts do not share a guard.
	sA, sB := newStub(), newStub()
	cA := c.startRefresh(c.bgCtx, "a", sA.do)
	cB := c.startRefresh(c.bgCtx, "b", sB.do)
	recv(t, sA.started, "a")
	recv(t, sB.started, "b")
	if cA == cB {
		t.Fatal("accounts share a refresh")
	}
	close(sA.release)
	close(sB.release)
	recv(t, cA.done, "a done")
	recv(t, cB.done, "b done")
}

func TestRefreshNowConcurrentJoinersSeeOneResult(t *testing.T) {
	c := openRFCache(t)
	a := accounts.Account{Name: "acct"}
	s := newStub()
	s.st = Stats{NewIDs: 7, FoldersScanned: 3}
	call := c.startRefresh(c.bgCtx, a.Name, s.do)
	recv(t, s.started, "stub start")

	const n = 8
	outs := make([]RefreshOutcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = c.RefreshNow(context.Background(), discardLog, a, 5*time.Minute, rfDeadline)
		}()
	}
	waitParked(t, "Cache).RefreshNow", n)
	close(s.release)
	wg.Wait()
	recv(t, call.done, "done")
	if got := s.calls.Load(); got != 1 {
		t.Fatalf("stub ran %d times, want 1", got)
	}
	for i := range n {
		o := outs[i]
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if o.State != StateRefreshed || !o.Joined || o.Run == nil {
			t.Fatalf("caller %d: %+v", i, o)
		}
		if o.Run.Stats.NewIDs != 7 || !o.Run.StartedAt.Equal(call.run.StartedAt) {
			t.Fatalf("caller %d saw a different result: %+v", i, o.Run)
		}
	}
}

// 5. the loop and on-demand share the guard.
func TestRunBlockingThenRefreshNowJoins(t *testing.T) {
	c := openRFCache(t)
	bh, accepted := blackhole(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, discardLog, bh, time.Hour) }()
	recv(t, accepted, "loop's dial")

	out, err := c.RefreshNow(context.Background(), discardLog, bh, 5*time.Minute, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != StateInProgress || !out.Joined {
		t.Fatalf("outcome = %+v, want joined in_progress", out)
	}
	select {
	case <-accepted:
		t.Fatal("RefreshNow dialled a second connection instead of joining")
	default:
	}
	cancel()
	recv(t, done, "Run to return")
}

func TestRunTickJoinsOnDemandAndTickerKeepsRunning(t *testing.T) {
	c := openRFCache(t)
	bh, accepted := blackhole(t)
	s := newStub()
	od := c.startRefresh(c.bgCtx, bh.Name, s.do) // the on-demand run
	recv(t, s.started, "stub start")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, discardLog, bh, 5*time.Millisecond) }()
	waitParked(t, "Cache).Run", 1)
	select {
	case <-accepted:
		t.Fatal("the loop started its own refresh next to the on-demand one")
	default:
	}
	if s.calls.Load() != 1 {
		t.Fatalf("stub calls = %d", s.calls.Load())
	}
	close(s.release)
	recv(t, od.done, "on-demand done")
	// The loop was not stopped by joining: its ticker fires and it dials.
	recv(t, accepted, "the loop's next tick")
	if s.calls.Load() != 1 {
		t.Fatalf("stub re-ran: %d", s.calls.Load())
	}
	cancel()
	recv(t, done, "Run to return")
}

// 7. timeout and Close.
func TestRefreshNowTimeoutThenCompletes(t *testing.T) {
	c := openRFCache(t)
	a := accounts.Account{Name: "acct"}
	s := newStub()
	call := c.startRefresh(c.bgCtx, a.Name, s.do)
	recv(t, s.started, "stub start")

	out, err := c.RefreshNow(context.Background(), discardLog, a, 5*time.Minute, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != StateInProgress || out.Run != nil || !out.Joined {
		t.Fatalf("outcome = %+v", out)
	}
	// A request context cancelled mid-wait also answers in_progress and
	// leaves the run alone.
	cctx, ccancel := context.WithCancel(context.Background())
	type res struct {
		out RefreshOutcome
		err error
	}
	rc := make(chan res, 1)
	go func() {
		o, err := c.RefreshNow(cctx, discardLog, a, 5*time.Minute, rfDeadline)
		rc <- res{o, err}
	}()
	waitParked(t, "Cache).RefreshNow", 1)
	ccancel()
	select {
	case r := <-rc:
		if r.err != nil || r.out.State != StateInProgress {
			t.Fatalf("cancelled ctx: %+v err=%v", r.out, r.err)
		}
	case <-time.After(rfDeadline):
		t.Fatal("RefreshNow ignored its cancelled context")
	}
	select {
	case <-call.done:
		t.Fatal("run ended with its waiter")
	default:
	}
	close(s.release)
	recv(t, call.done, "run to complete")
	if call.run.Err != nil {
		t.Fatalf("run err = %v", call.run.Err)
	}
	c.rfMu.Lock()
	last := c.acct(a.Name).last
	inflight := c.acct(a.Name).inflight
	c.rfMu.Unlock()
	if last == nil || inflight != nil {
		t.Fatalf("after completion last=%v inflight=%v", last, inflight)
	}
}

func TestCloseCancelsAndWaitsForRefresh(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	sawCancel := make(chan struct{})
	finish := make(chan struct{})
	call := c.startRefresh(c.bgCtx, "acct", func(ctx context.Context) (Stats, error) {
		<-ctx.Done()
		close(sawCancel)
		<-finish
		return Stats{}, ctx.Err()
	})
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	recv(t, sawCancel, "Close to cancel the refresh")
	select {
	case <-closed:
		t.Fatal("Close returned before the refresh finished")
	default:
	}
	close(finish)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(rfDeadline):
		t.Fatal("Close did not return")
	}
	select {
	case <-call.done:
	default:
		t.Fatal("Close returned with the refresh unfinished")
	}
	if !errors.Is(call.run.Err, context.Canceled) {
		t.Fatalf("run err = %v, want context.Canceled", call.run.Err)
	}
	if call.run.ErrText != SanitizeRefreshErr(context.Canceled) {
		t.Fatalf("ErrText = %q", call.run.ErrText)
	}
}

// 9. error sanitising and panics.
func TestSanitizeRefreshErr(t *testing.T) {
	const secret = "alice@corp.example imap.corp.example:993 /run/secrets/pw-alice"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"login", fmt.Errorf("acct: %s: %w", secret, imapx.ErrLogin), "login failed"},
		{"timeout", fmt.Errorf("%w (phase fetch): %s", imapx.ErrTimeout, secret), "mail server did not answer in time"},
		{"stalled", fmt.Errorf("acct: %s: %w", secret, ErrStalled), "refresh stalled: no progress"},
		{"canceled", fmt.Errorf("%s: %w", secret, context.Canceled), "refresh cancelled or ran out of time"},
		{"deadline", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), "refresh cancelled or ran out of time"},
		{"other", errors.New("dial tcp " + secret + ": connection refused"), "refresh failed (see server log)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeRefreshErr(tc.err)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for _, leak := range []string{"alice", "corp.example", "pw-alice", "993"} {
				if strings.Contains(got, leak) {
					t.Fatalf("%q leaks %q", got, leak)
				}
			}
		})
	}
}

func TestStartRefreshRecordsSanitizedError(t *testing.T) {
	c := openRFCache(t)
	raw := errors.New("alice@corp.example: dial imap.corp.example:993 refused")
	call := c.startRefresh(c.bgCtx, "acct", func(context.Context) (Stats, error) { return Stats{}, raw })
	recv(t, call.done, "done")
	if call.run.Err != raw || call.run.ErrText != "refresh failed (see server log)" {
		t.Fatalf("run = %+v", call.run)
	}
	if call.run.FinishedAt.Before(call.run.StartedAt) || call.run.StartedAt.IsZero() {
		t.Fatalf("timestamps = %v .. %v", call.run.StartedAt, call.run.FinishedAt)
	}
}

func TestStartRefreshRecoversPanic(t *testing.T) {
	c := openRFCache(t)
	call := c.startRefresh(c.bgCtx, "acct", func(context.Context) (Stats, error) {
		panic("boom alice@corp.example")
	})
	recv(t, call.done, "done after panic")
	if call.run.Err == nil || call.run.ErrText != "refresh failed (see server log)" {
		t.Fatalf("run = %+v", call.run)
	}
	if strings.Contains(call.run.Err.Error(), "alice") {
		t.Fatalf("panic value leaked into the error: %v", call.run.Err)
	}
	// The guard is released: a new run starts.
	s := newStub()
	close(s.release)
	next := c.startRefresh(c.bgCtx, "acct", s.do)
	recv(t, next.done, "next run")
	if next == call || s.calls.Load() != 1 {
		t.Fatal("panicked run still held the guard")
	}
}

// RefreshNow against an unreachable server: the failure is reported with the
// fixed phrase, never the dial error that names the host.
func TestRefreshNowFailureIsSanitised(t *testing.T) {
	c := openRFCache(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close() // nothing listens there now
	port, _ := strconv.Atoi(p)
	a := accounts.Account{Name: "down", Host: "127.0.0.1", Port: port, TLS: accounts.Implicit, Username: "alice@corp.example"}
	out, err := c.RefreshNow(context.Background(), discardLog, a, 5*time.Minute, rfDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != StateRefreshed || out.Run == nil || out.Run.Err == nil {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Run.ErrText != "refresh failed (see server log)" || out.LastOK {
		t.Fatalf("ErrText=%q LastOK=%v", out.Run.ErrText, out.LastOK)
	}
	if out.LastRefresh == nil {
		t.Fatal("a failed refresh must still leave a refreshes row")
	}
}
