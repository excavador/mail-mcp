package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/imapx"
)

// verbsSince is the command names the client sent after the first n bytes.
func (f *gmailFake) verbsSince(n int) []string {
	var out []string
	for _, ln := range strings.Split(f.log.raw()[n:], "\r\n") {
		if !gfTag.MatchString(ln) {
			continue
		}
		fl := strings.Fields(ln)
		v := strings.ToUpper(fl[1])
		if v == "UID" && len(fl) > 2 {
			v = strings.ToUpper(fl[2])
		}
		out = append(out, v)
	}
	return out
}

func indexOf(vs []string, want string) int {
	for i, v := range vs {
		if v == want {
			return i
		}
	}
	return -1
}

func count(vs []string, want string) int {
	n := 0
	for _, v := range vs {
		if v == want {
			n++
		}
	}
	return n
}

func TestServerSearchWithCachedAllMailSendsNoListAndPipelines(t *testing.T) {
	const delay = 300 * time.Millisecond
	e := gmailEnv(t, defaultFake(), Read)
	if got := e.st.AllMailFolder(context.Background(), e.g.Name); got != "[Gmail]/All Mail" {
		t.Fatalf("refresh did not record the \\All folder: %q", got)
	}
	e.fake.setDelay(delay)
	off := len(e.fake.log.raw())

	out, _ := ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "from:a"})
	if out.Count+out.UncachedCount != 4 {
		t.Errorf("results = %+v, want the 4 UIDs the fake answers", out)
	}

	vs := e.fake.verbsSince(off)
	if count(vs, "LIST") != 0 {
		t.Errorf("a LIST was sent although All Mail is cached: %v", vs)
	}
	ex, se := indexOf(vs, "EXAMINE"), indexOf(vs, "SEARCH")
	if ex < 0 || se < 0 || ex > se || count(vs, "EXAMINE") != 1 || count(vs, "SEARCH") != 1 {
		t.Fatalf("want one EXAMINE then one SEARCH, got %v", vs)
	}
	// Every answer is delayed, so a SEARCH sent only after EXAMINE was
	// answered would reach the server a full delay after the EXAMINE.
	tex, ok1 := e.fake.log.firstArrival("EXAMINE", off)
	tse, ok2 := e.fake.log.firstArrival("SEARCH", off)
	if !ok1 || !ok2 {
		t.Fatal("EXAMINE or SEARCH not in the arrival log")
	}
	if gap := tse.Sub(tex); gap > delay/2 {
		t.Errorf("SEARCH arrived %v after EXAMINE (delay %v): not pipelined", gap, delay)
	}
}

func TestGmailRawSearchInWithoutHintLists(t *testing.T) {
	e := gmailEnv(t, defaultFake(), Read)
	off := len(e.fake.log.raw())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := imapx.Do(ctx, e.g, "search", 15*time.Second, func(ctx context.Context, c *imapclient.Client) error {
		folder, uids, err := imapx.GmailRawSearchIn(ctx, c, "x", "")
		if err != nil {
			return err
		}
		if folder != "[Gmail]/All Mail" || len(uids) != 4 {
			t.Errorf("folder %q uids %v", folder, uids)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	vs := e.fake.verbsSince(off)
	li, ex, se := indexOf(vs, "LIST"), indexOf(vs, "EXAMINE"), indexOf(vs, "SEARCH")
	if li < 0 || !(li < ex && ex < se) {
		t.Errorf("verbs = %v, want LIST, EXAMINE, SEARCH", vs)
	}
}

func TestServerSearchFallsBackToListOnceWhenHintedFolderIsGone(t *testing.T) {
	e := gmailEnv(t, defaultFake(), Read)
	// The cached name goes stale: the folder was renamed on the server.
	for i := range e.fake.cfg.Folders {
		if e.fake.cfg.Folders[i].All {
			e.fake.cfg.Folders[i].Name = "[Gmail]/Tous les messages"
		}
	}
	off := len(e.fake.log.raw())

	res := call(t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "from:a"})
	if res.IsError {
		t.Fatalf("search failed instead of falling back: %s", text(res))
	}
	vs := e.fake.verbsSince(off)
	if count(vs, "LIST") != 1 || count(vs, "EXAMINE") != 2 || count(vs, "SEARCH") != 2 {
		t.Fatalf("verbs = %v, want EXAMINE SEARCH LIST EXAMINE SEARCH (LIST once)", vs)
	}
	if !(indexOf(vs, "EXAMINE") < indexOf(vs, "LIST")) {
		t.Errorf("hinted EXAMINE should come before the fallback LIST: %v", vs)
	}
	if !strings.Contains(e.fake.log.raw()[off:], "Tous les messages") {
		t.Error("the retry did not use the LISTed name")
	}
}

func TestServerSearchFallbackFailsCleanlyWhenNoAllMail(t *testing.T) {
	cfg := defaultFake()
	e := gmailEnv(t, cfg, Read)
	// Hint is stale and LIST has no \All folder either: the fallback uses
	// the well-known name, which does not exist, and the error is reported
	// after one retry, not looped.
	for i := range e.fake.cfg.Folders {
		if e.fake.cfg.Folders[i].All {
			e.fake.cfg.Folders[i].Name = "Gone"
			e.fake.cfg.Folders[i].All = false
		}
	}
	off := len(e.fake.log.raw())
	res := call(t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "from:a"})
	if !res.IsError {
		t.Fatalf("want an error, got %s", text(res))
	}
	vs := e.fake.verbsSince(off)
	if count(vs, "LIST") != 1 || count(vs, "EXAMINE") > 2 {
		t.Errorf("verbs = %v: want at most one LIST and two EXAMINE", vs)
	}
	noLeak(t, "no all mail", text(res), e.dir)
}
