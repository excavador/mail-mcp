// Package server builds the MCP servers mail-mcp exposes.
//
// There are two, from one process, because sluis grants sessions longer than
// 24 hours only to read-only resources (sluis ADR 0033). Searching and
// reading happens daily and should not need a daily sign-in; organising
// happens rarely and can. So:
//
//   - Read  — served at the read-only resource (/mail). Read tools only.
//   - Admin — served at the write resource (/mail-admin). Read tools plus the
//     tools that change a mailbox.
//
// There is no delete tool in either, and there will not be one. Moves are
// reversible; deletes are not.
package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/organise"
)

// Mode selects which tool set a server carries.
type Mode int

const (
	Read Mode = iota
	Admin
)

func (m Mode) String() string {
	if m == Admin {
		return "admin"
	}
	return "read"
}

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

// Option supplies what the history and write tools need.
type Option func(*options)

type options struct {
	hist          *history.Store
	org           *organise.Organiser
	maxUnelicited int
	approvalMode  ApprovalMode
}

// ApprovalMode says how apply_intent obtains the owner's approval.
type ApprovalMode string

const (
	// ApprovalClient never elicits: approval is the client's own tool-approval
	// prompt, capped at maxUnelicited messages. The default, because today's
	// clients that advertise elicitation do not all render it.
	ApprovalClient ApprovalMode = "client"
	// ApprovalElicitation asks the owner through MCP elicitation when the
	// client declares the capability.
	ApprovalElicitation ApprovalMode = "elicitation"
)

// ParseApprovalMode validates s.
func ParseApprovalMode(s string) (ApprovalMode, error) {
	switch m := ApprovalMode(s); m {
	case ApprovalClient, ApprovalElicitation:
		return m, nil
	}
	return "", fmt.Errorf("unknown approval mode %q (want %q or %q)", s, ApprovalClient, ApprovalElicitation)
}

// WithApprovalMode sets how apply_intent is approved. Default: ApprovalClient.
func WithApprovalMode(m ApprovalMode) Option { return func(o *options) { o.approvalMode = m } }

// DefaultMaxUnelicited is the largest apply a client without elicitation may run.
const DefaultMaxUnelicited = 50

// WithMaxUnelicited sets the most messages one apply_intent may change when the
// client cannot show the owner a confirmation of its own.
func WithMaxUnelicited(n int) Option { return func(o *options) { o.maxUnelicited = n } }

// WithHistory adds list_history (both modes) and, with WithOrganiser, lets
// Admin carry the write tools. One store is shared by every server.
func WithHistory(h *history.Store) Option { return func(o *options) { o.hist = h } }

// WithOrganiser supplies the preview and apply machinery. It is shared by
// every server so a token and the per-account write slot mean the same
// thing wherever they are used.
func WithOrganiser(g *organise.Organiser) Option { return func(o *options) { o.org = g } }

// New builds one MCP server carrying the tools for mode.
func New(accts []accounts.Account, store *cache.Cache, version string, mode Mode, opts ...Option) *mcp.Server {
	o := options{maxUnelicited: DefaultMaxUnelicited, approvalMode: ApprovalClient}
	for _, f := range opts {
		f(&o)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "mail-" + mode.String(), Version: version}, &mcp.ServerOptions{Logger: slog.Default()})
	byName := map[string]accounts.Account{}
	for _, a := range accts {
		byName[a.Name] = a
	}

	addReads(s, accts, byName)
	addListFolders(s, byName, store)
	addSearch(s, byName, store)
	addFetchMessage(s, byName, store)
	addGetThread(s, byName, store)
	addSearchStats(s, store)
	addSenderStats(s, byName, store)
	addSenders(s, byName, store)
	addListTags(s, byName, store)
	addListSavedQueries(s, byName, store)
	addCacheStatus(s, store)
	if o.hist != nil {
		addListHistory(s, byName, o.hist)
	}
	// Write tools exist only on the Admin server: create_folder, preview_intent,
	// apply_intent, undo and reapply. The Read server cannot be handed them by
	// any option.
	if mode == Admin && o.hist != nil && o.org != nil {
		addWriteTools(s, writeDeps{byName: byName, store: store, hist: o.hist, org: o.org, maxUnelicited: o.maxUnelicited, approvalMode: o.approvalMode})
	}
	return s
}

type accountInfo struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Username string `json:"username"`
	// How folders behave, so a caller does not have to know the providers.
	Folders string `json:"folders"`
}

func addReads(s *mcp.Server, accts []accounts.Account, byName map[string]accounts.Account) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_accounts",
		Description: "List the mailboxes this server can reach, with the provider behind each. " +
			"Gmail organises with labels (a message can carry many; moving out of the inbox means " +
			"label plus archive). Proton organises with folders (exactly one per message) and labels.",
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		out := make([]accountInfo, 0, len(accts))
		for _, a := range accts {
			fold := "labels: many per message; [Gmail]/All Mail holds everything"
			if a.Provider == accounts.Proton {
				fold = "Folders/... one per message; Labels/... many per message"
			}
			out = append(out, accountInfo{
				Name: a.Name, Provider: string(a.Provider), Username: a.Username, Folders: fold,
			})
		}
		return nil, map[string]any{"accounts": out}, nil
	})
}

// addCacheStatus registers cache_status, which reports what the local cache
// holds. It reads only the cache, never a mailbox, so it is as cheap and as
// safe as a tool gets.
func addCacheStatus(s *mcp.Server, store *cache.Cache) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "cache_status",
		Description: "Report what the local message cache holds for each account: cached messages, " +
			"folder memberships, folders, and when the cache was last refreshed from the mailbox.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		st, err := store.Status(ctx)
		if err != nil {
			return nil, nil, fail("cache_status", "cache status failed", err)
		}
		out := map[string]any{"accounts": st}
		if bf, err := store.BackfillStatus(ctx); err == nil {
			out["fts2_backfill"] = bf
		}
		if tb, err := store.ThreadsBackfillStatus(ctx); err == nil {
			out["threads_backfill"] = tb
		}
		if sj, err := store.SendersStatus(ctx); err == nil {
			out["senders_job"] = sj
		}
		return nil, out, nil
	})
}
