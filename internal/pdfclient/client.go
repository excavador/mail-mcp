// Package pdfclient talks to the pdftext sidecar over its unix socket. It is
// the only thing mail-mcp has to do with PDFs: it sends a digest and receives
// bounded text. It holds a worker pool of two, a per-call deadline and a
// circuit breaker, so a sick or absent sidecar costs the job a pause and
// nothing else.
package pdfclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/excavador/mail-mcp/internal/cache"
)

// Fixed error texts: nothing from the peer or the input is echoed.
var (
	ErrCall = errors.New("pdf extractor call failed")
	// ErrOpen wraps cache.ErrPDFUnavailable: the breaker is open.
	ErrOpen = cache.ErrPDFUnavailable
)

const (
	workers      = 2
	callDeadline = 25 * time.Second
	maxFailures  = 5
	coolDown     = time.Minute
	// maxResponse bounds what is read from the socket: 1 MiB of text can
	// grow several-fold once JSON-escaped.
	maxResponse = 8 << 20
)

// Client is a cache.PDFExtractor backed by the sidecar.
type Client struct {
	socket   string
	sem      chan struct{}
	deadline time.Duration
	cool     time.Duration
	maxFail  int
	now      func() time.Time

	mu       sync.Mutex
	failures int
	openedAt time.Time
	open     bool
	probing  bool
}

// New returns a Client for the sidecar's socket.
func New(socket string) *Client {
	return &Client{socket: socket, sem: make(chan struct{}, workers), deadline: callDeadline, cool: coolDown, maxFail: maxFailures, now: time.Now}
}

type request struct {
	Hash string `json:"hash"`
}

type response struct {
	Status      string `json:"status"`
	Text        string `json:"text"`
	Truncated   bool   `json:"truncated"`
	PagesCapped bool   `json:"pages_capped"`
}

// allow reports whether a call may go out: always while the breaker is
// closed; once open, only after the cool-down and then one probe at a time.
func (c *Client) allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		return true
	}
	if c.probing || c.now().Sub(c.openedAt) < c.cool {
		return false
	}
	c.probing = true
	return true
}

// record notes the outcome of a call. A failure is a transport error or an
// answer the client cannot read; any valid status, "failed" and "timeout"
// included (those describe one file), proves the sidecar is alive.
func (c *Client) record(fail bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probing = false
	if !fail {
		c.failures, c.open = 0, false
		return
	}
	c.failures++
	if c.open || c.failures >= c.maxFail {
		c.open, c.openedAt = true, c.now()
	}
}

// Open reports whether the breaker is open (for status and tests).
func (c *Client) Open() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open
}

// Extract asks the sidecar for the text of the file with the given SHA-256.
func (c *Client) Extract(ctx context.Context, sha string) (cache.PDFResult, error) {
	if !c.allow() {
		return cache.PDFResult{}, ErrOpen
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		c.mu.Lock() // not the sidecar's fault: release a probe slot, count nothing
		c.probing = false
		c.mu.Unlock()
		return cache.PDFResult{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	resp, err := c.call(ctx, sha)
	if err != nil {
		c.record(true)
		return cache.PDFResult{}, ErrCall
	}
	switch resp.Status {
	case "ok", "too_large", "not_pdf", "timeout", "failed":
		// Every status is the sidecar answering. timeout and failed are about
		// the file, not the sidecar's health.
		c.record(false)
	default:
		c.record(true)
		return cache.PDFResult{}, ErrCall
	}
	return cache.PDFResult{Status: resp.Status, Text: resp.Text, Truncated: resp.Truncated, PagesCapped: resp.PagesCapped}, nil
}

func (c *Client) call(ctx context.Context, sha string) (response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return response{}, err
	}
	defer conn.Close()
	// The deadline bounds every read and write; closing on ctx covers a
	// cancelled caller.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	b, _ := json.Marshal(request{Hash: sha})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return response{}, err
	}
	var r response
	if err := json.NewDecoder(io.LimitReader(conn, maxResponse)).Decode(&r); err != nil {
		return response{}, err
	}
	return r, nil
}
