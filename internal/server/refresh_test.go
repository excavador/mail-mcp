package server

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
)

// 2. parseMaxAge.
func TestParseMaxAge(t *testing.T) {
	for _, tc := range []struct {
		in       string
		want     time.Duration
		note     string // substring; "" means no note
		wantFail bool
	}{
		{"", 5 * time.Minute, "", false},
		{"  ", 5 * time.Minute, "", false},
		{"10s", 30 * time.Second, "30s", false},
		{"48h", 24 * time.Hour, "24h", false},
		{"5m", 5 * time.Minute, "", false},
		{"30s", 30 * time.Second, "", false},
		{"24h", 24 * time.Hour, "", false},
		{"-1m", 30 * time.Second, "30s", false},
		{"0", 30 * time.Second, "30s", false},
		{"bogus", 0, "", true},
		{"5", 0, "", true},
	} {
		got, note, err := parseMaxAge(tc.in)
		if tc.wantFail {
			if err == nil {
				t.Errorf("%q: want error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.in, got, tc.want)
		}
		if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
			t.Errorf("%q: note = %q, want it to contain %q", tc.in, note, tc.note)
		}
	}
}

// 3. resolveAccount.
func TestResolveAccount(t *testing.T) {
	accts := []accounts.Account{
		{Name: "work", Username: "Oleg@Work.Example", Aliases: []string{"o@work.example", "@work.example"}},
		{Name: "home", Username: "oleg@home.example", Aliases: []string{"me@home.example"}},
		{Name: "twin1", Username: "shared@x.example"},
		{Name: "twin2", Username: "shared@x.example"},
	}
	for _, tc := range []struct {
		key, want string
	}{
		{"work", "work"},
		{"  WORK ", "work"},
		{"oleg@work.example", "work"},
		{"OLEG@WORK.EXAMPLE", "work"},
		{"o@work.example", "work"},
		{"me@home.example", "home"},
		{"Me@Home.Example", "home"},
	} {
		a, err := resolveAccount(accts, tc.key)
		if err != nil || a.Name != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.key, a.Name, err, tc.want)
		}
	}

	for _, key := range []string{"", "   ", "@work.example", "@home.example", "@"} {
		if a, err := resolveAccount(accts, key); err == nil {
			t.Errorf("%q: resolved to %q, want error", key, a.Name)
		}
	}

	_, err := resolveAccount(accts, "nobody")
	if err == nil || !strings.Contains(err.Error(), "unknown account") {
		t.Fatalf("unknown: %v", err)
	}
	for _, leak := range []string{"work", "home", "twin", "example"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("unknown-account error leaks %q: %v", leak, err)
		}
	}

	_, err = resolveAccount(accts, "shared@x.example")
	if err == nil || !strings.Contains(err.Error(), "more than one account") {
		t.Fatalf("ambiguous: %v", err)
	}
	for _, leak := range []string{"twin1", "twin2"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("ambiguous error names accounts (%q): %v", leak, err)
		}
	}
	// A name beats a login that another account shares.
	if a, err := resolveAccount(accts, "twin1"); err != nil || a.Name != "twin1" {
		t.Errorf("name lookup: %q, %v", a.Name, err)
	}
}

// 10. registration.
func TestRefreshCacheRegisteredInBothModes(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, mode := range []Mode{Read, Admin} {
		t.Run(mode.String(), func(t *testing.T) {
			cs := connectServer(t, New(nil, c, "test", mode))
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			have := map[string]*mcp.Tool{}
			for _, tl := range res.Tools {
				have[tl.Name] = tl
			}
			rc := have["refresh_cache"]
			if rc == nil {
				t.Fatal("refresh_cache not registered")
			}
			if rc.Annotations == nil || !rc.Annotations.ReadOnlyHint || !rc.Annotations.IdempotentHint {
				t.Errorf("refresh_cache annotations = %+v, want read-only and idempotent", rc.Annotations)
			}
			cs2 := have["cache_status"]
			if cs2 == nil || !strings.Contains(cs2.Description, "refresh_cache") {
				t.Errorf("cache_status description does not mention refresh_cache: %v", cs2)
			}
		})
	}
}

type refreshResult struct {
	Account            string  `json:"account"`
	Refreshed          any     `json:"refreshed"`
	Reason             string  `json:"reason"`
	MaxAge             string  `json:"max_age"`
	MaxAgeNote         string  `json:"max_age_note"`
	StartedAt          *string `json:"started_at"`
	NewMessages        *int    `json:"new_messages"`
	LastRefresh        *string `json:"last_refresh"`
	LastRefreshOK      bool    `json:"last_refresh_ok"`
	JoinedRunningCycle bool    `json:"joined_running_refresh"`
	Error              string  `json:"error"`
}

func TestRefreshCacheToolEndToEnd(t *testing.T) {
	e := newEnv(t, "INBOX")
	e.add("INBOX", "a", "Alice <alice@example.com>", "one", "alpha", t0)
	for _, mode := range []Mode{Read, Admin} {
		t.Run(mode.String(), func(t *testing.T) {
			cs := e.connect(mode, e.accts)
			// Each mode sees the same cache, so use a different account per mode
			// to start from "no row".
			name := map[Mode]string{Read: "acct", Admin: "other"}[mode]
			o, raw := ok[refreshResult](t, cs, "refresh_cache", map[string]any{"account": name, "max_age": "10s"})
			if o.Refreshed != true || o.Account != name || o.StartedAt == nil || o.NewMessages == nil || *o.NewMessages != 1 {
				t.Fatalf("first call: %s", raw)
			}
			if o.MaxAge != "30s" || !strings.Contains(o.MaxAgeNote, "30s") || !o.LastRefreshOK || o.Error != "" {
				t.Fatalf("first call clamp/ok: %s", raw)
			}
			o, raw = ok[refreshResult](t, cs, "refresh_cache", map[string]any{"account": name})
			if o.Refreshed != false || o.Reason != "fresh" || o.StartedAt != nil || o.MaxAge != "5m0s" || !o.LastRefreshOK || o.LastRefresh == nil {
				t.Fatalf("second call: %s", raw)
			}
		})
	}
}

func TestRefreshCacheToolRejectsBadInput(t *testing.T) {
	e := newEnv(t, "INBOX")
	cs := e.connect(Read, e.accts)
	for _, args := range []map[string]any{
		{"account": "nope"},
		{"account": ""},
		{"account": "@example.com"},
		{"account": "u"}, // both accounts log in as u
		{"account": "acct", "max_age": "bogus"},
	} {
		res := call(t, cs, "refresh_cache", args)
		if !res.IsError {
			t.Errorf("%v: want a tool error, got %s", args, text(res))
			continue
		}
		msg := text(res)
		if args["account"] == "nope" && (strings.Contains(msg, "other") || strings.Contains(msg, "acct")) {
			t.Errorf("unknown-account error leaks account names: %s", msg)
		}
	}
	if st, _ := e.cache.Status(context.Background()); len(st) != 0 {
		t.Errorf("rejected calls must not touch the cache: %+v", st)
	}
}

// A refresh that cannot connect reports the fixed phrase, not the dial error.
func TestRefreshCacheToolFailureHidesHost(t *testing.T) {
	e := newEnv(t, "INBOX")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	port, _ := strconv.Atoi(p)
	down := e.load(port, "down")
	cs := e.connect(Read, down)
	o, raw := ok[refreshResult](t, cs, "refresh_cache", map[string]any{"account": "down"})
	if o.Refreshed != true || o.Error != "refresh failed (see server log)" || o.LastRefreshOK {
		t.Fatalf("result: %s", raw)
	}
	for _, leak := range []string{"127.0.0.1", p, "connect", "refused"} {
		if strings.Contains(raw, leak) {
			t.Errorf("result leaks %q: %s", leak, raw)
		}
	}
}

// A refresh that outlasts refreshWait answers in_progress; the next call
// joins it rather than starting another.
func TestRefreshCacheToolInProgressThenJoin(t *testing.T) {
	e := newEnv(t, "INBOX")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 8)
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
			accepted <- struct{}{}
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
	slow := e.load(port, "slow")

	old := refreshWait
	refreshWait = 50 * time.Millisecond
	t.Cleanup(func() { refreshWait = old })

	cs := e.connect(Read, slow)
	o, raw := ok[refreshResult](t, cs, "refresh_cache", map[string]any{"account": "slow"})
	if o.Refreshed != "in_progress" || o.JoinedRunningCycle {
		t.Fatalf("first: %s", raw)
	}
	select {
	case <-accepted:
	case <-time.After(20 * time.Second):
		t.Fatal("the refresh never dialled")
	}
	o, raw = ok[refreshResult](t, cs, "refresh_cache", map[string]any{"account": "slow"})
	if o.Refreshed != "in_progress" || !o.JoinedRunningCycle {
		t.Fatalf("second: %s", raw)
	}
	select {
	case <-accepted:
		t.Fatal("second call started another refresh")
	default:
	}
}
