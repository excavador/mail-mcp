// Command mail-mcp is a Model Context Protocol server for mailboxes reached
// over IMAP: Gmail directly, Proton through Proton Mail Bridge.
//
// It serves two MCP endpoints from one process. sluis grants sessions longer
// than 24 hours only to read-only resources, so reading lives on one resource
// and organising on another:
//
//	/mcp        read tools only            -> the read-only resource (7-day sessions)
//	/mcp-admin  read tools + write tools   -> the write resource (24-hour sessions)
//
// The HTTP transport validates every request itself: a bearer token minted by
// the issuer, checked against its JWKS with the endpoint's own resource URL
// as the required audience (RFC 8707), via truvity/sluis/identity/resource --
// the same as excavador/netbox-mcp and excavador/homebox-mcp.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truvity/sluis/identity/resource"
	"github.com/urfave/cli/v3"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/memlimit"
	"github.com/excavador/mail-mcp/internal/organise"
	"github.com/excavador/mail-mcp/internal/pdfclient"
	"github.com/excavador/mail-mcp/internal/server"
)

// version is overridden at build time.
var version = "dev"

func main() {
	// Everything this process creates (the cache, its SQLite WAL and shm
	// files) holds mail: owner-only, whatever the caller's umask was.
	syscall.Umask(0o077)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := &cli.Command{
		Name:    "mail-mcp",
		Usage:   "MCP server for mailboxes reached over IMAP (Gmail, Proton via Bridge)",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "accounts",
				Usage:    "path to the accounts file (passwords live in files it names, never in it)",
				Sources:  cli.EnvVars("ACCOUNTS_FILE"),
				Required: true,
			},
			&cli.StringFlag{
				Name: "transport",
				// stdio serves the admin tool set: a person running this on
				// their own machine against their own mailboxes needs no
				// split, and there is no issuer to grant sessions anyway.
				Usage:   "stdio or http",
				Value:   "stdio",
				Sources: cli.EnvVars("TRANSPORT"),
			},
			&cli.StringFlag{
				Name: "addr",
				// 0.0.0.0, not 127.0.0.1: in a pod, loopback means nothing
				// can reach it, including the readiness probe.
				Usage:   "listen address for the http transport",
				Value:   "0.0.0.0:8080",
				Sources: cli.EnvVars("ADDR"),
			},
			&cli.StringFlag{
				Name:    "issuer-url",
				Usage:   "issuer that mints tokens for both resources (http transport only)",
				Sources: cli.EnvVars("ISSUER_URL"),
			},
			&cli.StringFlag{
				Name: "read-resource-url",
				// The RFC 8707 resource indicator for the read endpoint: this
				// server's externally reachable URL for /mcp, which is also the
				// audience the issuer mints for it.
				Usage:   "external URL of the read endpoint (http transport only)",
				Sources: cli.EnvVars("READ_RESOURCE_URL"),
			},
			&cli.StringFlag{
				Name:    "admin-resource-url",
				Usage:   "external URL of the admin endpoint (http transport only)",
				Sources: cli.EnvVars("ADMIN_RESOURCE_URL"),
			},
			&cli.StringFlag{
				Name: "scope",
				// sluis rejects an authorize request that carries no scope at
				// all; "openid" is the one it accepts out of the box. Never
				// checked here -- see resource.Config.Scope.
				Usage:   "OAuth scope advertised in the PRM and the 401 challenge (http transport only)",
				Value:   "openid",
				Sources: cli.EnvVars("SCOPE"),
			},
			&cli.StringFlag{
				Name: "cache-dir",
				// Message blobs and the search index. Losing it only costs a
				// re-fetch, so an emptyDir would work; a volume avoids paying
				// that on every restart.
				Usage:   "directory for the immutable message cache",
				Value:   "/var/cache/mail-mcp",
				Sources: cli.EnvVars("CACHE_DIR"),
			},
			&cli.StringFlag{
				Name: "history-dir",
				// What mail-mcp changed, append-only. Unlike the cache it
				// cannot be rebuilt (undo needs it), so it belongs on a volume
				// that is backed up.
				Usage:   "directory for the append-only history of changes (history.jsonl)",
				Value:   "/var/lib/mail-mcp/history",
				Sources: cli.EnvVars("HISTORY_DIR"),
			},
			&cli.IntFlag{
				Name: "max-unelicited-apply",
				// A client that cannot ask the owner itself leaves approval to
				// its own tool prompt, which is easy to wave through; cap what
				// that path may move.
				Usage:   "most messages one apply may change when the client does not support elicitation",
				Value:   server.DefaultMaxUnelicited,
				Sources: cli.EnvVars("MAX_UNELICITED_APPLY"),
			},
			&cli.StringFlag{
				Name: "approval-mode",
				// client: approval is the client's own tool prompt, capped by
				// --max-unelicited-apply. elicitation: the server asks via MCP
				// elicitation. Default client until the Claude Code VS Code
				// extension renders forms (anthropics/claude-code#98978).
				Usage:   "how apply approval is obtained: client (the client's tool-approval prompt) or elicitation (MCP forms)",
				Value:   string(server.ApprovalClient),
				Sources: cli.EnvVars("APPROVAL_MODE"),
			},
			&cli.StringFlag{
				Name: "pdf-extractor-socket",
				// Empty (the default) turns PDF text off: mail-mcp has no PDF
				// parser and never will. With a socket, a sidecar running
				// the pdftext helper does the parsing, killably, in its own
				// container.
				Usage:   "unix socket of the pdftext sidecar; empty disables PDF attachment text",
				Sources: cli.EnvVars("PDF_EXTRACTOR_SOCKET"),
			},
			&cli.StringFlag{
				Name: "pdf-stage-dir",
				// Decoded PDFs are staged under <dir>/pdf for the sidecar. In a
				// pod this is a dedicated emptyDir the sidecar mounts read-only,
				// so it never sees the mail store. Default: the cache dir.
				Usage:   "root under which PDFs are staged for the sidecar (default: --cache-dir)",
				Sources: cli.EnvVars("PDF_STAGE_DIR"),
			},
			&cli.DurationFlag{
				Name:    "refresh-interval",
				Usage:   "how often to refresh the cache from each mailbox; 0 disables refreshing",
				Value:   15 * time.Minute,
				Sources: cli.EnvVars("REFRESH_INTERVAL"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "mail-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// The package-level slog functions (tool failures, imap session lines) and
	// the standard log package write through the default logger; without
	// this they came out as text lines beside the JSON ones.
	slog.SetDefault(log)
	memlimit.Set(log)

	approval, err := server.ParseApprovalMode(cmd.String("approval-mode"))
	if err != nil {
		return err
	}

	accts, err := accounts.Load(cmd.String("accounts"))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(accts))
	for _, a := range accts {
		names = append(names, a.Name)
	}
	log.Info("accounts loaded", "accounts", names, "version", version)

	store, err := cache.Open(cmd.String("cache-dir"))
	if err != nil {
		return err
	}
	// The owner's own addresses, before anything can thread a message.
	owners := map[string][]string{}
	for _, a := range accts {
		owners[a.Name] = []string{a.Username}
	}
	store.SetOwners(owners)
	hist, err := history.Open(cmd.String("history-dir"))
	if err != nil {
		_ = store.Close()
		return err
	}
	org, err := organise.New(store)
	if err != nil {
		_ = hist.Close()
		_ = store.Close()
		return err
	}
	opts := []server.Option{server.WithHistory(hist), server.WithOrganiser(org), server.WithMaxUnelicited(cmd.Int("max-unelicited-apply")), server.WithApprovalMode(approval)}
	if sock := cmd.String("pdf-extractor-socket"); sock != "" {
		if d := cmd.String("pdf-stage-dir"); d != "" {
			store.SetPDFStageDir(d)
		}
		store.SetPDFExtractor(pdfclient.New(sock))
		log.Info("pdf text", "extractor_socket", sock)
	}
	// Refreshers stop, and are waited for, before the cache closes under them.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		_ = store.Close()
		_ = hist.Close()
	}()
	// Background backfills, one goroutine, one after the other: the fts2
	// index (search depends on it) from the blobs on disk, then threading.
	// Both are resumable, use short write transactions and give way to
	// refresh; they never touch IMAP and stop with ctx.
	wg.Add(1)
	go func() {
		defer wg.Done()
		store.RunBackfills(ctx, log)
	}()
	for _, a := range accts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Run(ctx, log, a, cmd.Duration("refresh-interval"))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		store.RunSearchLogRetention(ctx, log)
	}()
	log.Info("approval", "mode", string(approval), "max_unelicited_apply", cmd.Int("max-unelicited-apply"))
	log.Info("history", "dir", cmd.String("history-dir"))
	log.Info("cache", "dir", cmd.String("cache-dir"), "refresh_interval", cmd.Duration("refresh-interval").String())

	if cmd.String("transport") != "http" {
		return server.New(accts, store, version, server.Admin, opts...).Run(ctx, &mcp.StdioTransport{})
	}

	issuer := cmd.String("issuer-url")
	readURL, adminURL := cmd.String("read-resource-url"), cmd.String("admin-resource-url")
	if issuer == "" || readURL == "" || adminURL == "" {
		return fmt.Errorf("--issuer-url, --read-resource-url and --admin-resource-url are required for the http transport")
	}
	if readURL == adminURL {
		// The split is the point: one audience for both would hand a
		// read-only, 7-day token the write tools.
		return fmt.Errorf("--read-resource-url and --admin-resource-url must differ")
	}

	mux := http.NewServeMux()
	for _, ep := range []struct {
		path, url string
		mode      server.Mode
	}{
		{"/mcp", readURL, server.Read},
		{"/mcp-admin", adminURL, server.Admin},
	} {
		auth, err := resource.New(resource.Config{
			IssuerURL:   issuer,
			ResourceURL: ep.url,
			Scope:       cmd.String("scope"),
		})
		if err != nil {
			return fmt.Errorf("auth for %s: %w", ep.path, err)
		}
		s := server.New(accts, store, version, ep.mode, opts...)
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Logger: log})

		mux.Handle(ep.path, auth.Protect(h))
		mux.Handle(ep.path+"/", auth.Protect(h))
		// RFC 9728 puts each resource's own path after the well-known
		// prefix, so the two endpoints get two metadata documents.
		mux.Handle(auth.Path(), auth.Metadata())
		log.Info("endpoint", "path", ep.path, "mode", ep.mode.String(), "resource", ep.url, "metadata", auth.Path())
	}

	// Liveness only, and deliberately does NOT touch any mailbox: a probe that
	// fails when Gmail blips takes the pod out of service for something a
	// restart cannot fix. Unauthenticated on purpose -- a probe carries no token.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	addr := cmd.String("addr")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("serving mcp over http", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
