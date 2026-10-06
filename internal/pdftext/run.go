package pdftext

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Config configures a Runner.
type Config struct {
	// Root is the read-only blob store root. A blob is Root/pdf/<hh>/<hash>.
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
}

// Runner extracts text from blobs.
type Runner struct {
	c   Config
	sem chan struct{}
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
		return Response{Status: StatusFailed}
	}
	if err := cmd.Start(); err != nil {
		return Response{Status: StatusFailed}
	}
	pgid := cmd.Process.Pid
	kill := func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

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
	close(done)
	kill() // any stragglers in the group

	timedOut := ctx.Err() != nil
	if timedOut && !truncated {
		return Response{Status: StatusTimeout}
	}
	if !truncated && (rerr != nil || werr != nil) {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGXCPU {
				return Response{Status: StatusTimeout}
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
