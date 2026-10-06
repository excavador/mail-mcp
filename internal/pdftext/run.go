package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Config configures a Runner.
type Config struct {
	// Root is the read-only store root. A file is Root/pdf/<hh>/<hash>.
	Root string
	// Program is the pdftotext binary (default "pdftotext").
	Program string
	// Self is the helper binary to re-exec for the limit stage (default
	// os.Executable()). Tests point it at the test binary.
	Self string
	// SelfArgs precedes ChildMode in the re-exec argv (tests only).
	SelfArgs []string
	// Limits; zero means the production default.
	WallTime time.Duration
	CPUSec   uint64
	ASBytes  uint64
	MaxIn    int64
	MaxOut   int
	// ScratchDir is a writable directory for the start-up self-test file.
	ScratchDir string
}

// Runner extracts text from blobs.
type Runner struct {
	c   Config
	sem chan struct{}

	mu       sync.Mutex
	broken   bool      // the last self-test failed
	testedAt time.Time // when it ran
}

// NewRunner fills defaults and returns a Runner that serves at most
// MaxConcurrent jobs at once.
func NewRunner(c Config) (*Runner, error) {
	if c.Root == "" {
		return nil, errors.New("pdftext: root is empty")
	}
	if c.Program == "" {
		c.Program = "pdftotext"
	}
	if c.Self == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		c.Self = self
	}
	if c.WallTime == 0 {
		c.WallTime = 20 * time.Second
	}
	if c.CPUSec == 0 {
		c.CPUSec = 15
	}
	if c.ASBytes == 0 {
		c.ASBytes = 400 << 20
	}
	if c.MaxIn == 0 {
		c.MaxIn = MaxInputBytes
	}
	if c.MaxOut == 0 {
		c.MaxOut = MaxOutputBytes
	}
	return &Runner{c: c, sem: make(chan struct{}, MaxConcurrent)}, nil
}

// blobPath resolves a digest inside the root. It is the only place a path is
// made, and it is made from 64 lowercase hex characters and constants.
func (r *Runner) blobPath(hash string) (string, bool) {
	if !hashRE.MatchString(hash) {
		return "", false
	}
	return filepath.Join(r.c.Root, "pdf", hash[:2], hash), true
}

// Extract runs one job. It waits for one of the MaxConcurrent slots (or ctx).
func (r *Runner) Extract(ctx context.Context, hash string) Response {
	path, ok := r.blobPath(hash)
	if !ok {
		return Response{Status: StatusFailed}
	}
	if r.isBroken() {
		return Response{Status: StatusUnavailable}
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return Response{Status: StatusTimeout}
	}
	// Lstat: a symlink in the store is not followed. Stat before spawning:
	// an oversized input never costs a process.
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return Response{Status: StatusFailed}
	}
	if fi.Size() > r.c.MaxIn {
		return Response{Status: StatusTooLarge}
	}
	f, err := os.Open(path)
	if err != nil {
		return Response{Status: StatusFailed}
	}
	var magic [5]byte
	n, _ := io.ReadFull(f, magic[:])
	_ = f.Close()
	if n < 5 || string(magic[:]) != "%PDF-" {
		return Response{Status: StatusNotPDF}
	}
	return r.run(ctx, path)
}

func (r *Runner) run(ctx context.Context, path string) Response {
	ctx, cancel := context.WithTimeout(ctx, r.c.WallTime)
	defer cancel()

	args := append([]string{}, r.c.SelfArgs...)
	args = append(args, ChildMode, strconv.FormatUint(r.c.CPUSec, 10), strconv.FormatUint(r.c.ASBytes, 10), "--",
		r.c.Program, "-q", "-layout", "-l", strconv.Itoa(MaxPages), "--", path, "-")
	cmd := exec.Command(r.c.Self, args...) // no CommandContext: the group is killed explicitly
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{} // the child sets its own
	cmd.Stdin = nil
	cmd.Stderr = nil
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Response{Status: StatusUnavailable}
	}
	if err := cmd.Start(); err != nil {
		return Response{Status: StatusUnavailable}
	}
	pgid := cmd.Process.Pid
	// kill never signals a reaped child: after Wait the group id could be
	// reused, so reaped is set under the same lock the kill takes.
	var (
		mu     sync.Mutex
		reaped bool
	)
	kill := func() {
		mu.Lock()
		defer mu.Unlock()
		if !reaped {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}

	// The wall-clock limit: kill the whole process group when ctx ends.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			kill()
		case <-done:
		}
	}()

	// Read one byte past the cap: that byte proves there was more.
	buf, rerr := io.ReadAll(io.LimitReader(out, int64(r.c.MaxOut)+1))
	truncated := len(buf) > r.c.MaxOut
	if truncated {
		buf = buf[:r.c.MaxOut]
		kill()
	}
	// Reap. After a kill the pipe closes and Wait returns promptly.
	if truncated {
		_, _ = io.Copy(io.Discard, io.LimitReader(out, 1<<20))
	}
	werr := cmd.Wait()
	mu.Lock()
	reaped = true
	mu.Unlock()
	close(done)

	timedOut := ctx.Err() != nil
	if timedOut && !truncated {
		return Response{Status: StatusTimeout}
	}
	if !truncated && (rerr != nil || werr != nil) {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				switch {
				case ws.Signaled() && ws.Signal() == syscall.SIGXCPU:
					return Response{Status: StatusTimeout}
				case ws.Signaled() && ws.Signal() == syscall.SIGKILL:
					// Not sent by this helper (that case returned above): the
					// kernel's OOM killer, or an operator.
					return Response{Status: StatusUnavailable}
				case ws.Exited() && (ws.ExitStatus() == ExitLimitFailed || ws.ExitStatus() == 127):
					// The limit-and-exec stage could not set limits or exec.
					return Response{Status: StatusUnavailable}
				}
			}
		}
		return Response{Status: StatusFailed}
	}
	text, pages := clean(buf, truncated)
	return Response{Status: StatusOK, Text: text, Truncated: truncated, PagesCapped: pages >= MaxPages}
}

// clean makes the output text: pages counted by form feed and the feed
// removed, invalid UTF-8 dropped, and, when the output was cut, the cut made
// on a rune boundary.
func clean(b []byte, cut bool) (string, int) {
	pages := bytes.Count(b, []byte{'\f'})
	b = bytes.ReplaceAll(b, []byte{'\f'}, []byte{'\n'})
	if cut {
		// Drop a partial trailing rune.
		for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size != 1 {
				break
			}
			b = b[:len(b)-1]
		}
	}
	return strings.ToValidUTF8(string(b), ""), pages
}

// selfTestMarker is the text of the embedded self-test document.
const selfTestMarker = "pdftext-selftest-ok"

// selfTestPDF is a one-page PDF with a valid xref table, built at start-up.
func selfTestPDF() []byte {
	var b strings.Builder
	var offs []int
	obj := func(s string) {
		offs = append(offs, b.Len())
		b.WriteString(strconv.Itoa(len(offs)) + " 0 obj\n" + s + "\nendobj\n")
	}
	b.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 100] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	content := "BT /F1 12 Tf 20 50 Td (" + selfTestMarker + ") Tj ET"
	obj("<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n" + content + "\nendstream")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	x := b.Len()
	b.WriteString("xref\n0 " + strconv.Itoa(len(offs)+1) + "\n0000000000 65535 f \n")
	for _, o := range offs {
		b.WriteString(fmt.Sprintf("%010d 00000 n \n", o))
	}
	b.WriteString("trailer\n<< /Size " + strconv.Itoa(len(offs)+1) + " /Root 1 0 R >>\nstartxref\n" + strconv.Itoa(x) + "\n%%EOF\n")
	return []byte(b.String())
}

// SelfTest runs the extractor, under the real limits, on a tiny embedded PDF.
// It records the result: while the last self-test failed every request is
// answered "unavailable" instead of marking each file failed. It returns the
// result. A failed helper retests itself, at most every 30 s, when a request
// arrives.
func (r *Runner) SelfTest(ctx context.Context) bool {
	ok := r.selfTest(ctx)
	r.mu.Lock()
	r.broken, r.testedAt = !ok, time.Now()
	r.mu.Unlock()
	return ok
}

func (r *Runner) selfTest(ctx context.Context) bool {
	dir := r.c.ScratchDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, ".selftest-*.pdf")
	if err != nil {
		return false
	}
	name := f.Name()
	defer os.Remove(name)
	_, werr := f.Write(selfTestPDF())
	if cerr := f.Close(); werr != nil || cerr != nil {
		return false
	}
	r.sem <- struct{}{}
	defer func() { <-r.sem }()
	resp := r.run(ctx, name)
	return resp.Status == StatusOK && strings.Contains(resp.Text, selfTestMarker)
}

func (r *Runner) isBroken() bool {
	r.mu.Lock()
	broken, at := r.broken, r.testedAt
	r.mu.Unlock()
	if !broken {
		return false
	}
	if time.Since(at) < 30*time.Second {
		return true
	}
	return !r.SelfTest(context.Background())
}
