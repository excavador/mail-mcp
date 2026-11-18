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
	"github.com/urfave/cli/v3"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/calendar"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/leader"
	"github.com/excavador/mail-mcp/internal/memlimit"
	"github.com/excavador/mail-mcp/internal/organise"
	"github.com/excavador/mail-mcp/internal/pdfclient"
	"github.com/excavador/mail-mcp/internal/server"
)

// version is overridden at build time.
var version = "dev"

// shutdownTimeout bounds how long in-flight requests get after SIGTERM; keep
// it below the pod's terminationGracePeriodSeconds.
const shutdownTimeout = 20 * time.Second

func main() {
	// Everything this process creates (the cache, its SQLite WAL and shm
	// files) holds mail: owner-only, whatever the caller's umask was.
	syscall.Umask(0o077)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// `mail-mcp calendar-login ...` is the owner's one-time consent helper,
	// run on their own machine; it needs none of the server's flags.
	if len(os.Args) > 1 && os.Args[1] == "calendar-login" {
		if err := calendar.Login(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "mail-mcp calendar-login: %v\n", err)
			stop()
			os.Exit(1)
		}
		return
	}

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
				// Loopback only: the resource-proxy sidecars in the same pod are
				// the sole way in. Anything else is refused at start unless
				// --allow-non-loopback is given.
				Usage:   "MCP listen address for the http transport; must be a loopback address",
				Value:   "127.0.0.1:8081",
				Sources: cli.EnvVars("ADDR"),
			},
			&cli.BoolFlag{
				Name: "allow-non-loopback",
				// This server checks no token. A non-loopback --addr is
				// reachable by whatever can route to it, with no authentication.
				// Only for a setup where something else authenticates every
				// request and is the only way in.
				Usage:   "allow a non-loopback --addr; the server checks no token, so only with an authenticating proxy as the only way in",
				Sources: cli.EnvVars("ALLOW_NON_LOOPBACK"),
			},
			&cli.StringFlag{
				Name: "ops-addr",
				// Serves /healthz and nothing of MCP, for the kubelet. Empty
				// turns it off.
				Usage:   "listen address of the ops listener (/healthz only); empty disables it",
				Value:   ":9090",
				Sources: cli.EnvVars("OPS_ADDR"),
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
			&cli.IntFlag{
				Name: "max-unelicited-label",
				// A label adds and never removes, and its undo removes only
				// what it added, so the client-approval cap is larger.
				Usage:   "most messages one label apply (or the undo of one) may change when the client does not support elicitation",
				Value:   server.DefaultMaxUnelicitedLabel,
				Sources: cli.EnvVars("MAX_UNELICITED_LABEL"),
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
			&cli.StringFlag{
				Name: "leader-election",
				// Only the holder of a Kubernetes Lease runs the background
				// writers (refresh, backfills, retention), so a RollingUpdate
				// can overlap two pods. Every pod serves requests.
				Usage:   "auto (on when running in a cluster), true or false (always the leader, for local runs)",
				Value:   "auto",
				Sources: cli.EnvVars("LEADER_ELECTION"),
			},
			&cli.StringFlag{
				Name:    "lease-name",
				Usage:   "name of the coordination.k8s.io Lease that elects the pod running the background jobs",
				Value:   "mail-mcp",
				Sources: cli.EnvVars("LEASE_NAME"),
			},
			&cli.StringFlag{
				Name:    "lease-namespace",
				Usage:   "namespace of the Lease (default: the pod's own namespace; set POD_NAMESPACE from the downward API)",
				Sources: cli.EnvVars("POD_NAMESPACE"),
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

	// Validated before anything is opened or served: a bad mode or an unknown
	// namespace is a startup error, not a healthy pod that never runs jobs.
	elector, err := leader.New(leader.Config{
		Mode:      cmd.String("leader-election"),
		Name:      cmd.String("lease-name"),
		Namespace: cmd.String("lease-namespace"),
	})
	if err != nil {
		return err
	}

	store, err := cache.Open(cmd.String("cache-dir"))
	if err != nil {
		return err
	}
	// The owner's own addresses, before anything can thread a message.
	owners := map[string][]string{}
	for _, a := range accts {
		owners[a.Name] = a.OwnerAddrs()
		if len(a.Aliases) > 0 {
			log.Info("owner aliases", "account", a.Name, "aliases", len(a.Aliases))
		}
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
	opts := []server.Option{server.WithHistory(hist), server.WithOrganiser(org), server.WithMaxUnelicited(cmd.Int("max-unelicited-apply")), server.WithMaxUnelicitedLabel(cmd.Int("max-unelicited-label")), server.WithApprovalMode(approval)}
	cals, err := calendar.NewRegistry(ctx, accts)
	if err != nil {
		_ = hist.Close()
		_ = store.Close()
		return err
	}
	if len(cals) > 0 {
		opts = append(opts, server.WithCalendars(cals))
		calNames := make([]string, 0, len(cals))
		for n, c := range cals {
			calNames = append(calNames, fmt.Sprintf("%s(write=%t)", n, c.Write))
		}
		log.Info("calendars", "accounts", calNames)
	}
	if sock := cmd.String("pdf-extractor-socket"); sock != "" {
		if d := cmd.String("pdf-stage-dir"); d != "" {
			store.SetPDFStageDir(d)
		}
		store.SetPDFExtractor(pdfclient.New(sock))
		log.Info("pdf text", "extractor_socket", sock)
	}
	// The background jobs run only while this pod holds the Lease. They stop,
	// and are waited for, before the cache closes under them. Their context is
	// not the request context: it ends after the HTTP server has drained, so
	// the Lease is released last and a successor takes over from a quiet pod.
	jobsCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	defer func() {
		cancelJobs()
		wg.Wait()
		_ = store.Close()
		_ = hist.Close()
	}()
	jobs := func(jctx context.Context) {
		// Owner-derived recount: it resets counters and writes markers, so
		// the leader does it, once per leadership, before the backfills.
		// A changed username/alias set restarts the recount; the work itself
		// runs in RunBackfills.
		recountOK := true
		if err := store.ReconcileOwners(log); err != nil {
			log.Error("owner reconcile failed; the backfills are skipped until the next leadership", "error", err.Error())
			recountOK = false
		}
		var jw sync.WaitGroup
		defer jw.Wait()
		// Background backfills, one goroutine, one after the other: the fts2
		// index (search depends on it) from the blobs on disk, then threading,
		// senders, entities and PDF text. All are resumable, use short write
		// transactions and give way to refresh; they never touch IMAP and stop
		// with the context.
		if recountOK {
			jw.Add(1)
			go func() {
				defer jw.Done()
				store.RunBackfills(jctx, log)
			}()
		}
		for _, a := range accts {
			jw.Add(1)
			go func() {
				defer jw.Done()
				store.Run(jctx, log, a, cmd.Duration("refresh-interval"))
			}()
		}
		jw.Add(1)
		go func() {
			defer jw.Done()
			store.RunSearchLogRetention(jctx, log)
		}()
		<-jctx.Done()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := elector.Run(jobsCtx, log, jobs); err != nil {
			log.Error("leader election failed; no background jobs run in this pod", "error", err.Error())
		}
	}()
	log.Info("approval", "mode", string(approval), "max_unelicited_apply", cmd.Int("max-unelicited-apply"), "max_unelicited_label", cmd.Int("max-unelicited-label"))
	log.Info("history", "dir", cmd.String("history-dir"))
	log.Info("cache", "dir", cmd.String("cache-dir"), "refresh_interval", cmd.Duration("refresh-interval").String())

	if cmd.String("transport") != "http" {
		return server.New(accts, store, version, server.Admin, opts...).Run(ctx, &mcp.StdioTransport{})
	}

	addr := cmd.String("addr")
	if cmd.Bool("allow-non-loopback") {
		log.Warn("non-loopback MCP address allowed: this server checks no token", "addr", addr)
	} else if err := requireLoopback(addr); err != nil {
		return err
	}
	ops := cmd.String("ops-addr")
	if ops != "" && ops == addr {
		return fmt.Errorf("--ops-addr and --addr must differ")
	}

	mux := newMCPMux(log,
		server.New(accts, store, version, server.Read, opts...),
		server.New(accts, store, version, server.Admin, opts...))
	log.Info("endpoint", "path", "/mcp", "mode", server.Read.String())
	log.Info("endpoint", "path", "/mcp-admin", "mode", server.Admin.String())

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	var opsSrv *http.Server
	if ops != "" {
		opsSrv = &http.Server{Addr: ops, Handler: newOpsMux(), ReadHeaderTimeout: 10 * time.Second}
	}

	// On SIGTERM: stop accepting, let in-flight requests finish (bounded),
	// and only then return, so the deferred cancel of the jobs (which
	// releases the Lease) and the closing of the cache come after the last
	// request.
	srvCtx, cancelSrv := context.WithCancel(ctx)
	defer cancelSrv()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-srvCtx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Warn("http shutdown timed out; closing remaining connections", "error", err.Error())
			_ = srv.Close()
		}
		if opsSrv != nil {
			_ = opsSrv.Shutdown(shutdown)
		}
	}()

	// The ops listener answers the kubelet while the MCP listener is up; if
	// either stops with an error the process ends and the pod restarts.
	failed := make(chan error, 2)
	if opsSrv != nil {
		go func() {
			log.Info("serving ops over http", "addr", ops)
			if err := opsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				failed <- fmt.Errorf("ops listener: %w", err)
			}
		}()
	}
	go func() {
		log.Info("serving mcp over http", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			failed <- fmt.Errorf("mcp listener: %w", err)
		}
	}()
	select {
	case err := <-failed:
		cancelSrv()
		<-drained
		return err
	case <-drained:
	}
	return nil
}
