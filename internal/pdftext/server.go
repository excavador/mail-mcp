package pdftext

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// Listen binds the unix socket at path, replacing a stale socket file, with
// mode 0600 (the client runs as the same uid in the same pod).
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve answers requests on ln until it is closed: one JSON request line and
// one JSON response per connection.
func Serve(ln net.Listener, r *Runner, log *slog.Logger) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(conn, r, log)
		}()
	}
}

func handle(conn net.Conn, r *Runner, log *slog.Logger) {
	defer conn.Close()
	defer func() {
		if p := recover(); p != nil {
			log.Error("pdftext handler panicked")
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxRequestLine), maxRequestLine).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req Request
	resp := Response{Status: StatusFailed}
	if json.Unmarshal(line, &req) == nil {
		// If the client goes away, stop waiting for a slot, and kill the job.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go watchClose(conn, cancel)
		_ = conn.SetReadDeadline(time.Time{})
		start := time.Now()
		resp = r.Extract(ctx, req.Hash)
		// The hash is validated before it is logged; a request that fails
		// validation is logged without it.
		if hashRE.MatchString(req.Hash) {
			log.Info("pdftext job", "hash", req.Hash, "status", resp.Status, "bytes", len(resp.Text), "truncated", resp.Truncated, "took", time.Since(start).Round(time.Millisecond).String())
		} else {
			log.Info("pdftext job refused", "status", resp.Status)
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(conn).Encode(resp)
}

// watchClose cancels when the peer closes its end (the client sends one line
// and then only waits, so any read result means it is gone).
func watchClose(conn net.Conn, cancel context.CancelFunc) {
	var b [1]byte
	_, _ = conn.Read(b[:])
	cancel()
}
