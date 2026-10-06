package pdfclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/cache"
)

// fakeSidecar answers each connection with handler's response; a nil
// response closes the connection without one.
func fakeSidecar(t *testing.T, handler func(hash string) *response) (sock string, conns *atomic.Int32) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				var req request
				_ = json.Unmarshal([]byte(line), &req)
				if r := handler(req.Hash); r != nil {
					_ = json.NewEncoder(c).Encode(r)
				}
			}()
		}
	}()
	return sock, conns
}

const h1 = "0000000000000000000000000000000000000000000000000000000000000001"

func TestExtractOK(t *testing.T) {
	sock, _ := fakeSidecar(t, func(h string) *response {
		return &response{Status: "ok", Text: "hello " + h[63:], Truncated: true, PagesCapped: true}
	})
	r, err := New(sock).Extract(context.Background(), h1)
	if err != nil || r.Status != "ok" || r.Text != "hello 1" || !r.Truncated || !r.PagesCapped {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBreakerOpensAfterFiveFailuresAndRecoversAfterCoolDown(t *testing.T) {
	var healthy atomic.Bool
	sock, conns := fakeSidecar(t, func(string) *response {
		if healthy.Load() {
			return &response{Status: "ok", Text: "x"}
		}
		return nil // a dead peer: EOF instead of an answer
	})
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	c := New(sock)
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	for i := 0; i < 5; i++ {
		if _, err := c.Extract(context.Background(), h1); !errors.Is(err, ErrCall) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if !c.Open() {
		t.Fatal("breaker should be open after 5 consecutive failures")
	}
	dialed := conns.Load()
	for i := 0; i < 10; i++ {
		if _, err := c.Extract(context.Background(), h1); !errors.Is(err, cache.ErrPDFUnavailable) {
			t.Fatalf("open breaker returned %v", err)
		}
	}
	if conns.Load() != dialed {
		t.Fatal("an open breaker still dialed the sidecar")
	}
	// Still open just before the cool-down ends.
	advance(coolDown - time.Second)
	if _, err := c.Extract(context.Background(), h1); !errors.Is(err, cache.ErrPDFUnavailable) {
		t.Fatalf("before cool-down: %v", err)
	}
	// After it: one probe goes out; it fails, so the breaker re-opens at once.
	advance(2 * time.Second)
	if _, err := c.Extract(context.Background(), h1); !errors.Is(err, ErrCall) {
		t.Fatalf("probe: %v", err)
	}
	if _, err := c.Extract(context.Background(), h1); !errors.Is(err, cache.ErrPDFUnavailable) {
		t.Fatalf("after a failed probe: %v", err)
	}
	// The next probe succeeds and closes it.
	healthy.Store(true)
	advance(coolDown + time.Second)
	if r, err := c.Extract(context.Background(), h1); err != nil || r.Status != "ok" {
		t.Fatalf("recovery probe: %+v %v", r, err)
	}
	if c.Open() {
		t.Fatal("breaker should have closed")
	}
}

func TestSuccessResetsTheFailureCount(t *testing.T) {
	var n atomic.Int32
	sock, _ := fakeSidecar(t, func(string) *response {
		if n.Add(1)%5 == 0 {
			return &response{Status: "ok"}
		}
		return nil
	})
	c := New(sock)
	for i := 0; i < 40; i++ {
		_, _ = c.Extract(context.Background(), h1)
	}
	if c.Open() {
		t.Fatal("four failures between successes must never open the breaker")
	}
}

func TestStatusesAndBreaker(t *testing.T) {
	for _, tc := range []struct {
		status string
		trips  bool
	}{{"ok", false}, {"too_large", false}, {"not_pdf", false}, {"failed", true}, {"timeout", true}} {
		sock, _ := fakeSidecar(t, func(string) *response { return &response{Status: tc.status} })
		c := New(sock)
		for i := 0; i < 5; i++ {
			r, err := c.Extract(context.Background(), h1)
			if err != nil || r.Status != tc.status {
				t.Fatalf("%s: %+v %v", tc.status, r, err)
			}
		}
		if c.Open() != tc.trips {
			t.Errorf("%s: open=%v want %v", tc.status, c.Open(), tc.trips)
		}
	}
}

func TestUnknownStatusIsAnErrorWithoutEcho(t *testing.T) {
	sock, _ := fakeSidecar(t, func(string) *response { return &response{Status: "SECRET-" + "pwned"} })
	_, err := New(sock).Extract(context.Background(), h1)
	if !errors.Is(err, ErrCall) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}

func TestNoSocketAndDeadlineAreFixedErrors(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "absent.sock"))
	_, err := c.Extract(context.Background(), h1)
	if !errors.Is(err, ErrCall) || strings.Contains(err.Error(), "absent") {
		t.Fatalf("dial: %v", err)
	}
	sock, _ := fakeSidecar(t, func(string) *response { time.Sleep(2 * time.Second); return &response{Status: "ok"} })
	c = New(sock)
	c.deadline = 100 * time.Millisecond
	start := time.Now()
	if _, err := c.Extract(context.Background(), h1); !errors.Is(err, ErrCall) || time.Since(start) > time.Second {
		t.Fatalf("deadline: %v after %v", err, time.Since(start))
	}
}

func TestPoolOfTwo(t *testing.T) {
	var cur, max atomic.Int32
	sock, _ := fakeSidecar(t, func(string) *response {
		n := cur.Add(1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		cur.Add(-1)
		return &response{Status: "ok"}
	})
	c := New(sock)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Extract(context.Background(), h1) }()
	}
	wg.Wait()
	if m := max.Load(); m != workers {
		t.Fatalf("max concurrent calls = %d, want %d", m, workers)
	}
}
