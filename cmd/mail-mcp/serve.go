package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requireLoopback refuses any listen address that is not loopback. The MCP
// listener checks no token: the resource-proxy sidecars in the same pod do,
// and reach this process on 127.0.0.1. An empty host (":8081"), 0.0.0.0, a
// pod IP or a name other than localhost would publish the mailboxes to
// whatever can route to the pod, so they fail at start rather than serve.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--addr %q is not a loopback address: this server checks no token, so it listens on 127.0.0.1 behind the sluis resource-proxy sidecar; pass --allow-non-loopback (ALLOW_NON_LOOPBACK=true) only when another component authenticates every request and is the only way in", addr)
}

// newMCPMux serves the two tool sets on two paths of one listener:
//
//	/mcp        read   -> the proxy of the read resource
//	/mcp-admin  admin  -> the proxy of the admin resource
//
// The split holds because each sidecar forwards only its own resource's
// prefix to exactly one of these paths (/mail to /mcp, /mail-admin to
// /mcp-admin) after checking the token's audience for that resource alone. A
// token minted for the read resource therefore never reaches /mcp-admin.
// Nothing here looks at a token, a header or a path outside these two
// prefixes.
func newMCPMux(log *slog.Logger, read, admin *mcp.Server) *http.ServeMux {
	mux := http.NewServeMux()
	for _, ep := range []struct {
		path string
		s    *mcp.Server
	}{
		{"/mcp", read},
		{"/mcp-admin", admin},
	} {
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ep.s }, &mcp.StreamableHTTPOptions{Logger: log})
		mux.Handle(ep.path, h)
		mux.Handle(ep.path+"/", h)
	}
	return mux
}

// newOpsMux is the ops listener's handler: liveness, and nothing of MCP. It
// touches no mailbox: a probe that fails when Gmail blips takes the pod out
// of service for something a restart cannot fix. It is independent of
// leadership (a new pod is Ready while the old one still leads).
func newOpsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
