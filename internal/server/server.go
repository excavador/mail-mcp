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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
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

// New builds one MCP server carrying the tools for mode.
func New(accts []accounts.Account, store *cache.Cache, version string, mode Mode) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mail-" + mode.String(), Version: version}, nil)
	byName := map[string]accounts.Account{}
	for _, a := range accts {
		byName[a.Name] = a
	}

	addReads(s, accts, byName)
	addListFolders(s, byName, store)
	addSearch(s, byName, store)
	addFetchMessage(s, byName, store)
	addSenderStats(s, byName, store)
	addCacheStatus(s, store)
	// Write tools (create_folder, apply, undo, reapply) are registered only
	// for Admin, and arrive with the organise and history work.
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
			return nil, nil, err
		}
		return nil, map[string]any{"accounts": st}, nil
	})
}
