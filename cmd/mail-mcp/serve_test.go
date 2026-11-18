package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/organise"
	"github.com/excavador/mail-mcp/internal/server"
)

func TestRequireLoopback(t *testing.T) {
	ok := []string{"127.0.0.1:8081", "127.0.0.2:8081", "[::1]:8081", "localhost:8081"}
	for _, a := range ok {
		if err := requireLoopback(a); err != nil {
			t.Errorf("requireLoopback(%q) = %v, want nil", a, err)
		}
	}
	// Every way of publishing the listener beyond the pod, and every
	// malformed address, must fail closed.
	refused := []string{
		":8081",            // all interfaces
		"0.0.0.0:8081",     // all interfaces, IPv4
		"[::]:8081",        // all interfaces, IPv6
		"10.1.2.3:8081",    // a pod IP
		"192.168.1.5:80",   // a LAN address
		"mail-mcp:8081",    // a name that is not localhost
		"example.com:80",   // a public name
		"127.0.0.1",        // no port
		"",                 // empty
		"localhost.evil:1", // a lookalike
	}
	for _, a := range refused {
		err := requireLoopback(a)
		if err == nil {
			t.Errorf("requireLoopback(%q) = nil, want an error", a)
			continue
		}
		if a != "" && a != "127.0.0.1" && !strings.Contains(err.Error(), "--allow-non-loopback") {
			t.Errorf("requireLoopback(%q) error does not name the override: %v", a, err)
		}
	}
}

func TestDefaultAddrIsLoopback(t *testing.T) {
	// The default must pass the guard it is subject to.
	if err := requireLoopback("127.0.0.1:8081"); err != nil {
		t.Fatal(err)
	}
}

// startMux serves the real read and admin servers over HTTP on loopback.
func startMux(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	store, err := cache.Open(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hist, err := history.Open(filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hist.Close() })
	org, err := organise.New(store)
	if err != nil {
		t.Fatal(err)
	}
	opts := []server.Option{server.WithHistory(hist), server.WithOrganiser(org)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(newMCPMux(log,
		server.New(nil, store, "test", server.Read, opts...),
		server.New(nil, store, "test", server.Admin, opts...)))
	t.Cleanup(ts.Close)
	return ts
}

func toolNames(t *testing.T, endpoint string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	defer func() { _ = cs.Close() }()
	names := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names[tool.Name] = true
	}
	return names
}

// The read/admin split of D8: the pod's read proxy forwards to /mcp and the
// admin proxy to /mcp-admin, so what each path carries is what a token of
// that resource can reach. /mcp must hold no write tool, whatever options
// the process was started with.
func TestEndpointsCarryTheirOwnToolSets(t *testing.T) {
	ts := startMux(t)
	read := toolNames(t, ts.URL+"/mcp")
	admin := toolNames(t, ts.URL+"/mcp-admin")

	writes := []string{"create_folder", "preview_intent", "apply_intent", "undo", "reapply"}
	for _, w := range writes {
		if read[w] {
			t.Errorf("/mcp (read) carries the write tool %s", w)
		}
		if !admin[w] {
			t.Errorf("/mcp-admin lacks the write tool %s", w)
		}
	}
	for _, r := range []string{"search", "fetch_message", "list_accounts"} {
		if !read[r] || !admin[r] {
			t.Errorf("read tool %s missing (read=%t admin=%t)", r, read[r], admin[r])
		}
	}
}

// A read-resource caller reaches /mcp and below, never /mcp-admin. The
// sidecar hands this listener a literal dot-dot (it re-encodes the path), and
// the mux answers that with a redirect, not a tool set. An encoded dot-dot
// is no climb at all: the mux matches it as a segment under /mcp, which is
// the read server.
func TestReadPathCannotClimbToAdmin(t *testing.T) {
	ts := startMux(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/mcp/../mcp-admin", "/mcp/./../mcp-admin", "/mcp/%2e%2e/mcp-admin"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+p, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue // refused or redirected: nothing served
		}
		// Served: it must be the read server, which has no write tool.
		if names := toolNames(t, ts.URL+p); names["apply_intent"] || names["create_folder"] {
			t.Errorf("%s reached the admin tool set", p)
		}
	}
}

func TestMCPMuxServesNothingElse(t *testing.T) {
	ts := startMux(t)
	for _, p := range []string{"/healthz", "/", "/.well-known/oauth-protected-resource", "/mail", "/mcp-adminx"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (the MCP listener serves only /mcp and /mcp-admin)", p, resp.StatusCode)
		}
	}
}

func TestOpsMuxServesHealthzOnly(t *testing.T) {
	ts := httptest.NewServer(newOpsMux())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
	for _, p := range []string{"/mcp", "/mcp-admin", "/readyz", "/"} {
		resp, err := http.Post(ts.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("ops POST %s = %d, want 404", p, resp.StatusCode)
		}
	}
}
