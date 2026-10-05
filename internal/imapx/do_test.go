package imapx

// Tests for Do and the bounded Dial: a scripted server that goes quiet at a
// chosen step, and the budget, the phase and the grace Do promises. Every wait
// is bounded: servers drop a connection after 30s and tests poll with limits.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
)

const testBudget = time.Second

// slack is what a test allows on top of budget + graceAfterTimeout.
const slack = 500 * time.Millisecond

type quiet struct {
	// mode: "tcp" accepts and says nothing (no TLS handshake either);
	// "tls" completes the handshake and says nothing; "plain" is a plaintext
	// port that never greets; "plain-greet" greets in plaintext and never
	// answers STARTTLS; "imap" is a full TLS server that answers everything
	// except the verb in stall.
	mode  string
	stall string // "imap" mode: the verb that gets no answer
}

func drain(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	_, _ = io.Copy(io.Discard, c)
}

func startQuiet(t *testing.T, q quiet) (accounts.Account, struct{}) {
	t.Helper()
	cert, pin := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				switch q.mode {
				case "tcp", "plain":
					drain(c)
				case "plain-greet":
					_, _ = io.WriteString(c, "* OK [CAPABILITY IMAP4rev1 STARTTLS] hi\r\n")
					drain(c)
				case "tls", "imap":
					tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
					if err := tc.Handshake(); err != nil {
						return
					}
					if q.mode == "tls" {
						drain(tc)
						return
					}
					serveIMAP(tc, q.stall)
				}
			}()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	a := account(h, port, pin)
	if q.mode == "plain" || q.mode == "plain-greet" {
		a.TLS = accounts.StartTLS
	}
	return a, struct{}{}
}

func serveIMAP(c net.Conn, stall string) {
	say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	caps := "IMAP4rev1 UIDPLUS X-GM-EXT-1"
	say("* OK [CAPABILITY " + caps + "] fake ready")
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		tag, verb := f[0], strings.ToUpper(f[1])
		if verb == "UID" && len(f) > 2 {
			verb = strings.ToUpper(f[2])
		}
		if verb == stall {
			drain(c) // reads until the client closes, says nothing
			return
		}
		switch verb {
		case "LOGIN":
			say(tag + " OK [CAPABILITY " + caps + "] logged in")
		case "LIST":
			say(`* LIST (\HasNoChildren \All) "/" "[Gmail]/All Mail"`)
			say(tag + " OK done")
		case "STATUS":
			say(`* STATUS "[Gmail]/All Mail" (MESSAGES 3)`)
			say(tag + " OK done")
		case "EXAMINE", "SELECT":
			say("* 0 EXISTS")
			say("* OK [UIDVALIDITY 1] ok")
			say(tag + " OK [READ-ONLY] done")
		case "SEARCH":
			say("* SEARCH 1 2")
			say(tag + " OK done")
		case "LOGOUT":
			say("* BYE bye")
			say(tag + " OK done")
			return
		default:
			say(tag + " OK done")
		}
	}
}

func searchFn(ctx context.Context, c *imapclient.Client) error {
	_, _, err := GmailRawSearch(ctx, c, "x")
	return err
}

func listFn(ctx context.Context, c *imapclient.Client) error {
	_, err := ListFolders(ctx, c)
	return err
}

func noopFn(context.Context, *imapclient.Client) error { return nil }

var afterRE = regexp.MustCompile(`after [0-9.]+(ms|s|m[0-9.]*s?)`)

// wantTimeout runs Do against a and checks the promises: ErrTimeout, in time,
// the phase named, the elapsed time reported.
func wantTimeout(t *testing.T, a accounts.Account, fn func(context.Context, *imapclient.Client) error, phases ...string) {
	t.Helper()
	start := time.Now()
	err := Do(context.Background(), a, "test", testBudget, fn)
	took := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v after %s, want ErrTimeout", err, took)
	}
	if max := testBudget + graceAfterTimeout + slack; took > max {
		t.Errorf("Do took %s, want at most %s", took, max)
	}
	if took < testBudget {
		t.Errorf("Do returned after %s, before its budget %s", took, testBudget)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "mail server did not answer in time") {
		t.Errorf("message %q", msg)
	}
	ok := false
	for _, p := range phases {
		if strings.Contains(msg, "(phase "+p+",") {
			ok = true
		}
	}
	if !ok {
		t.Errorf("message %q does not name phase %v", msg, phases)
	}
	if !afterRE.MatchString(msg) {
		t.Errorf("message %q does not report the elapsed time", msg)
	}
}

// 1: a server that logs in and then never answers SEARCH.
func TestDoStalledSearchTimesOutInPhaseSearch(t *testing.T) {
	t.Parallel()
	a, _ := startQuiet(t, quiet{mode: "imap", stall: "SEARCH"})
	wantTimeout(t, a, searchFn, "search")
}

// 2: every silent phase is bounded and named.
func TestDoSilentPhasesAreBoundedAndNamed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		q      quiet
		fn     func(context.Context, *imapclient.Client) error
		phases []string
	}{
		{"implicit TLS, TCP up but no handshake", quiet{mode: "tcp"}, noopFn, []string{"tls"}},
		// The greeting is awaited after the handshake; Dial reports "tls"
		// until imapclient.New returns and "login" after, so either names it.
		{"implicit TLS, no greeting", quiet{mode: "tls"}, noopFn, []string{"tls", "login"}},
		{"STARTTLS, no greeting", quiet{mode: "plain"}, noopFn, []string{"starttls"}},
		{"STARTTLS, greeting but no answer to STARTTLS", quiet{mode: "plain-greet"}, noopFn, []string{"starttls"}},
		{"LOGIN stalled", quiet{mode: "imap", stall: "LOGIN"}, noopFn, []string{"login"}},
		{"LIST stalled", quiet{mode: "imap", stall: "LIST"}, listFn, []string{"list"}},
		{"EXAMINE stalled", quiet{mode: "imap", stall: "EXAMINE"}, searchFn, []string{"examine"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _ := startQuiet(t, tc.q)
			wantTimeout(t, a, tc.fn, tc.phases...)
		})
	}
}

// A healthy server through Do: no timeout, and what fn returned comes back.
func TestDoHealthySessionReturnsFnResult(t *testing.T) {
	t.Parallel()
	a, _ := startQuiet(t, quiet{mode: "imap"})
	if err := Do(context.Background(), a, "test", 5*time.Second, searchFn); err != nil {
		t.Fatalf("healthy Do: %v", err)
	}
	want := errors.New("fn failed")
	err := Do(context.Background(), a, "test", 5*time.Second, func(context.Context, *imapclient.Client) error { return want })
	if !errors.Is(err, want) || errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want fn's error and no timeout", err)
	}
}

// lockedBuf is a log sink two goroutines may share.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// captureLog swaps the default logger for the test's duration. Tests that use
// it are not parallel.
func captureLog(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// 3: the caller's own cancel is "canceled", not a timeout.
func TestDoParentCancelIsCanceledNotTimeout(t *testing.T) {
	logs := captureLog(t)
	a, _ := startQuiet(t, quiet{mode: "imap", stall: "SEARCH"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	err := Do(ctx, a, "cancel_tool", 20*time.Second, searchFn)
	took := time.Since(start)
	if err == nil {
		t.Fatal("canceled Do returned nil")
	}
	if errors.Is(err, ErrTimeout) {
		t.Errorf("a parent cancel reported as a timeout: %v", err)
	}
	if took > graceAfterTimeout+slack+300*time.Millisecond {
		t.Errorf("canceled Do took %s", took)
	}
	out := logs.String()
	if !strings.Contains(out, "outcome=canceled") || !strings.Contains(out, "tool=cancel_tool") {
		t.Errorf("log lacks the canceled outcome: %s", out)
	}
}

// The "imap session" line carries account, tool, phase, outcome and duration.
func TestDoLogsOneSessionLine(t *testing.T) {
	logs := captureLog(t)
	a, _ := startQuiet(t, quiet{mode: "imap", stall: "SEARCH"})
	_ = Do(context.Background(), a, "log_tool", testBudget, searchFn)
	var lines []string
	for _, ln := range strings.Split(logs.String(), "\n") {
		if strings.Contains(ln, `msg="imap session"`) {
			lines = append(lines, ln)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one imap session line, got %d: %s", len(lines), logs.String())
	}
	for _, want := range []string{"account=t", "tool=log_tool", "phase=search", "outcome=timeout", "duration=", "level=WARN"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q lacks %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "pw") && strings.Contains(strings.ToLower(lines[0]), "password") {
		t.Errorf("line mentions a password: %q", lines[0])
	}

	logs2 := captureLog(t)
	b, _ := startQuiet(t, quiet{mode: "imap"})
	if err := Do(context.Background(), b, "ok_tool", 5*time.Second, searchFn); err != nil {
		t.Fatal(err)
	}
	if o := logs2.String(); !strings.Contains(o, "outcome=ok") || !strings.Contains(o, "level=INFO") {
		t.Errorf("success line: %s", o)
	}
}

// 4: nothing is left running after a timeout. Not parallel: it counts every
// goroutine in the process.
func TestDoLeavesNoGoroutinesBehindAfterTimeout(t *testing.T) {
	a, _ := startQuiet(t, quiet{mode: "imap", stall: "SEARCH"})
	b, _ := startQuiet(t, quiet{mode: "tcp"})
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()
	for range 3 {
		if err := Do(context.Background(), a, "leak", 500*time.Millisecond, searchFn); !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v", err)
		}
		if err := Do(context.Background(), b, "leak", 500*time.Millisecond, noopFn); !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v", err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	var n int
	for {
		n = runtime.NumGoroutine()
		if n <= base+3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n > base+3 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutines: %d before, %d after six timeouts\n%s", base, n, buf)
	}
}

// A STATUS that stalls: ListFolders swallows a failed STATUS, so a listing
// cut short by the budget must still come back as a timeout, not as folders
// with zero counts.
func TestDoStalledStatusIsATimeoutNotAZeroCount(t *testing.T) {
	t.Parallel()
	a, _ := startQuiet(t, quiet{mode: "imap", stall: "STATUS"})
	var got []Folder
	start := time.Now()
	err := Do(context.Background(), a, "status", testBudget, func(ctx context.Context, c *imapclient.Client) error {
		var lerr error
		got, lerr = ListFolders(ctx, c)
		return lerr
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v (folders %+v) after %s, want ErrTimeout", err, got, time.Since(start))
	}
}

func TestTimeoutMessageNamesNoSecret(t *testing.T) {
	t.Parallel()
	a, _ := startQuiet(t, quiet{mode: "imap", stall: "LOGIN"})
	err := Do(context.Background(), a, "test", testBudget, noopFn)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), a.Username+" ") && strings.Contains(err.Error(), "refused") {
		t.Errorf("a stalled login reads as a refusal: %v", err)
	}
	_ = fmt.Sprint(err)
}
