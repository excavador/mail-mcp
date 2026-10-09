package calendar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const (
	loginRefresh = "1//0gLOGIN-REFRESH-TOKEN-xyz"
	loginAccess  = "ya29.LOGIN-ACCESS-TOKEN"
)

// urlWriter is a stderr that hands over the authorization URL once printed.
type urlWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	ch   chan string
}

func (w *urlWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	s := w.buf.String()
	w.mu.Unlock()
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "https://") {
			w.once.Do(func() { w.ch <- ln })
		}
	}
	return len(p), nil
}

func (w *urlWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

type tokenServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	forms    []url.Values
	refresh  string // refresh_token to return
	scope    string // scope to return
	failWith string // OAuth error code to return with 400
}

func newTokenServer(t *testing.T, scope string) *tokenServer {
	t.Helper()
	ts := &tokenServer{refresh: loginRefresh, scope: scope}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.forms = append(ts.forms, r.PostForm)
		fail, ref, sc := ts.failWith, ts.refresh, ts.scope
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail != "" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fail, "error_description": "bad " + loginRefresh})
			return
		}
		out := map[string]any{"access_token": loginAccess, "token_type": "Bearer", "expires_in": 3600, "scope": sc}
		if ref != "" {
			out["refresh_token"] = ref
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) calls() []url.Values {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]url.Values(nil), ts.forms...)
}

type loginRun struct {
	conf   *oauth2.Config
	out    string
	stdout *bytes.Buffer
	stderr *urlWriter
	done   chan error
	auth   *url.URL
}

// startLogin runs login() against a fake token endpoint and waits (no sleeping)
// for the authorization URL.
func startLogin(t *testing.T, ts *tokenServer, scopes []string, outPath string, timeout time.Duration) *loginRun {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conf := &oauth2.Config{
		ClientID: cid, ClientSecret: csecret, Scopes: scopes,
		Endpoint:    oauth2.Endpoint{AuthURL: "https://accounts.example/o/oauth2/auth", TokenURL: ts.srv.URL + "/token"},
		RedirectURL: "http://" + ln.Addr().String() + "/",
	}
	r := &loginRun{conf: conf, out: outPath, stdout: &bytes.Buffer{}, stderr: &urlWriter{ch: make(chan string, 1)}, done: make(chan error, 1)}
	go func() { r.done <- login(context.Background(), conf, ln, outPath, timeout, r.stderr, r.stdout) }()
	select {
	case raw := <-r.stderr.ch:
		if r.auth, err = url.Parse(raw); err != nil {
			t.Fatal(err)
		}
	case err := <-r.done:
		t.Fatalf("login ended before printing the URL: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no authorization URL")
	}
	return r
}

func (r *loginRun) callback(t *testing.T, params url.Values) int {
	t.Helper()
	resp, err := http.Get(r.conf.RedirectURL + "?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (r *loginRun) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("login did not finish")
		return nil
	}
}

func (r *loginRun) good(code string) url.Values {
	return url.Values{"state": {r.auth.Query().Get("state")}, "code": {code}}
}

func notLeaked(t *testing.T, r *loginRun) {
	t.Helper()
	for _, s := range []string{loginRefresh, loginAccess, csecret} {
		if strings.Contains(r.stdout.String(), s) || strings.Contains(r.stderr.String(), s) {
			t.Errorf("%q printed:\nstdout=%q\nstderr=%q", s, r.stdout.String(), r.stderr.String())
		}
	}
}

func TestLoginSavesTheRefreshTokenAndPrintsOnlySaved(t *testing.T) {
	scopes := []string{ScopeReadonly, ScopeEvents}
	ts := newTokenServer(t, strings.Join(scopes, " "))
	out := filepath.Join(t.TempDir(), "token")
	r := startLogin(t, ts, scopes, out, time.Minute)

	q := r.auth.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" ||
		q.Get("response_type") != "code" || q.Get("client_id") != cid || q.Get("redirect_uri") != r.conf.RedirectURL ||
		q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("scope") != strings.Join(scopes, " ") {
		t.Errorf("auth URL = %s", r.auth)
	}
	if strings.Contains(r.auth.String(), csecret) {
		t.Error("the client secret is in the authorization URL")
	}
	if !strings.HasPrefix(r.conf.RedirectURL, "http://127.0.0.1:") {
		t.Errorf("redirect = %s", r.conf.RedirectURL)
	}

	if code := r.callback(t, r.good("the-code")); code != 200 {
		t.Errorf("callback status %d", code)
	}
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(out)
	if err != nil || string(b) != loginRefresh+"\n" {
		t.Fatalf("token file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if r.stdout.String() != "saved\n" {
		t.Errorf("stdout = %q", r.stdout.String())
	}
	notLeaked(t, r)

	forms := ts.calls()
	if len(forms) != 1 || forms[0].Get("code") != "the-code" || forms[0].Get("grant_type") != "authorization_code" {
		t.Fatalf("token requests = %v", forms)
	}
	sum := sha256.Sum256([]byte(forms[0].Get("code_verifier")))
	if forms[0].Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Errorf("the verifier does not match the challenge")
	}
	if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 1 {
		t.Errorf("leftover files beside the token: %v", ents)
	}
}

func TestLoginStateMismatchIsRefusedAndDoesNotEndTheFlow(t *testing.T) {
	ts := newTokenServer(t, ScopeReadonly)
	out := filepath.Join(t.TempDir(), "token")
	r := startLogin(t, ts, []string{ScopeReadonly}, out, time.Minute)

	if code := r.callback(t, url.Values{"state": {"forged"}, "code": {"evil"}}); code != 400 {
		t.Errorf("forged state status = %d", code)
	}
	if code := r.callback(t, url.Values{"state": {"forged"}, "error": {"access_denied"}}); code != 400 {
		t.Errorf("forged error status = %d", code)
	}
	if code := r.callback(t, url.Values{"code": {"evil"}}); code != 400 {
		t.Errorf("missing state status = %d", code)
	}
	if n := len(ts.calls()); n != 0 {
		t.Fatalf("a forged callback reached the token endpoint (%d calls)", n)
	}
	select {
	case err := <-r.done:
		t.Fatalf("login ended on a forged callback: %v", err)
	default:
	}
	r.callback(t, r.good("real"))
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	if forms := ts.calls(); len(forms) != 1 || forms[0].Get("code") != "real" {
		t.Errorf("token requests = %v", forms)
	}
}

func TestLoginFailuresWriteNothing(t *testing.T) {
	type tc struct {
		name   string
		setup  func(ts *tokenServer)
		params func(r *loginRun) url.Values
		want   string
		calls  int
	}
	for _, c := range []tc{
		{"access_denied", nil, func(r *loginRun) url.Values {
			return url.Values{"state": {r.auth.Query().Get("state")}, "error": {"access_denied"}}
		}, "access_denied", 0},
		{"error text is sanitised", nil, func(r *loginRun) url.Values {
			return url.Values{"state": {r.auth.Query().Get("state")}, "error": {"access_denied\nINJECT <b>"}}
		}, "Google refused: access_deniedINJECTb", 0},
		{"no code", nil, func(r *loginRun) url.Values { return url.Values{"state": {r.auth.Query().Get("state")}} }, "no authorization code", 0},
		{"no refresh token", func(ts *tokenServer) { ts.refresh = "" }, func(r *loginRun) url.Values { return r.good("c") }, "no refresh token", 1},
		{"a scope was not granted", func(ts *tokenServer) { ts.scope = ScopeReadonly }, func(r *loginRun) url.Values { return r.good("c") }, "did not grant the scope " + ScopeEvents, 1},
		{"no scope reported", func(ts *tokenServer) { ts.scope = "" }, func(r *loginRun) url.Values { return r.good("c") }, "did not grant the scope", 1},
		{"exchange refused", func(ts *tokenServer) { ts.failWith = "invalid_grant" }, func(r *loginRun) url.Values { return r.good("c") }, "token exchange failed: invalid_grant", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTokenServer(t, ScopeReadonly+" "+ScopeEvents)
			if c.setup != nil {
				c.setup(ts)
			}
			dir := t.TempDir()
			out := filepath.Join(dir, "token")
			r := startLogin(t, ts, []string{ScopeReadonly, ScopeEvents}, out, time.Minute)
			r.callback(t, c.params(r))
			err := r.result(t)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if _, serr := os.Stat(out); !os.IsNotExist(serr) {
				t.Errorf("a token file exists after a failure: %v", serr)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("files left behind: %v", ents)
			}
			if strings.Contains(r.stdout.String(), "saved") || r.stdout.Len() != 0 {
				t.Errorf("stdout = %q", r.stdout.String())
			}
			if strings.Contains(err.Error(), loginRefresh) || strings.Contains(err.Error(), loginAccess) {
				t.Errorf("error leaks a token: %v", err)
			}
			notLeaked(t, r)
			// oauth2 may retry a refused exchange with the other client-auth style.
			if n := len(ts.calls()); (c.calls == 0 && n != 0) || (c.calls > 0 && n < c.calls) {
				t.Errorf("%d token requests, want %d", n, c.calls)
			}
		})
	}
}

func TestLoginTimesOutWithoutABrowser(t *testing.T) {
	ts := newTokenServer(t, ScopeReadonly)
	out := filepath.Join(t.TempDir(), "token")
	r := startLogin(t, ts, []string{ScopeReadonly}, out, 100*time.Millisecond)
	if err := r.result(t); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("a token file exists")
	}
}

func TestLoginReplacesAnExistingFileAtomicallyAndRefusesASymlink(t *testing.T) {
	scopes := []string{ScopeReadonly}
	dir := t.TempDir()

	// An existing, more open file is replaced by a 0600 one.
	out := filepath.Join(dir, "token")
	if err := os.WriteFile(out, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := newTokenServer(t, ScopeReadonly)
	r := startLogin(t, ts, scopes, out, time.Minute)
	r.callback(t, r.good("c"))
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	fi, _ := os.Stat(out)
	if string(b) != loginRefresh+"\n" || fi.Mode().Perm() != 0o600 {
		t.Errorf("file = %q mode %v", b, fi.Mode().Perm())
	}

	// A symlink at --out is not written through.
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	ts2 := newTokenServer(t, ScopeReadonly)
	r2 := startLogin(t, ts2, scopes, link, time.Minute)
	r2.callback(t, r2.good("c"))
	err := r2.result(t)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "precious\n" {
		t.Errorf("victim = %q", b)
	}
	if lf, _ := os.Lstat(link); lf.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
	notLeaked(t, r2)
	for _, e := range mustReadDir(t, dir) {
		if strings.Contains(e, ".tmp-") {
			t.Errorf("temporary file left behind: %s", e)
		}
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestLoginCommandArgumentsAndCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var so, se bytes.Buffer
	run := func(args ...string) error {
		so.Reset()
		se.Reset()
		return Login(context.Background(), args, &so, &se)
	}
	if err := run(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("no flags: %v", err)
	}
	id, secret, empty := write("id", cid+"\n"), write("secret", csecret), write("empty", " \n")
	out := filepath.Join(dir, "out")
	if err := run("--client-id-file", filepath.Join(dir, "absent"), "--client-secret-file", secret, "--out", out); err == nil || !strings.Contains(err.Error(), "client-id-file") || strings.Contains(err.Error(), dir) {
		t.Errorf("missing id file: %v", err)
	}
	if err := run("--client-id-file", id, "--client-secret-file", empty, "--out", out); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty secret file: %v", err)
	}
	if err := run("--bogus"); err == nil {
		t.Error("unknown flag accepted")
	}
	// A short timeout ends a real run at once; the URL asks for what --write says.
	if err := run("--client-id-file", id, "--client-secret-file", secret, "--out", out, "--timeout", "50ms"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("read-only run: %v", err)
	}
	u := firstURL(se.String())
	if !strings.Contains(u, "calendar.readonly") || strings.Contains(u, "calendar.events") || !strings.Contains(u, "code_challenge_method=S256") {
		t.Errorf("read-only auth URL: %s", u)
	}
	if err := run("--client-id-file", id, "--client-secret-file", secret, "--out", out, "--timeout", "50ms", "--write"); err == nil {
		t.Error("write run did not time out")
	}
	if u := firstURL(se.String()); !strings.Contains(u, "calendar.readonly") || !strings.Contains(u, "calendar.events.owned") {
		t.Errorf("write auth URL: %s", u)
	}
	if strings.Contains(se.String(), csecret) || strings.Contains(so.String(), csecret) {
		t.Error("the client secret was printed")
	}
}

func firstURL(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "https://") {
			u, _ := url.QueryUnescape(ln)
			return u
		}
	}
	return ""
}
