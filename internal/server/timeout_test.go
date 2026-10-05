package server

// Tool-level tests for the hard budgets on live IMAP calls. liveTimeout is a
// var in this package and is shortened here (those tests are not parallel, so
// the swap is safe), and so are the organise budgets via organise.SetBudgets.
// Every wait is bounded. The -race run belongs to CI (no gcc here).

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/organise"
)

const timeoutText = "mail server did not answer in time"

// shortLive shortens liveTimeout for the test and restores it.
func shortLive(t *testing.T) time.Duration {
	t.Helper()
	old := liveTimeout
	liveTimeout = 2 * time.Second
	t.Cleanup(func() { liveTimeout = old })
	return liveTimeout
}

// callLong is call with room for a budget to run out.
func callLong(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	start := time.Now()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: protocol error: %v", tool, err)
	}
	return res, time.Since(start)
}

// wantTimeoutResult checks a tool result is the fixed timeout text, leaks
// nothing, and came back within budget + grace + slack.
func wantTimeoutResult(t *testing.T, label string, res *mcp.CallToolResult, took, budget time.Duration, dir string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("%s: want a tool error, got %s", label, text(res))
	}
	if got := text(res); got != timeoutText {
		t.Errorf("%s: error = %q, want exactly %q", label, got, timeoutText)
	}
	noLeak(t, label, text(res), dir)
	if took < budget-time.Second || took > budget+3*time.Second+2*time.Second {
		t.Errorf("%s: took %s, want about %s (at most +3s grace)", label, took, budget)
	}
}

// holdNth blocks the connection that sends the nth command matching re until
// release is closed; reached is closed when it is blocked. Other connections
// and other commands pass.
func holdNth(re *regexp.Regexp, n int, reached, release chan struct{}) func([]byte) error {
	var mu sync.Mutex
	seen := 0
	return func(p []byte) error {
		c := countRE(re, p)
		if c == 0 {
			return nil
		}
		mu.Lock()
		before := seen
		seen += c
		hit := before < n && seen >= n
		mu.Unlock()
		if hit {
			close(reached)
			select {
			case <-release:
			case <-time.After(180 * time.Second):
			}
		}
		return nil
	}
}

// closeOnCleanup releases a hold when the test ends, once.
func closeOnCleanup(t *testing.T, ch chan struct{}) {
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(ch) }) })
}

// 5: server search against a stalled fake.
func TestServerSearchStalledTimesOutFreesSlotAndRecovers(t *testing.T) {
	budget := shortLive(t)
	cfg := defaultFake()
	cfg.Hold = make(chan struct{})
	cfg.Entered = make(chan struct{})
	e := gmailEnv(t, cfg, Read)
	args := map[string]any{"server": true, "account": e.g.Name, "query": "from:a"}

	res, took := callLong(t, e.cs, "search", args)
	wantTimeoutResult(t, "first search", res, took, budget, e.dir)

	// After the grace the slot is free: the next call reaches the server
	// again (and stalls again) instead of being refused as in progress.
	res, took = callLong(t, e.cs, "search", args)
	if strings.Contains(text(res), "in progress") {
		t.Fatalf("second search refused as busy after the timeout: %q", text(res))
	}
	wantTimeoutResult(t, "second search", res, took, budget, e.dir)

	e.fake.release()
	got, _ := ok[gmSearchOut](t, e.cs, "search", args)
	if len(got.Results) == 0 {
		t.Errorf("search after release returned nothing: %+v", got)
	}
}

// 5: live list_folders shares the budget and the message.
func TestLiveListFoldersStalledTimesOutAndRecovers(t *testing.T) {
	budget := shortLive(t)
	e := newWEnv(t, true, []wspec{{"lf-stall", accounts.Gmail}}, "INBOX", "Work")
	cs := e.connect(Read, nil)
	reached, release := make(chan struct{}), make(chan struct{})
	closeOnCleanup(t, release)
	e.setHook(holdFirst(regexp.MustCompile(`(?i) LIST `), reached, release))
	args := map[string]any{"account": "lf-stall", "live": true}

	res, took := callLong(t, cs, "list_folders", args)
	wantTimeoutResult(t, "live list_folders", res, took, budget, e.dir)
	select {
	case <-reached:
	default:
		t.Error("the stall never engaged")
	}
	// The slot is free and the next call (not held) works.
	out, _ := ok[listFoldersOut](t, cs, "list_folders", args)
	if out.Source != "server" || out.Count == 0 {
		t.Errorf("live listing after the stall: %+v", out)
	}
}

// 6: create_folder against a stalled CREATE.
func TestCreateFolderStalledTimesOutAndReleasesTheWriteSlot(t *testing.T) {
	defer organise.SetBudgets(organise.Budgets{Create: time.Second})()
	e := newWEnv(t, true, []wspec{{"acct", accounts.Gmail}}, "INBOX")
	cs := e.admin()
	reached, release := make(chan struct{}), make(chan struct{})
	closeOnCleanup(t, release)
	e.setHook(holdNth(regexp.MustCompile(`(?i) CREATE `), 1, reached, release)) // holdFirst gives up after 30s, under the budget

	res, took := callLong(t, cs, "create_folder", map[string]any{"account": "acct", "name": "Fresh"})
	wantTimeoutResult(t, "create_folder", res, took, time.Second, e.dir)
	if h, _ := listHistory(t, cs, "acct"); h.Count != 0 {
		t.Errorf("a create_folder that timed out wrote history: %+v", h)
	}
	// The write slot is free: the next write is not "another write is in progress".
	res = call(t, cs, "create_folder", map[string]any{"account": "acct", "name": "Fresh2"})
	if res.IsError {
		t.Fatalf("create_folder after the timeout: %s", text(res))
	}
}

// 7: apply with the second MOVE chunk held.
func TestApplyTimeoutKeepsTheCompletedChunkInHistoryAndReleasesTheSlot(t *testing.T) {
	defer organise.SetBudgets(organise.Budgets{Apply: 3 * time.Second})()
	e := newWEnv(t, true, gmailPair, "INBOX", "Work")
	e.addMany("INBOX", "bulk@example.com", 600)
	e.refresh("acct")
	e.log.reset()
	cs := e.connect(Admin, acceptConfirm(t), WithHistory(e.hist), WithOrganiser(e.org), WithApprovalMode(ApprovalElicitation))
	p := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Work", "move")
	if p.Matched != 600 {
		t.Fatalf("matched %d, want 600", p.Matched)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	closeOnCleanup(t, release)
	e.setHook(holdNth(uidMoveRE, 2, reached, release))

	res, took := callLong(t, cs, "apply_intent", applyArgs(p))
	if !res.IsError {
		t.Fatalf("apply = %s, want a timeout", text(res))
	}
	msg := text(res)
	if !strings.HasPrefix(msg, timeoutText+"; 500 of 600 messages were changed (history ") {
		t.Errorf("apply text = %q", msg)
	}
	noLeak(t, "apply", msg, e.dir)
	// applyBudget 3s, then the refresh runs on a fresh connection.
	if took < 2*time.Second || took > 3*time.Second+3*time.Second+10*time.Second {
		t.Errorf("apply took %s", took)
	}
	select {
	case <-reached:
	default:
		t.Error("the second MOVE was never held")
	}

	h, _ := listHistory(t, cs, "acct")
	if h.Count != 1 {
		t.Fatalf("history = %+v", h)
	}
	r := h.Records[0]
	if r.Kind != "apply" || r.TouchedCount != 500 || r.Error != timeoutText {
		t.Errorf("record = kind %q touched %d error %q, want apply, 500, %q", r.Kind, r.TouchedCount, r.Error, timeoutText)
	}
	if r.Preview.ApprovedBy != history.ApprovedElicitation {
		t.Errorf("approved_by = %q", r.Preview.ApprovedBy)
	}
	// Chunk 1 is on the server and the cache was refreshed to say so.
	if n := e.serverCount("Work"); n != 500 {
		t.Errorf("server Work = %d, want 500", n)
	}
	if n, m := len(e.members("acct", "Work")), len(e.members("acct", "INBOX")); n != 500 || m != 100 {
		t.Errorf("cache Work %d INBOX %d, want 500 and 100", n, m)
	}
	// The write slot was released.
	res = call(t, cs, "create_folder", map[string]any{"account": "acct", "name": "Fresh"})
	if res.IsError {
		t.Errorf("write after the timed-out apply: %s", text(res))
	}
}

// 8: refusals before the session did anything send no refresh. The command
// log shows one LOGIN for the apply and, only on success, a second for the
// post-apply refresh.
func TestEarlyApplyRefusalsDoNotRefreshAfterwards(t *testing.T) {
	logins := func(e *wenv) int { return e.log.count("LOGIN") }

	t.Run("control: a successful apply logs in twice", func(t *testing.T) {
		e := basic(t, true)
		cs := e.admin()
		p := preview(t, cs, "acct", "INBOX", fromCrit("alice@example.com"), "Work", "move")
		e.log.reset()
		if a := apply(t, cs, p); a.Done != 2 {
			t.Fatalf("apply = %+v", a)
		}
		if n := logins(e); n != 2 {
			t.Errorf("LOGIN %d times, want 2 (apply + refresh): %q", n, e.log.lines())
		}
	})

	check := func(t *testing.T, e *wenv, cs *mcp.ClientSession, p prevT, want string) {
		t.Helper()
		e.log.reset()
		requireToolError(t, cs, "apply_intent", applyArgs(p), want)
		if n := logins(e); n != 1 {
			t.Errorf("LOGIN %d times, want 1 (no refresh after a refusal): %q", n, e.log.lines())
		}
		if v := e.log.count("MOVE") + e.log.count("COPY"); v != 0 {
			t.Errorf("a move or copy was sent: %q", e.log.lines())
		}
	}

	t.Run("no MOVE capability", func(t *testing.T) {
		e := basic(t, false)
		cs := e.admin()
		p := preview(t, cs, "acct", "INBOX", fromCrit("alice@example.com"), "Work", "move")
		check(t, e, cs, p, "")
	})
	t.Run("source folder missing on the server", func(t *testing.T) {
		e := newWEnv(t, true, gmailPair, "INBOX", "Src", "Work")
		e.addAt("acct", "Src", "s1", alice, "s1", t0)
		e.refresh("acct")
		cs := e.admin()
		p := preview(t, cs, "acct", "Src", fromCrit("alice@example.com"), "Work", "move")
		if err := e.user.Delete("Src"); err != nil {
			t.Fatal(err)
		}
		check(t, e, cs, p, "")
	})
	t.Run("target folder missing on the server", func(t *testing.T) {
		e := basic(t, true)
		cs := e.admin()
		if err := e.cache.NoteFolder(e.ctx(), "acct", "Ghost"); err != nil {
			t.Fatal(err)
		}
		p := preview(t, cs, "acct", "INBOX", fromCrit("alice@example.com"), "Ghost", "move")
		check(t, e, cs, p, "target folder does not exist")
	})
	t.Run("UIDVALIDITY changed", func(t *testing.T) {
		e := basic(t, true)
		cs := e.admin()
		p := preview(t, cs, "acct", "INBOX", fromCrit("alice@example.com"), "Work", "move")
		if err := e.user.Delete("INBOX"); err != nil {
			t.Fatal(err)
		}
		if err := e.user.Create("INBOX", nil); err != nil {
			t.Fatal(err)
		}
		e.add("INBOX", "a1", alice, "a1")
		e.add("INBOX", "a2", alice, "a2")
		e.add("INBOX", "b1", bobby, "b1")
		check(t, e, cs, p, "UIDVALIDITY changed")
	})
}

// 9: the texts a refusal or timeout produces carry no host, port or path.
func TestEarlyRefusalTextsLeakNothing(t *testing.T) {
	e := basic(t, false)
	cs := e.admin()
	p := preview(t, cs, "acct", "INBOX", fromCrit("alice@example.com"), "Work", "move")
	res := call(t, cs, "apply_intent", applyArgs(p))
	if !res.IsError {
		t.Fatalf("want refusal, got %s", text(res))
	}
	noLeak(t, "no MOVE", text(res), e.dir)
}
