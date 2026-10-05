package server

// Harness for the write tools: an in-process IMAP server that can advertise
// MOVE or not, records every command the client sends, and can hold or kill a
// connection when a given command arrives. Everything is bounded.

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/imapx"
	"github.com/excavador/mail-mcp/internal/organise"
)

type wspec struct {
	name string
	p    accounts.Provider
}

type wlog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *wlog) write(p []byte) { l.mu.Lock(); l.buf.Write(p); l.mu.Unlock() }
func (l *wlog) reset()         { l.mu.Lock(); l.buf.Reset(); l.mu.Unlock() }

func (l *wlog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range strings.Split(l.buf.String(), "\r\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// verbs: the command name of every logged line ("UID MOVE" is "MOVE").
func (l *wlog) verbs() []string {
	var out []string
	for _, ln := range l.lines() {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		v := strings.ToUpper(f[1])
		if v == "UID" && len(f) > 2 {
			v = strings.ToUpper(f[2])
		}
		out = append(out, v)
	}
	return out
}

func (l *wlog) count(verb string) int {
	n := 0
	for _, v := range l.verbs() {
		if v == verb {
			n++
		}
	}
	return n
}

type wConn struct {
	net.Conn
	e *wenv
}

func (c wConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.e.log.write(p[:n])
		c.e.hookMu.Lock()
		h := c.e.hook
		c.e.hookMu.Unlock()
		if h != nil {
			if herr := h(p[:n]); herr != nil {
				_ = c.Conn.Close()
				return 0, herr
			}
		}
	}
	return n, err
}

// Write rewrites the server's LIST replies so chosen folders carry a
// special-use attribute (imapmemserver cannot set one on a folder).
func (c wConn) Write(p []byte) (int, error) {
	c.e.hookMu.Lock()
	attrs := c.e.attrs
	c.e.hookMu.Unlock()
	if len(attrs) == 0 || !bytes.HasPrefix(p, []byte("* LIST (")) {
		return c.Conn.Write(p)
	}
	out := p
	for name, attr := range attrs {
		for _, q := range []string{`"` + name + `"`, name} {
			suffix := []byte(` "/" ` + q + "\r\n")
			if bytes.HasSuffix(p, suffix) {
				if bytes.HasPrefix(p, []byte("* LIST () ")) {
					out = bytes.Replace(p, []byte("* LIST ("), []byte("* LIST ("+attr), 1)
				} else {
					out = bytes.Replace(p, []byte("* LIST ("), []byte("* LIST ("+attr+" "), 1)
				}
			}
		}
	}
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

type wListener struct {
	net.Listener
	e *wenv
}

func (l wListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return wConn{Conn: c, e: l.e}, nil
}

type wenv struct {
	t       *testing.T
	dir     string
	host    string
	port    int
	pin     string
	user    *imapmemserver.User
	cache   *cache.Cache
	log     *wlog
	hookMu  sync.Mutex
	hook    func([]byte) error
	attrs   map[string]string
	accts   []accounts.Account
	org     *organise.Organiser
	hist    *history.Store
	histDir string
}

func (e *wenv) setAttrs(m map[string]string) {
	e.hookMu.Lock()
	e.attrs = m
	e.hookMu.Unlock()
}

func (e *wenv) setHook(h func([]byte) error) {
	e.hookMu.Lock()
	e.hook = h
	e.hookMu.Unlock()
}

// newWEnv starts the server. withMove says whether it advertises MOVE.
func newWEnv(t *testing.T, withMove bool, specs []wspec, folders ...string) *wenv {
	t.Helper()
	cert, pin := selfSigned(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("u", "pw")
	mem.AddUser(user)
	for _, f := range folders {
		if err := user.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}}
	if withMove {
		caps[imap.CapMove] = struct{}{}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	raw, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	e := &wenv{t: t, pin: pin, user: user, log: &wlog{}, dir: t.TempDir()}
	go func() { _ = srv.Serve(wListener{Listener: raw, e: e}) }()
	t.Cleanup(func() { _ = srv.Close() })
	h, p, _ := net.SplitHostPort(raw.Addr().String())
	e.host = h
	e.port, _ = strconv.Atoi(p)

	pw := filepath.Join(e.dir, "pw")
	if err := os.WriteFile(pw, []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	y := "accounts:\n"
	for _, s := range specs {
		y += fmt.Sprintf("  - name: %s\n    provider: %s\n    host: %s\n    port: %d\n    tls: implicit\n    username: u\n    passwordFile: %s\n    pinnedCertSHA256: %s\n",
			s.name, s.p, e.host, e.port, pw, pin)
	}
	cfg := filepath.Join(e.dir, "accounts.yaml")
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.accts, err = accounts.Load(cfg); err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(filepath.Join(e.dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	e.cache = c
	if e.org, err = organise.New(c); err != nil {
		t.Fatal(err)
	}
	e.histDir = filepath.Join(e.dir, "hist")
	if e.hist, err = history.Open(e.histDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.hist.Close() })
	return e
}

var gmailPair = []wspec{{"acct", accounts.Gmail}, {"other", accounts.Gmail}}

func (e *wenv) acct(name string) accounts.Account {
	for _, a := range e.accts {
		if a.Name == name {
			return a
		}
	}
	e.t.Fatalf("no account %s", name)
	return accounts.Account{}
}

func (e *wenv) ctx() context.Context {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	e.t.Cleanup(cancel)
	return ctx
}

func (e *wenv) dial(name string) *imapclient.Client {
	e.t.Helper()
	c, err := imapx.Dial(e.ctx(), e.acct(name))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = c.Close() })
	return c
}

// addAt appends one message to folder with internal date date.
func (e *wenv) addAt(name, folder, id, from, subject string, date time.Time, extra ...string) {
	e.t.Helper()
	c := e.dial(name)
	e.appendTo(c, folder, mkRaw(id, from, subject, "body "+id, date, extra...), date)
}

func (e *wenv) add(folder, id, from, subject string) {
	e.t.Helper()
	e.addAt("acct", folder, id, from, subject, t0)
}

func (e *wenv) appendTo(c *imapclient.Client, folder string, raw []byte, date time.Time) {
	e.t.Helper()
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Time: date})
	if _, err := cmd.Write(raw); err != nil {
		e.t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		e.t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		e.t.Fatal(err)
	}
}

// addMany appends n messages from from on one connection.
func (e *wenv) addMany(folder, from string, n int) {
	e.t.Helper()
	c := e.dial("acct")
	for i := range n {
		id := fmt.Sprintf("bulk%d", i)
		e.appendTo(c, folder, mkRaw(id, from, "bulk "+id, "b", t0.Add(time.Duration(i)*time.Second)), t0.Add(time.Duration(i)*time.Second))
	}
}

// removeUID deletes one message server side (test setup; the log is reset after).
func (e *wenv) removeBySubject(folder, subject string) {
	e.t.Helper()
	c := e.dial("acct")
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		e.t.Fatal(err)
	}
	ids, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: subject}}}, nil).Wait()
	if err != nil || len(ids.AllUIDs()) == 0 {
		e.t.Fatalf("find %q: %v %v", subject, ids, err)
	}
	set := imap.UIDSetNum(ids.AllUIDs()...)
	if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *wenv) refresh(name string) {
	e.t.Helper()
	c := e.dial(name)
	if _, err := e.cache.Refresh(e.ctx(), e.acct(name), c); err != nil {
		e.t.Fatalf("Refresh: %v", err)
	}
}

func (e *wenv) serverCount(folder string) int {
	e.t.Helper()
	c := e.dial("acct")
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		e.t.Fatalf("select %s: %v", folder, err)
	}
	return int(sel.NumMessages)
}

func (e *wenv) db() *sql.DB {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dir, "cache", "index.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = db.Close() })
	return db
}

// members lists the subjects the cache says are in folder, sorted.
func (e *wenv) members(account, folder string) []string {
	e.t.Helper()
	rows, err := e.db().Query(`SELECT m.subject FROM membership s JOIN messages m ON m.account = s.account AND m.stable_id = s.stable_id WHERE s.account = ? AND s.folder = ?`, account, folder)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (e *wenv) uids(account, folder string) []int {
	e.t.Helper()
	rows, err := e.db().Query(`SELECT uid FROM membership WHERE account = ? AND folder = ? ORDER BY uid`, account, folder)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var u int
		_ = rows.Scan(&u)
		out = append(out, u)
	}
	return out
}

// connect returns an MCP client on a server in mode, built with opts. elicit
// nil means the client does not declare the elicitation capability.
func (e *wenv) connect(mode Mode, elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error), opts ...Option) *mcp.ClientSession {
	e.t.Helper()
	// From protocol 2026-07-28 a server may not send elicitation/create in
	// the middle of a request, so an elicitation client is pinned below it.
	return e.connectP(mode, elicit, elicit != nil, opts...)
}

// noDiscover makes the server answer server/discover as an old server would,
// so the client falls back to the initialize handshake (protocol 2025-11-25).
func noDiscover(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "server/discover" {
			return nil, errors.New("method not found")
		}
		return next(ctx, method, req)
	}
}

func (e *wenv) connectP(mode Mode, elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error), old bool, opts ...Option) *mcp.ClientSession {
	e.t.Helper()
	s := New(e.accts, e.cache, "test", mode, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	e.t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	if old {
		s.AddReceivingMiddleware(noDiscover)
	}
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = ss.Close() })
	var co *mcp.ClientOptions
	if elicit != nil {
		co = &mcp.ClientOptions{ElicitationHandler: elicit}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, co).Connect(ctx, ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// admin is a full admin server and a client without elicitation.
func (e *wenv) admin() *mcp.ClientSession {
	return e.connect(Admin, nil, WithHistory(e.hist), WithOrganiser(e.org))
}

func acceptConfirm(*testing.T) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}
}

// ---- hooks ----

var uidMoveRE = regexp.MustCompile(`(?i) UID MOVE `)

func countRE(re *regexp.Regexp, p []byte) int { return len(re.FindAll(p, -1)) }

// killOnNth closes the connection, before the server sees it, when the nth
// command matching re arrives.
func killOnNth(re *regexp.Regexp, n int) func([]byte) error {
	var mu sync.Mutex
	seen := 0
	return func(p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		seen += countRE(re, p)
		if seen >= n {
			return io.ErrClosedPipe
		}
		return nil
	}
}

// holdFirst blocks the connection that sends the first command matching re
// until release is closed; reached is closed when it is blocked.
func holdFirst(re *regexp.Regexp, reached, release chan struct{}) func([]byte) error {
	var once sync.Once
	return func(p []byte) error {
		if !re.Match(p) {
			return nil
		}
		hold := false
		once.Do(func() { hold = true })
		if hold {
			close(reached)
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
		}
		return nil
	}
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ---- tool helpers ----

type prevT struct {
	Notice       string `json:"notice"`
	PreviewToken string `json:"preview_token"`
	Account      string `json:"account"`
	Kind         string `json:"kind"`
	Action       string `json:"action"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	Matched      int    `json:"matched"`
	NotFound     int    `json:"not_found"`
	MoveBack     int    `json:"move_back"`
	CopyBack     int    `json:"copy_back"`
	Sampled      int    `json:"sampled"`
	Untrusted    struct {
		Samples []struct {
			StableID string `json:"stable_id"`
			From     string `json:"from"`
			Subject  string `json:"subject"`
		} `json:"samples"`
	} `json:"untrusted"`
}

type applyT struct {
	Matched    int    `json:"matched"`
	Done       int    `json:"done"`
	Skipped    int    `json:"skipped"`
	ApprovedBy string `json:"approved_by"`
	HistoryID  string `json:"history_id"`
}

type histT struct {
	Count   int `json:"count"`
	Records []struct {
		ID              string `json:"id"`
		Account         string `json:"account"`
		Kind            string `json:"kind"`
		Action          string `json:"action"`
		TouchedCount    int    `json:"touched_count"`
		AlreadyInTarget int    `json:"already_in_target"`
		CopiedBack      int    `json:"copied_back"`
		Skipped         int    `json:"skipped"`
		Error           string `json:"error"`
		Undoes          string `json:"undoes"`
		Reapplies       string `json:"reapplies"`
		Preview         struct {
			Matched    int    `json:"matched"`
			ApprovedBy string `json:"approved_by"`
		} `json:"preview"`
		Untrusted struct {
			Intent *struct {
				Criterion map[string]any `json:"criterion"`
				Target    string         `json:"target"`
			} `json:"intent"`
			Target string `json:"target"`
		} `json:"untrusted"`
	} `json:"records"`
}

func fromCrit(from string) map[string]any { return map[string]any{"from": from} }

func previewArgs(account, folder string, crit map[string]any, target, action string) map[string]any {
	c := map[string]any{"folder": folder}
	for k, v := range crit {
		c[k] = v
	}
	return map[string]any{"account": account, "criterion": c, "target": target, "action": action}
}

func preview(t *testing.T, cs *mcp.ClientSession, account, folder string, crit map[string]any, target, action string) prevT {
	t.Helper()
	p, _ := ok[prevT](t, cs, "preview_intent", previewArgs(account, folder, crit, target, action))
	return p
}

// applyArgs is the one place that knows what apply_intent is called with: the
// token, the approval and the echo of what the owner was shown.
func applyArgs(p prevT) map[string]any {
	return map[string]any{
		"preview_token": p.PreviewToken, "approved": true,
		"expect_account": p.Account, "expect_action": p.Action,
		"expect_source": p.Source, "expect_target": p.Target, "expect_matched": p.Matched,
	}
}

func apply(t *testing.T, cs *mcp.ClientSession, p prevT) applyT {
	t.Helper()
	a, _ := ok[applyT](t, cs, "apply_intent", applyArgs(p))
	return a
}

func listHistory(t *testing.T, cs *mcp.ClientSession, account string) (histT, string) {
	t.Helper()
	args := map[string]any{}
	if account != "" {
		args["account"] = account
	}
	return ok[histT](t, cs, "list_history", args)
}

// noWrites fails if the client sent anything that changes a mailbox.
func (e *wenv) noWrites(t *testing.T) {
	t.Helper()
	for _, v := range e.log.verbs() {
		switch v {
		case "MOVE", "COPY", "STORE", "EXPUNGE", "DELETE", "RENAME", "CREATE", "APPEND":
			t.Errorf("client sent %s; log: %q", v, e.log.lines())
		}
	}
}
