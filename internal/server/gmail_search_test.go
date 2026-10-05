package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/imapx"
)

var gmSeq atomic.Int64

type gmEnv struct {
	t    *testing.T
	fake *gmailFake
	dir  string
	st   *cache.Cache
	g    accounts.Account // gmail
	p    accounts.Account // proton
	cs   *mcp.ClientSession
}

// gmailEnv starts the fake, refreshes a Gmail account into a fresh cache (so
// the cache holds All Mail UIDs) and connects an MCP client. Account names are
// unique per test because the live slot is process-wide.
func gmailEnv(t *testing.T, cfg gfConfig, mode Mode) *gmEnv {
	t.Helper()
	f := startGmailFake(t, cfg)
	dir := t.TempDir()
	name := fmt.Sprintf("gm%d", gmSeq.Add(1))
	e := &gmEnv{t: t, fake: f, dir: dir}
	e.g = f.account(t, dir, name+"-g", accounts.Gmail)
	e.p = f.account(t, dir, name+"-p", accounts.Proton)
	st, err := cache.Open(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := imapx.Dial(ctx, e.g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Refresh(ctx, e.g, c); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	_ = c.Close()

	e.cs = connectServer(t, New([]accounts.Account{e.g, e.p}, st, "test", mode))
	return e
}

func defaultFake() gfConfig {
	return gfConfig{
		Folders: []gfFolder{
			{Name: "INBOX", Validity: 1, Msgs: []gfMsg{
				{UID: 3, MsgID: 111, ThrID: 9, Raw: gfMailAt("one", "alpha", t0), Date: t0},
			}},
			{Name: "[Gmail]/All Mail", Validity: 2, All: true, Msgs: []gfMsg{
				{UID: 77, MsgID: 111, ThrID: 9, Raw: gfMailAt("one", "alpha", t0), Date: t0},
				{UID: 78, MsgID: 222, ThrID: 10, Raw: gfMailAt("two", "beta", t0.Add(time.Hour)), Date: t0.Add(time.Hour)},
			}},
		},
		Search: []uint32{77, 78, 500, 501},
	}
}

type gmSearchOut struct {
	Results []struct {
		StableID string   `json:"stable_id"`
		Subject  string   `json:"subject"`
		Snippet  string   `json:"snippet"`
		Folders  []string `json:"folders"`
	} `json:"hits"`
	NextCursor    string `json:"next_cursor"`
	Note          string `json:"note"`
	UncachedCount int    `json:"uncached_count"`
	Notice        string `json:"notice"`
}

var hostPortRE = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+|:\d{3,5}\b|localhost`)

func noLeak(t *testing.T, label, msg, dir string) {
	t.Helper()
	if strings.Contains(msg, "/") || hostPortRE.MatchString(msg) || strings.Contains(msg, dir) {
		t.Errorf("%s: error leaks a path or host:port: %q", label, msg)
	}
}

func TestServerSearchValidationErrors(t *testing.T) {
	e := gmailEnv(t, defaultFake(), Read)
	g, p := e.g.Name, e.p.Name
	before := len(e.fake.log.raw())

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no account", map[string]any{"server": true, "query": "from:a"}, "needs an account"},
		{"unknown account", map[string]any{"server": true, "account": "ghost", "query": "from:a"}, ""},
		{"non-gmail", map[string]any{"server": true, "account": p, "query": "from:a"}, "Gmail accounts only"},
		{"empty query", map[string]any{"server": true, "account": g}, "needs a query"},
		{"blank query", map[string]any{"server": true, "account": g, "query": " \t "}, "needs a query"},
		{"513 bytes", map[string]any{"server": true, "account": g, "query": strings.Repeat("a", 513)}, "query too large"},
		{"513 bytes multibyte", map[string]any{"server": true, "account": g, "query": strings.Repeat("é", 257)}, "query too large"},
		{"newline", map[string]any{"server": true, "account": g, "query": "a\nb"}, "single line"},
		{"carriage return", map[string]any{"server": true, "account": g, "query": "a\rb"}, "single line"},
		{"NUL", map[string]any{"server": true, "account": g, "query": "a\x00b"}, "single line"},
		{"folder", map[string]any{"server": true, "account": g, "query": "x", "folder": "INBOX"}, "only account, query"},
		{"from", map[string]any{"server": true, "account": g, "query": "x", "from": "a@b"}, "only account, query"},
		{"since", map[string]any{"server": true, "account": g, "query": "x", "since": "2026-01-01"}, "only account, query"},
		{"until", map[string]any{"server": true, "account": g, "query": "x", "until": "2026-01-01"}, "only account, query"},
		{"fts_syntax", map[string]any{"server": true, "account": g, "query": "x", "fts_syntax": true}, "only account, query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, e.cs, "search", tc.args)
			if !res.IsError {
				t.Fatalf("want a tool error, got %s", text(res))
			}
			msg := text(res)
			if tc.want != "" && !strings.Contains(msg, tc.want) {
				t.Errorf("error %q lacks %q", msg, tc.want)
			}
			noLeak(t, tc.name, msg, e.dir)
		})
	}
	// Validation happens before any connection is made.
	if got := e.fake.log.raw(); len(got) != before {
		t.Errorf("validation errors still talked to the server:\n%s", got[before:])
	}
	// Exactly 512 bytes is accepted past validation (it reaches the server).
	res := call(t, e.cs, "search", map[string]any{"server": true, "account": g, "query": strings.Repeat("a", 512)})
	if res.IsError {
		t.Errorf("512-byte query rejected: %s", text(res))
	}
}

func TestServerSearchWithoutGmailExtension(t *testing.T) {
	cfg := defaultFake()
	cfg.Caps = gfCapsPlain
	e := gmailEnv(t, cfg, Read)
	res := call(t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "from:a"})
	if !res.IsError || !strings.Contains(text(res), "does not support Gmail search") {
		t.Fatalf("isError=%v %q", res.IsError, text(res))
	}
	noLeak(t, "no ext", text(res), e.dir)
	for _, v := range e.fake.log.verbs() {
		if v == "SEARCH" {
			t.Error("a SEARCH was sent to a server without X-GM-EXT-1")
		}
	}
}

func TestServerSearchSlotBusy(t *testing.T) {
	cfg := defaultFake()
	cfg.Hold = make(chan struct{})
	cfg.Entered = make(chan struct{})
	e := gmailEnv(t, cfg, Read)
	args := map[string]any{"server": true, "account": e.g.Name, "query": "from:a"}

	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		res, err := e.cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: args})
		if err != nil {
			t.Errorf("first search: %v", err)
		}
		done <- res
	}()
	select {
	case <-cfg.Entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first search never reached the server")
	}
	res := call(t, e.cs, "search", args)
	if !res.IsError || !strings.Contains(text(res), "already in progress") {
		t.Errorf("second search: isError=%v %q", res.IsError, text(res))
	}
	noLeak(t, "busy", text(res), e.dir)
	// list_folders live shares the slot too.
	res = call(t, e.cs, "list_folders", map[string]any{"account": e.g.Name, "live": true})
	if !res.IsError || !strings.Contains(text(res), "in progress") {
		t.Errorf("live list_folders while busy: isError=%v %q", res.IsError, text(res))
	}
	e.fake.release()
	select {
	case r := <-done:
		if r == nil || r.IsError {
			t.Errorf("first search failed: %v", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first search never finished")
	}
	// The slot is free again.
	if res := call(t, e.cs, "search", args); res.IsError {
		t.Errorf("after release: %s", text(res))
	}
}

// sent returns the bytes the client sent after offset.
func (e *gmEnv) sent(offset int) string { return e.fake.log.raw()[offset:] }

func TestServerSearchSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		wire  func(q string) string // the exact tail the SEARCH command must have
	}{
		{"ascii with quotes", `from:"bob" -label:spam has:attachment`,
			func(q string) string { return `UID SEARCH X-GM-RAW "from:\"bob\" -label:spam has:attachment"` + "\r\n" }},
		{"non-ascii as literal", `subject:café "ünï"`,
			func(q string) string {
				return fmt.Sprintf("UID SEARCH CHARSET UTF-8 X-GM-RAW {%d}\r\n%s\r\n", len(q), q)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := gmailEnv(t, defaultFake(), Read)
			off := len(e.fake.log.raw())
			before, _ := e.st.Status(context.Background())

			out, _ := ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": tc.query})

			sent := e.sent(off)
			if !strings.Contains(sent, tc.wire(tc.query)) {
				t.Errorf("query not sent verbatim.\nwant tail %q\nsent:\n%q", tc.wire(tc.query), sent)
			}
			// Results: the cached UIDs of All Mail, newest first, rest counted.
			if len(out.Results) != 2 || out.UncachedCount != 2 || (out.NextCursor != "") {
				t.Fatalf("out = %+v", out)
			}
			if out.Results[0].Subject != "beta" || out.Results[1].Subject != "alpha" {
				t.Errorf("order = %q, %q; want beta (newer) then alpha", out.Results[0].Subject, out.Results[1].Subject)
			}
			if out.Results[0].StableID != "gm:222" || out.Results[1].StableID != "gm:111" {
				t.Errorf("stable ids = %q, %q", out.Results[0].StableID, out.Results[1].StableID)
			}
			for _, r := range out.Results {
				if r.Snippet != "" {
					t.Errorf("server search returned a snippet: %q", r.Snippet)
				}
			}
			// gm:111 is in INBOX as well as All Mail.
			if got := strings.Join(out.Results[1].Folders, ","); !strings.Contains(got, "INBOX") || !strings.Contains(got, "[Gmail]/All Mail") {
				t.Errorf("folders of gm:111 = %v", out.Results[1].Folders)
			}
			if out.Notice == "" {
				t.Error("untrusted-content notice missing")
			}

			// Read-only on the wire: EXAMINE, never SELECT, nothing that writes.
			var after []string
			for _, ln := range strings.Split(sent, "\r\n") {
				if gfTag.MatchString(ln) {
					f := strings.Fields(ln)
					v := strings.ToUpper(f[1])
					if v == "UID" {
						v = strings.ToUpper(f[2])
					}
					after = append(after, v)
				}
			}
			allowed := map[string]bool{"LOGIN": true, "CAPABILITY": true, "LIST": true, "EXAMINE": true, "SEARCH": true, "LOGOUT": true}
			seen := map[string]bool{}
			for _, v := range after {
				seen[v] = true
				if !allowed[v] {
					t.Errorf("search sent unexpected %s", v)
				}
			}
			if !seen["EXAMINE"] || !seen["SEARCH"] || seen["SELECT"] {
				t.Errorf("verbs = %v; want EXAMINE and SEARCH, no SELECT", after)
			}
			if !strings.Contains(sent, `EXAMINE "[Gmail]/All Mail"`) {
				t.Errorf("did not examine All Mail:\n%s", sent)
			}
			for _, bad := range []string{" STORE ", " COPY ", " MOVE ", " EXPUNGE", " APPEND ", " CREATE ", " DELETE "} {
				if strings.Contains(strings.ToUpper(sent), bad) {
					t.Errorf("search sent%s", bad)
				}
			}
			// Nothing was fetched or written to the cache.
			if seen["FETCH"] {
				t.Error("search fetched messages")
			}
			afterSt, _ := e.st.Status(context.Background())
			if fmt.Sprint(before) != fmt.Sprint(afterSt) {
				t.Errorf("cache changed by a server search:\n%v\n%v", before, afterSt)
			}
		})
	}
}

func TestServerSearchUsesAllFolderAndFallback(t *testing.T) {
	// LIST marks a differently named folder \All: that one is examined.
	cfg := defaultFake()
	cfg.Folders[1].Name = "[Gmail]/Alle Nachrichten"
	e := gmailEnv(t, cfg, Read)
	off := len(e.fake.log.raw())
	out, _ := ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "x"})
	if !strings.Contains(e.sent(off), `EXAMINE "[Gmail]/Alle Nachrichten"`) || len(out.Results) != 2 {
		t.Errorf("\\All folder not used: count=%d sent=%q", len(out.Results), e.sent(off))
	}

	// No \All attribute: falls back to [Gmail]/All Mail.
	cfg = defaultFake()
	cfg.Folders[1].All = false
	e = gmailEnv(t, cfg, Read)
	off = len(e.fake.log.raw())
	out, _ = ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "x"})
	if !strings.Contains(e.sent(off), `EXAMINE "[Gmail]/All Mail"`) || len(out.Results) != 2 {
		t.Errorf("fallback not used: count=%d sent=%q", len(out.Results), e.sent(off))
	}
}

func TestServerSearchLimitAndEmptyResult(t *testing.T) {
	e := gmailEnv(t, defaultFake(), Read)
	out, _ := ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "x", "limit": 1})
	if len(out.Results) != 1 || !(out.NextCursor != "") || out.Results[0].Subject != "beta" || out.UncachedCount != 2 {
		t.Errorf("limit 1: %+v", out)
	}

	cfg := defaultFake()
	cfg.Search = nil
	e = gmailEnv(t, cfg, Read)
	out, raw := ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "x"})
	if len(out.Results) != 0 || (out.NextCursor != "") || out.UncachedCount != 0 || !strings.Contains(raw, `"hits":[]`) {
		t.Errorf("empty: %s", raw)
	}
}

func TestServerSearchCapsUIDsAtTwentyThousand(t *testing.T) {
	cfg := defaultFake()
	cfg.Search = nil
	for u := uint32(1); u <= 20001; u++ {
		cfg.Search = append(cfg.Search, u)
	}
	// 77 and 78 are cached in All Mail and are inside the newest 20000.
	e := gmailEnv(t, cfg, Read)
	out, _ := ok[gmSearchOut](t, e.cs, "search", map[string]any{"server": true, "account": e.g.Name, "query": "x", "limit": 500})
	if len(out.Results) != 2 || out.Note == "" || out.UncachedCount != 20000-2 {
		t.Errorf("capped: count=%d truncated=%v uncached=%d, want 2 true 19998", len(out.Results), (out.NextCursor != ""), out.UncachedCount)
	}
}

func TestSearchToolStillReadOnlyAndCacheSearchHasNoUncachedCount(t *testing.T) {
	for _, mode := range []Mode{Read, Admin} {
		t.Run(mode.String(), func(t *testing.T) {
			e := gmailEnv(t, defaultFake(), mode)
			res, err := e.cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var found *mcp.Tool
			for _, tl := range res.Tools {
				if tl.Name == "search" {
					found = tl
				}
			}
			if found == nil {
				t.Fatal("search not registered")
			}
			if found.Annotations == nil || !found.Annotations.ReadOnlyHint {
				t.Error("search lacks ReadOnlyHint")
			}
			if b, _ := json.Marshal(found.InputSchema); !strings.Contains(string(b), `"server"`) {
				t.Errorf("search input schema lacks server: %s", b)
			}

			// A cache search (server not set) never carries uncached_count.
			for _, args := range []map[string]any{
				{"account": e.g.Name, "query": "alpha"},
				{"query": "zzzznomatch"},
				{"account": e.g.Name, "query": "alpha", "server": false},
			} {
				_, raw := ok[gmSearchOut](t, e.cs, "search", args)
				if strings.Contains(raw, "uncached_count") {
					t.Errorf("cache search %v output has uncached_count: %s", args, raw)
				}
			}
			// And it never contacts the server.
			off := len(e.fake.log.raw())
			ok[gmSearchOut](t, e.cs, "search", map[string]any{"account": e.g.Name, "query": "alpha"})
			if got := e.sent(off); got != "" {
				t.Errorf("cache search talked to the server: %q", got)
			}
		})
	}
}
