package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

// refreshErr is e.refresh without the fatal on error, with a ctx.
func (e *env) refreshErr(ctx context.Context) (Stats, error) {
	e.t.Helper()
	c, err := imapx.Dial(e.ctx(), e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return e.cache.Refresh(ctx, e.acct, c)
}

// killAfterBodyFetches closes every connection when the n-th full-body FETCH
// arrives, before the server answers it: n-1 body FETCHes complete.
func (e *env) killAfterBodyFetches(n int) {
	var seen atomic.Int32
	e.log.setHook(func(chunk string) {
		if c := strings.Count(chunk, "BODY.PEEK[]"); c > 0 && int(seen.Add(int32(c))) >= n {
			e.log.killConns()
		}
	})
}

func (e *env) clearHook() { e.log.setHook(nil) }

var fetchSetRE = regexp.MustCompile(`(?i)UID FETCH (\S+) `)

// expandSet turns "1:3,7" into UIDs.
func expandSet(t *testing.T, s string) []uint32 {
	t.Helper()
	var out []uint32
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a, err := strconv.ParseUint(lo, 10, 32)
		if err != nil {
			t.Fatalf("bad uid set %q", s)
		}
		b := a
		if isRange {
			b, err = strconv.ParseUint(hi, 10, 32)
			if err != nil {
				t.Fatalf("bad uid set %q", s)
			}
		}
		for u := a; u <= b; u++ {
			out = append(out, uint32(u))
		}
	}
	return out
}

// headerFetchUIDs is every UID named by a header FETCH in the log.
func headerFetchUIDs(t *testing.T, l *cmdLog) map[uint32]bool {
	t.Helper()
	out := map[uint32]bool{}
	for _, ln := range l.lines() {
		if !strings.Contains(ln, "HEADER.FIELDS") {
			continue
		}
		m := fetchSetRE.FindStringSubmatch(ln)
		if m == nil {
			t.Fatalf("unparsable header fetch %q", ln)
		}
		for _, u := range expandSet(t, m[1]) {
			out[u] = true
		}
	}
	return out
}

func bodyFetchCount(l *cmdLog) int {
	n := 0
	for _, ln := range l.lines() {
		if strings.Contains(ln, "BODY.PEEK[]") {
			n++
		}
	}
	return n
}

func memberUIDs(t *testing.T, e *env, folder string) map[uint32]bool {
	t.Helper()
	rows, err := e.cache.db.Query(`SELECT uid FROM membership WHERE folder = ?`, folder)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uint32]bool{}
	for rows.Next() {
		var u uint32
		_ = rows.Scan(&u)
		out[u] = true
	}
	return out
}

func fill(e *env, folder string, n int, prefix string) {
	for i := 1; i <= n; i++ {
		e.appendMsg(folder, mkMsg(fmt.Sprintf("%s%d", prefix, i), fmt.Sprintf("s %s%d", prefix, i), "body "+prefix), t0.Add(time.Duration(i)*time.Second))
	}
}

// interrupted runs a refresh killed on its 2nd body FETCH (so one batch of
// bodyBatch messages is committed) and returns what that run did.
func interrupted(t *testing.T, e *env, total int) Stats {
	t.Helper()
	e.killAfterBodyFetches(2)
	st, err := e.refreshErr(e.ctx())
	e.clearHook()
	if err == nil {
		t.Fatal("interrupted refresh returned no error")
	}
	if st.NewBodies != bodyBatch {
		t.Fatalf("setup: interrupted run stored %d bodies, want %d (stats %+v)", st.NewBodies, bodyBatch, st)
	}
	return st
}

func TestInterruptedRefreshResumesWithoutRefetchingMembers(t *testing.T) {
	const total = 60
	e := newEnv(t, accounts.Gmail, "X")
	fill(e, "X", total, "m")

	st1 := interrupted(t, e, total)
	if st1.NewUIDs != total {
		t.Errorf("first run NewUIDs = %d, want %d", st1.NewUIDs, total)
	}
	// Membership equals the messages committed.
	members := memberUIDs(t, e, "X")
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != bodyBatch || len(members) != bodyBatch {
		t.Fatalf("after kill: messages=%d membership=%d, want %d each", n, len(members), bodyBatch)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership s JOIN messages m USING (account, stable_id)`); n != bodyBatch {
		t.Errorf("membership rows with an indexed message = %d, want %d", n, bodyBatch)
	}

	e.log.reset()
	st2, err := e.refreshErr(e.ctx())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	// No header FETCH for a UID already in membership.
	hdr := headerFetchUIDs(t, e.log)
	if len(hdr) == 0 {
		t.Fatal("no header FETCH recorded: the assertion would be vacuous")
	}
	for u := range hdr {
		if members[u] {
			t.Errorf("uid %d was in membership but header-fetched again", u)
		}
	}
	if len(hdr) != total-bodyBatch {
		t.Errorf("header-fetched %d uids, want %d", len(hdr), total-bodyBatch)
	}
	// NewUIDs is only the UIDs not yet in membership.
	if st2.NewUIDs != total-bodyBatch {
		t.Errorf("second run NewUIDs = %d, want %d (production symptom: membership not persisted across runs)", st2.NewUIDs, total-bodyBatch)
	}
	if st2.NewBodies != total-bodyBatch {
		t.Errorf("second run NewBodies = %d, want %d", st2.NewBodies, total-bodyBatch)
	}
	if n := len(memberUIDs(t, e, "X")); n != total {
		t.Errorf("membership = %d, want %d", n, total)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != total {
		t.Errorf("messages = %d, want %d", n, total)
	}

	// Third run is quiet.
	st3 := e.refresh()
	if st3.NewUIDs != 0 || st3.NewBodies != 0 || st3.Removed != 0 {
		t.Errorf("third run not quiet: %+v", st3)
	}
}

// Membership of already-indexed ids is written right after the header fetch,
// before any body arrives.
func TestInterruptedRefreshKeepsMembershipOfAlreadyIndexedIDs(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "A", "B")
	fill(e, "A", 30, "s") // folder A: 30 messages
	e.refresh()           // B empty, A fully indexed
	// B: the same 30 messages (already indexed) then 30 new ones.
	fill(e, "B", 30, "s")
	for i := 1; i <= 30; i++ {
		e.appendMsg("B", mkMsg(fmt.Sprintf("n%d", i), "new", "new"), t0.Add(time.Hour+time.Duration(i)*time.Second))
	}
	e.killAfterBodyFetches(1)
	_, err := e.refreshErr(e.ctx())
	e.clearHook()
	if err == nil {
		t.Fatal("expected an error from the killed refresh")
	}
	if n := len(memberUIDs(t, e, "B")); n != 30 {
		t.Fatalf("B membership after kill = %d, want the 30 already-indexed ids", n)
	}
	members := memberUIDs(t, e, "B")

	e.log.reset()
	st, err := e.refreshErr(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	for u := range headerFetchUIDs(t, e.log) {
		if members[u] {
			t.Errorf("B uid %d re-header-fetched", u)
		}
	}
	if st.NewUIDs != 30 || st.NewBodies != 30 {
		t.Errorf("second run %+v, want 30 new uids / 30 bodies", st)
	}
	if n := len(memberUIDs(t, e, "B")); n != 60 {
		t.Errorf("B membership = %d, want 60", n)
	}
}

func TestRemovalsAfterResumedRun(t *testing.T) {
	const total = 60
	e := newEnv(t, accounts.Gmail, "X")
	fill(e, "X", total, "m")
	interrupted(t, e, total)
	members := memberUIDs(t, e, "X")
	var member, unresolved uint32
	for u := uint32(1); u <= total; u++ {
		if members[u] && member == 0 {
			member = u
		}
		if !members[u] {
			unresolved = u
		}
	}
	if member == 0 || unresolved == 0 {
		t.Fatal("setup: need a member and an unresolved uid")
	}
	e.expungeUID("X", imapUID(member))
	e.expungeUID("X", imapUID(unresolved))

	blobsBefore := e.count(`SELECT COUNT(DISTINCT blob_sha256) FROM messages`)
	msgsBefore := e.count(`SELECT COUNT(*) FROM messages`)
	st, err := e.refreshErr(e.ctx())
	if err != nil {
		t.Fatalf("refresh after expunge of a never-resolved uid: %v", err)
	}
	if st.Removed != 1 {
		t.Errorf("Removed = %d, want 1 (the member; the unresolved uid has no row)", st.Removed)
	}
	if st.NewUIDs != total-bodyBatch-1 {
		t.Errorf("NewUIDs = %d, want %d", st.NewUIDs, total-bodyBatch-1)
	}
	if members := memberUIDs(t, e, "X"); members[member] || len(members) != total-2 {
		t.Errorf("membership = %d rows (expunged member present: %v), want %d", len(members), members[member], total-2)
	}
	// The expunged member's message stays; the never-fetched one never existed.
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != total-1 || n < msgsBefore {
		t.Errorf("messages = %d, want %d (before %d)", n, total-1, msgsBefore)
	}
	if n := e.count(`SELECT COUNT(DISTINCT blob_sha256) FROM messages`); n < blobsBefore {
		t.Errorf("blobs shrank: %d < %d", n, blobsBefore)
	}
}

func TestUIDValidityChangeAfterInterruptedRunDropsAndRescans(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "X")
	fill(e, "X", 60, "m")
	interrupted(t, e, 60)
	e.deleteFolder("X")
	e.createFolder("X")
	fill(e, "X", 3, "q")
	st, err := e.refreshErr(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	if st.Removed != bodyBatch {
		t.Errorf("Removed = %d, want %d (old membership dropped)", st.Removed, bodyBatch)
	}
	if st.NewUIDs != 3 || st.NewBodies != 3 {
		t.Errorf("stats %+v, want 3 new uids / 3 bodies", st)
	}
	if n := len(memberUIDs(t, e, "X")); n != 3 {
		t.Errorf("membership = %d, want 3", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != bodyBatch+3 {
		t.Errorf("messages = %d, want %d", n, bodyBatch+3)
	}
}

func TestSharedStableIDTwoUIDsOneBodyFetch(t *testing.T) {
	// Same folder, two UIDs, one id.
	e := newEnv(t, accounts.Gmail, "X", "Label")
	raw := mkMsg("dup", "dup", "same bytes")
	e.appendMsg("X", raw, t0)
	e.appendMsg("X", raw, t0)
	e.log.reset()
	st := e.refresh()
	if st.NewUIDs != 2 || st.NewIDs != 1 || st.NewBodies != 1 {
		t.Fatalf("stats %+v", st)
	}
	if n := bodyFetchCount(e.log); n != 1 {
		t.Errorf("body FETCHes = %d, want 1", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'X'`); n != 2 {
		t.Errorf("membership in X = %d, want 2", n)
	}
	// Second folder, same message, same refresh: no further body FETCH.
	e.appendMsg("Label", raw, t0)
	e.log.reset()
	st = e.refresh()
	if st.NewUIDs != 1 || st.NewIDs != 0 || st.NewBodies != 0 {
		t.Fatalf("label copy stats %+v", st)
	}
	if n := bodyFetchCount(e.log); n != 0 {
		t.Errorf("body FETCHes for label copy = %d, want 0", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'Label'`); n != 1 {
		t.Errorf("membership in Label = %d, want 1", n)
	}
}

func TestProgressIsMonotonicAndTotalIsFreshCount(t *testing.T) {
	const total = 60
	e := newEnv(t, accounts.Gmail, "X")
	fill(e, "X", total, "m")
	var got []Progress
	ctx := WithProgress(e.ctx(), func(p Progress) { got = append(got, p) })
	if _, err := e.refreshErr(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 {
		t.Fatalf("only %d reports", len(got))
	}
	if got[0].Done != 0 {
		t.Errorf("first report Done = %d, want 0", got[0].Done)
	}
	prev := -1
	for i, p := range got {
		if p.Total != total {
			t.Errorf("report %d Total = %d, want %d", i, p.Total, total)
		}
		if p.Done < prev {
			t.Errorf("report %d Done %d went backwards from %d", i, p.Done, prev)
		}
		prev = p.Done
	}
	last := got[len(got)-1]
	if last.Done != total || last.NewBodies != total {
		t.Errorf("last report %+v, want Done=Total=NewBodies=%d", last, total)
	}
}

func TestProgressLoggerRateLimits(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	lines := func() int { return strings.Count(buf.String(), "cache fill progress") }

	f := progressLogger(log, "a", time.Hour)
	for i := 1; i <= 10; i++ {
		f(Progress{Folder: "X", Done: i, Total: 10})
	}
	if n := lines(); n != 1 {
		t.Errorf("hour interval logged %d lines, want 1", n)
	}
	buf.Reset()
	f = progressLogger(log, "a", time.Hour)
	f(Progress{Folder: "X", Total: 0})
	if lines() != 0 {
		t.Error("Total==0 must not log")
	}
	buf.Reset()
	f = progressLogger(log, "a", 30*time.Millisecond)
	f(Progress{Folder: "X", Done: 1, Total: 10})
	f(Progress{Folder: "X", Done: 2, Total: 10})
	time.Sleep(60 * time.Millisecond)
	f(Progress{Folder: "X", Done: 3, Total: 10})
	if n := lines(); n != 2 {
		t.Errorf("short interval logged %d lines, want 2", n)
	}
}

// --- withStallTimeout -------------------------------------------------------

func waitDone(t *testing.T, ctx context.Context, d time.Duration) bool {
	t.Helper()
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func TestStallTimeoutFiresWithErrStalled(t *testing.T) {
	ctx, _, cancel := withStallTimeout(context.Background(), 50*time.Millisecond, time.Minute)
	defer cancel()
	if !waitDone(t, ctx, 2*time.Second) {
		t.Fatal("did not fire")
	}
	if c := context.Cause(ctx); !errors.Is(c, ErrStalled) {
		t.Errorf("cause = %v, want ErrStalled", c)
	}
}

func TestStallTimeoutHoldsWhileTouched(t *testing.T) {
	ctx, touch, cancel := withStallTimeout(context.Background(), 80*time.Millisecond, time.Minute)
	defer cancel()
	for i := 0; i < 10; i++ { // 300ms total, > 3x the stall window
		time.Sleep(30 * time.Millisecond)
		touch()
		if ctx.Err() != nil {
			t.Fatalf("fired while touched (iteration %d): %v", i, context.Cause(ctx))
		}
	}
	if !waitDone(t, ctx, 2*time.Second) {
		t.Fatal("did not fire after touches stopped")
	}
	if !errors.Is(context.Cause(ctx), ErrStalled) {
		t.Errorf("cause = %v", context.Cause(ctx))
	}
}

func TestStallTimeoutCeilingFiresDespiteTouches(t *testing.T) {
	ctx, touch, cancel := withStallTimeout(context.Background(), 100*time.Millisecond, 150*time.Millisecond)
	defer cancel()
	deadline := time.After(2 * time.Second)
	for ctx.Err() == nil {
		select {
		case <-deadline:
			t.Fatal("ceiling did not fire")
		case <-time.After(10 * time.Millisecond):
			touch()
		}
	}
	if c := context.Cause(ctx); !errors.Is(c, context.DeadlineExceeded) {
		t.Errorf("cause = %v, want DeadlineExceeded", c)
	}
}

func TestStallTimeoutParentCancelPropagates(t *testing.T) {
	parent, pc := context.WithCancel(context.Background())
	ctx, _, cancel := withStallTimeout(parent, time.Minute, time.Minute)
	defer cancel()
	pc()
	if !waitDone(t, ctx, 2*time.Second) {
		t.Fatal("parent cancel did not propagate")
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Errorf("cause = %v, want Canceled", context.Cause(ctx))
	}
}

func TestStallTimeoutCancelReleasesWithoutStalledCause(t *testing.T) {
	ctx, touch, cancel := withStallTimeout(context.Background(), 40*time.Millisecond, time.Minute)
	cancel()
	time.Sleep(120 * time.Millisecond)
	touch() // must not re-arm into ErrStalled
	if errors.Is(context.Cause(ctx), ErrStalled) {
		t.Errorf("cause after cancel = %v", context.Cause(ctx))
	}
}

func imapUID(u uint32) imap.UID { return imap.UID(u) }
