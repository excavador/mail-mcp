package pdftext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// TestMain lets the test binary play the limit-and-exec child: the runner
// re-execs "Self", which here is this binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ChildMode {
		ExitChild(os.Args[2:])
		return
	}
	os.Exit(m.Run())
}

type fixture struct {
	root, script string
	r            *Runner
}

// newFixture makes a store root and a fake pdftotext with the given shell
// body. No real poppler is involved.
func newFixture(t *testing.T, body string, mod func(*Config)) *fixture {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "pdftotext")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Root: filepath.Join(dir, "store"), Program: script, Self: self}
	if mod != nil {
		mod(&cfg)
	}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{root: cfg.Root, script: script, r: r}
}

// put stores content as a blob under root/pdf and returns its hash.
func (f *fixture) put(t *testing.T, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	h := hex.EncodeToString(sum[:])
	p := filepath.Join(f.root, "pdf", h[:2], h)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

var pdfBytes = []byte("%PDF-1.7\nnot really a pdf, the extractor is fake\n")

func TestOKAndPageCount(t *testing.T) {
	f := newFixture(t, `printf 'invoice 42\n\fsecond page\n\f'`, nil)
	got := f.r.Extract(context.Background(), f.put(t, pdfBytes))
	if got.Status != StatusOK || !strings.Contains(got.Text, "invoice 42") || strings.Contains(got.Text, "\f") {
		t.Fatalf("got %+v", got)
	}
	if got.Truncated || got.PagesCapped {
		t.Fatalf("flags: %+v", got)
	}
}

func TestPagesCapped(t *testing.T) {
	f := newFixture(t, `i=0; while [ $i -lt 30 ]; do printf 'p\n\f'; i=$((i+1)); done`, nil)
	got := f.r.Extract(context.Background(), f.put(t, pdfBytes))
	if got.Status != StatusOK || !got.PagesCapped {
		t.Fatalf("got %+v", got)
	}
}

func TestWallClockTimeoutKillsGroup(t *testing.T) {
	// The script execs sleep (no fork: NPROC is 1). A unique argument lets
	// the test find the process afterwards.
	f := newFixture(t, `exec sleep 600.7351`, func(c *Config) { c.WallTime = 500 * time.Millisecond })
	h := f.put(t, pdfBytes)
	start := time.Now()
	got := f.r.Extract(context.Background(), h)
	if got.Status != StatusTimeout {
		t.Fatalf("got %+v", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	for i := 0; i < 40 && procWithArg("600.7351"); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if procWithArg("600.7351") {
		t.Fatal("the killed job's process is still alive")
	}
}

// procWithArg reports whether a live (non-zombie) "sleep <arg>" process exists.
func procWithArg(arg string) bool {
	es, _ := os.ReadDir("/proc")
	for _, e := range es {
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !strings.HasSuffix(string(b), "sleep\x00"+arg+"\x00") {
			continue
		}
		st, _ := os.ReadFile("/proc/" + e.Name() + "/stat")
		if strings.Contains(string(st), ") Z ") {
			continue
		}
		return true
	}
	return false
}

func TestOutputCapTruncatesAtRuneBoundary(t *testing.T) {
	// Endless 3-byte runes; a cap of 1000 bytes lands inside a rune.
	f := newFixture(t, `exec yes 'ééé€'`, func(c *Config) { c.MaxOut = 1000 })
	got := f.r.Extract(context.Background(), f.put(t, pdfBytes))
	if got.Status != StatusOK || !got.Truncated {
		t.Fatalf("got status=%s truncated=%v", got.Status, got.Truncated)
	}
	if len(got.Text) > 1000 || !utf8.ValidString(got.Text) || len(got.Text) < 990 {
		t.Fatalf("len=%d valid=%v", len(got.Text), utf8.ValidString(got.Text))
	}
}

func TestNonZeroExitIsFailed(t *testing.T) {
	f := newFixture(t, `echo partial; exit 3`, nil)
	if got := f.r.Extract(context.Background(), f.put(t, pdfBytes)); got.Status != StatusFailed || got.Text != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestMissingProgramIsUnavailableNotFailed(t *testing.T) {
	f := newFixture(t, `true`, func(c *Config) { c.Program = "/nonexistent/pdftotext" })
	if got := f.r.Extract(context.Background(), f.put(t, pdfBytes)); got.Status != StatusUnavailable {
		t.Fatalf("got %+v", got)
	}
}

func TestKillNotCausedByTheHelperIsUnavailable(t *testing.T) {
	f := newFixture(t, `kill -9 $$`, nil)
	if got := f.r.Extract(context.Background(), f.put(t, pdfBytes)); got.Status != StatusUnavailable {
		t.Fatalf("got %+v", got)
	}
}

func TestKillIsBlamedOnTheFileWhenTheSelfTestPasses(t *testing.T) {
	// pdftotext "gets OOM-killed" on the file but handles the self-test
	// document: a healthy helper, so the file caused it.
	f := newFixture(t, `case "$*" in *selftest*) echo `+selfTestMarker+` ;; *) kill -9 $$ ;; esac`, func(c *Config) { c.ScratchDir = t.TempDir() })
	if got := f.r.Extract(context.Background(), f.put(t, pdfBytes)); got.Status != StatusFailed {
		t.Fatalf("got %+v, want failed", got)
	}
	if f.r.isBroken() {
		t.Fatal("the helper was marked broken by one bad file")
	}
}

func TestKillIsBlamedOnTheHelperWhenTheSelfTestFails(t *testing.T) {
	f := newFixture(t, `kill -9 $$`, func(c *Config) { c.ScratchDir = t.TempDir() })
	if got := f.r.Extract(context.Background(), f.put(t, pdfBytes)); got.Status != StatusUnavailable {
		t.Fatalf("got %+v, want unavailable", got)
	}
}

func TestSelfTestGatesEveryRequest(t *testing.T) {
	// A fake that prints the marker passes; one that fails does not, and then
	// every request, valid file or not, is answered unavailable.
	good := newFixture(t, `echo `+selfTestMarker, func(c *Config) { c.ScratchDir = t.TempDir() })
	if !good.r.SelfTest(context.Background()) {
		t.Fatal("self-test should pass")
	}
	if got := good.r.Extract(context.Background(), good.put(t, pdfBytes)); got.Status != StatusOK {
		t.Fatalf("after a passing self-test: %+v", got)
	}
	bad := newFixture(t, `echo something else`, func(c *Config) { c.ScratchDir = t.TempDir() })
	if bad.r.SelfTest(context.Background()) {
		t.Fatal("self-test should fail")
	}
	if got := bad.r.Extract(context.Background(), bad.put(t, pdfBytes)); got.Status != StatusUnavailable {
		t.Fatalf("after a failed self-test: %+v", got)
	}
	// The embedded document itself is a PDF the helper accepts.
	if b := selfTestPDF(); !strings.HasPrefix(string(b), "%PDF-") || !strings.Contains(string(b), selfTestMarker) {
		t.Fatal("bad self-test document")
	}
}

func TestCPULimitIsTimeout(t *testing.T) {
	f := newFixture(t, `exec sh -c "while :; do :; done"`, func(c *Config) { c.CPUSec = 1; c.WallTime = 15 * time.Second })
	got := f.r.Extract(context.Background(), f.put(t, pdfBytes))
	if got.Status != StatusTimeout {
		t.Fatalf("got %+v", got)
	}
}

func TestRlimitsAreInForce(t *testing.T) {
	f := newFixture(t, `echo nofile=; ulimit -n; echo fsize=; ulimit -f; echo core=; ulimit -c; echo cpu=; ulimit -t`, nil)
	got := f.r.Extract(context.Background(), f.put(t, pdfBytes))
	if got.Status != StatusOK {
		t.Fatalf("got %+v", got)
	}
	for _, want := range []string{"nofile=\n64", "fsize=\n0", "core=\n0", "cpu=\n15"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("%q missing in %q", want, got.Text)
		}
	}
}

func TestForkIsRefused(t *testing.T) {
	// NPROC=1: a program that tries to start a child cannot (dash exits when
	// fork fails, so the job fails and never reaches the echo).
	f := newFixture(t, `/bin/true; echo forked`, nil)
	got := f.r.Extract(context.Background(), f.put(t, pdfBytes))
	if got.Status == StatusOK || strings.Contains(got.Text, "forked") {
		t.Fatalf("fork succeeded: %+v", got)
	}
}

func TestInputGates(t *testing.T) {
	f := newFixture(t, `echo should not run`, func(c *Config) { c.MaxIn = 100 })
	if got := f.r.Extract(context.Background(), f.put(t, append(pdfBytes, make([]byte, 200)...))); got.Status != StatusTooLarge {
		t.Fatalf("too large: %+v", got)
	}
	if got := f.r.Extract(context.Background(), f.put(t, []byte("GIF89a not a pdf"))); got.Status != StatusNotPDF {
		t.Fatalf("not pdf: %+v", got)
	}
	if got := f.r.Extract(context.Background(), f.put(t, []byte("%PD"))); got.Status != StatusNotPDF {
		t.Fatalf("short: %+v", got)
	}
}

func TestHashValidationAndPathConfinement(t *testing.T) {
	f := newFixture(t, `echo SECRET`, nil)
	secret := filepath.Join(filepath.Dir(f.root), "secret.pdf")
	if err := os.WriteFile(secret, pdfBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	valid := f.put(t, pdfBytes)
	for _, bad := range []string{
		"", "../secret.pdf", "../../etc/passwd", "/etc/passwd", valid + "/../..", strings.ToUpper(valid),
		"pdf/" + valid[:2] + "/" + valid, valid[:63], valid + "0", strings.Repeat("g", 64), valid[:2] + "/" + valid[3:], valid + "\x00",
		"..%2f..%2fetc%2fpasswd",
	} {
		if p, ok := f.r.blobPath(bad); ok {
			t.Errorf("%q accepted as %s", bad, p)
		}
		if got := f.r.Extract(context.Background(), bad); got.Status != StatusFailed || got.Text != "" {
			t.Errorf("%q: %+v", bad, got)
		}
	}
	p, ok := f.r.blobPath(valid)
	if !ok || !strings.HasPrefix(p, f.root+string(filepath.Separator)) {
		t.Fatalf("valid hash resolved to %q", p)
	}
	// A symlink in the store is not followed out of it.
	link := strings.Repeat("a", 64)
	lp := filepath.Join(f.root, "pdf", "aa", link)
	_ = os.MkdirAll(filepath.Dir(lp), 0o755)
	if err := os.Symlink(secret, lp); err != nil {
		t.Fatal(err)
	}
	if got := f.r.Extract(context.Background(), link); got.Status != StatusFailed {
		t.Fatalf("symlink followed: %+v", got)
	}
	if got := f.r.Extract(context.Background(), strings.Repeat("b", 64)); got.Status != StatusFailed {
		t.Fatalf("missing blob: %+v", got)
	}
}

func TestAtMostTwoJobsAtOnce(t *testing.T) {
	cnt := filepath.Join(t.TempDir(), "run")
	_ = os.MkdirAll(cnt, 0o755)
	// Each job drops a file, holds for a moment, and removes it; the test
	// samples how many exist.
	f := newFixture(t, `f=`+cnt+`/$$; : > $f; read -t 1 x < /dev/zero 2>/dev/null; exec sleep 0.4`, nil)
	h := f.put(t, pdfBytes)
	var max atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			es, _ := os.ReadDir(cnt)
			alive := int32(0)
			for _, e := range es {
				if _, err := os.Stat("/proc/" + e.Name()); err == nil {
					if st, _ := os.ReadFile("/proc/" + e.Name() + "/stat"); !strings.Contains(string(st), ") Z ") {
						alive++
					}
				}
			}
			if alive > max.Load() {
				max.Store(alive)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	done := make(chan struct{}, 6)
	for i := 0; i < 6; i++ {
		go func() { f.r.Extract(context.Background(), h); done <- struct{}{} }()
	}
	for i := 0; i < 6; i++ {
		<-done
	}
	close(stop)
	if m := max.Load(); m > MaxConcurrent || m == 0 {
		t.Fatalf("saw %d concurrent jobs, want 1..%d", m, MaxConcurrent)
	}
}

func TestServeOverSocket(t *testing.T) {
	f := newFixture(t, `echo hello pdf`, nil)
	h := f.put(t, pdfBytes)
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = Serve(ln, f.r, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	defer ln.Close()
	call := func(line string) Response {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write([]byte(line + "\n"))
		var r Response
		if err := json.NewDecoder(c).Decode(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := call(`{"hash":"` + h + `"}`); r.Status != StatusOK || !strings.Contains(r.Text, "hello pdf") {
		t.Fatalf("%+v", r)
	}
	if r := call(`{"hash":"../../etc/passwd"}`); r.Status != StatusFailed {
		t.Fatalf("%+v", r)
	}
	if r := call(`not json`); r.Status != StatusFailed {
		t.Fatalf("%+v", r)
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
}
