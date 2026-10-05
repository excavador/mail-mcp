package cache

// delayConn models a high-latency link in the server-to-client direction:
// every Write is queued and delivered no earlier than d later, in order, and
// Write itself never blocks. A server that answers N pipelined commands
// therefore delivers all N answers about d after the first, whereas a client
// that waits for each answer before sending the next pays d every time. This
// file is identical in internal/cache and internal/server except for the
// package clause.

import (
	"net"
	"sync"
	"time"
)

type delayedChunk struct {
	at time.Time
	b  []byte
}

type delayConn struct {
	net.Conn
	d      time.Duration
	mu     sync.Mutex
	q      chan delayedChunk
	closed bool
	done   chan struct{}
}

func newDelayConn(c net.Conn, d time.Duration) *delayConn {
	dc := &delayConn{Conn: c, d: d, q: make(chan delayedChunk, 4096), done: make(chan struct{})}
	go func() {
		defer close(dc.done)
		for ch := range dc.q {
			if w := time.Until(ch.at); w > 0 {
				time.Sleep(w)
			}
			if _, err := dc.Conn.Write(ch.b); err != nil {
				for range dc.q { // drain so Write never blocks
				}
				return
			}
		}
	}()
	return dc
}

func (c *delayConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.q <- delayedChunk{at: time.Now().Add(c.d), b: append([]byte(nil), p...)}
	return len(p), nil
}

// Close delivers what is queued (bounded), then closes the connection.
func (c *delayConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.q)
	}
	c.mu.Unlock()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
	return c.Conn.Close()
}

// maybeDelay wraps c when d > 0.
func maybeDelay(c net.Conn, d time.Duration) net.Conn {
	if d <= 0 {
		return c
	}
	return newDelayConn(c, d)
}
