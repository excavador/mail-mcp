package server

// The read tools over the senders table, tags and saved queries. All are
// annotated read-only and registered in both modes; none writes to a mailbox.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
)

type sendersIn struct {
	Account      string `json:"account" jsonschema:"account name"`
	Query        string `json:"query,omitempty" jsonschema:"substring of the sender's address, domain or display name"`
	Kind         string `json:"kind,omitempty" jsonschema:"human, list, transactional or notification"`
	MinMsgs      int    `json:"min_msgs,omitempty" jsonschema:"only senders with at least this many messages"`
	NeverReplied bool   `json:"never_replied,omitempty" jsonschema:"only senders the owner never replied to (n_replied_by_me = 0)"`
	Since        string `json:"since,omitempty" jsonschema:"only senders whose last message is on or after this date, RFC 3339 or YYYY-MM-DD"`
	Sort         string `json:"sort,omitempty" jsonschema:"count (default, most messages first), last_at (latest first) or first_at (most recently first seen first)"`
	Limit        int    `json:"limit,omitempty" jsonschema:"rows per page, default 20, at most 100"`
	Cursor       string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page of the same query"`
}

type senderRowOut struct {
	Addr         string `json:"addr"`
	Name         string `json:"name,omitempty"`
	Kind         string `json:"kind"`
	KindSource   string `json:"kind_source"`
	NMsgs        int    `json:"n_msgs"`
	NRepliedByMe int    `json:"n_replied_by_me"`
	LastAt       string `json:"last_at,omitempty"`
	ListID       string `json:"list_id,omitempty"`
}

type sendersOut struct {
	Account    string         `json:"account"`
	Total      int            `json:"total" jsonschema:"senders that matched"`
	Senders    []senderRowOut `json:"senders"`
	NextCursor string         `json:"next_cursor,omitempty" jsonschema:"pass as cursor to get the next page"`
	Note       string         `json:"note,omitempty"`
	Notice     string         `json:"notice"`
}

func addSenders(s *mcp.Server, byName map[string]accounts.Account, store *cache.Cache) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "senders",
		Description: "List the senders in one account from the local cache, one row per address: kind (human, list, " +
			"transactional, notification), who set it (rule, llm or owner), message count, how many messages the owner " +
			"sent in reply to this sender (n_replied_by_me), last message date and the sender's usual List-Id. " +
			"Filter by query (address, domain or name), kind, min_msgs, never_replied and since; sort by count, " +
			"last_at or first_at; page with next_cursor. \"Transactional senders I never replied to\" is " +
			"kind=transactional, never_replied=true. Kinds come from header rules and can be wrong; the owner's " +
			"correction (set_sender_kind, admin) always wins. Counts are complete once cache_status shows the " +
			"senders job complete.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendersIn) (*mcp.CallToolResult, sendersOut, error) {
		a, err := account(byName, in.Account)
		if err != nil {
			return nil, sendersOut{}, err
		}
		if in.Kind != "" && !cache.ValidSenderKind(in.Kind) {
			return nil, sendersOut{}, errors.New("kind must be human, list, transactional or notification")
		}
		if in.Sort != "" && !slices.Contains(cache.SenderSorts, in.Sort) {
			return nil, sendersOut{}, errors.New("sort must be count, last_at or first_at")
		}
		since, err := parseDate("since", in.Since, false)
		if err != nil {
			return nil, sendersOut{}, err
		}
		limit := in.Limit
		switch {
		case limit <= 0:
			limit = 20
		case limit > 100:
			limit = 100
		}
		key := cache.QueryKey(a.Name, in.Query, in.Kind, fmt.Sprint(in.MinMsgs), fmt.Sprint(in.NeverReplied), in.Since, in.Sort)
		offset, err := decodeCursor(in.Cursor, key)
		if err != nil {
			return nil, sendersOut{}, err
		}
		rows, total, err := store.ListSenders(ctx, cache.SenderQuery{
			Account: a.Name, Query: in.Query, Kind: in.Kind, MinMsgs: in.MinMsgs, NeverReplied: in.NeverReplied,
			Since: since, Sort: in.Sort, Limit: limit, Offset: offset,
		})
		switch {
		case errors.Is(err, cache.ErrQueryLimit):
			return nil, sendersOut{}, err
		case err != nil:
			return nil, sendersOut{}, fail("senders", "sender listing failed", err, "account", a.Name)
		}
		out := sendersOut{Account: a.Name, Total: total, Senders: make([]senderRowOut, 0, len(rows)), Notice: untrustedFieldsNotice}
		for _, r := range rows {
			out.Senders = append(out.Senders, senderRowOut{
				Addr: r.Addr, Name: field(r.Name), Kind: r.Kind, KindSource: r.KindSource, NMsgs: r.NMsgs,
				NRepliedByMe: r.NRepliedByMe, LastAt: fmtTime(r.LastAt), ListID: field(r.ListID),
			})
		}
		if offset+len(rows) < total && len(rows) > 0 {
			out.NextCursor = encodeCursor(offset+len(rows), key)
		}
		if st, err := store.SendersStatus(ctx); err == nil && !st.Complete {
			out.Note = fmt.Sprintf("the senders job is %s (%d of %d messages counted); counts are partial until it completes", st.State, st.Done, st.Total)
		}
		return nil, out, nil
	})
}

type listTagsIn struct {
	Account string `json:"account" jsonschema:"account name"`
}

type tagOutRow struct {
	Tag       string `json:"tag"`
	Count     int    `json:"count"`
	LastAdded string `json:"last_added"`
}

type listTagsOut struct {
	Account string      `json:"account"`
	Tags    []tagOutRow `json:"tags"`
	Count   int         `json:"count"`
	Notice  string      `json:"notice"`
}

func addListTags(s *mcp.Server, byName map[string]accounts.Account, store *cache.Cache) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_tags",
		Description: "List the tags in one account with how many messages carry each and when one was last added. " +
			"Tags are local labels kept in the cache (never written to the mailbox); case/<name> marks an investigation. " +
			"Search the messages of a tag with search(tag=...).",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTagsIn) (*mcp.CallToolResult, listTagsOut, error) {
		a, err := account(byName, in.Account)
		if err != nil {
			return nil, listTagsOut{}, err
		}
		ts, err := store.ListTags(ctx, a.Name)
		if err != nil {
			return nil, listTagsOut{}, fail("list_tags", "tag listing failed", err, "account", a.Name)
		}
		out := listTagsOut{Account: a.Name, Tags: make([]tagOutRow, 0, len(ts)), Notice: untrustedFieldsNotice}
		for _, t := range ts {
			out.Tags = append(out.Tags, tagOutRow{Tag: field(t.Tag), Count: t.Count, LastAdded: fmtTime(t.LastAdded)})
		}
		out.Count = len(out.Tags)
		return nil, out, nil
	})
}

type savedQueryOut struct {
	Name      string             `json:"name"`
	Filters   cache.SavedFilters `json:"query"`
	Note      string             `json:"note,omitempty"`
	CreatedAt string             `json:"created_at"`
}

type listSavedOut struct {
	Account string          `json:"account"`
	Queries []savedQueryOut `json:"queries"`
	Count   int             `json:"count"`
	Notice  string          `json:"notice"`
}

func cleanFilters(f cache.SavedFilters) cache.SavedFilters {
	f.Query, f.Folder, f.From = field(f.Query), field(f.Folder), field(f.From)
	f.Since, f.Until, f.Tag, f.GroupBy = field(f.Since), field(f.Until), field(f.Tag), field(f.GroupBy)
	f.ExcludeFrom, f.ExcludeKind = fieldAll(f.ExcludeFrom), fieldAll(f.ExcludeKind)
	return f
}

func addListSavedQueries(s *mcp.Server, byName map[string]accounts.Account, store *cache.Cache) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_saved_queries",
		Description: "List the saved searches of one account (name, the search inputs, note). Run one with " +
			"search(account, saved=<name>); inputs given to search override the saved ones.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTagsIn) (*mcp.CallToolResult, listSavedOut, error) {
		a, err := account(byName, in.Account)
		if err != nil {
			return nil, listSavedOut{}, err
		}
		qs, err := store.ListSavedQueries(ctx, a.Name)
		if err != nil {
			return nil, listSavedOut{}, fail("list_saved_queries", "listing saved queries failed", err, "account", a.Name)
		}
		out := listSavedOut{Account: a.Name, Queries: make([]savedQueryOut, 0, len(qs)), Notice: untrustedFieldsNotice}
		for _, q := range qs {
			out.Queries = append(out.Queries, savedQueryOut{Name: field(q.Name), Filters: cleanFilters(q.Filters), Note: field(strings.TrimSpace(q.Note)), CreatedAt: fmtTime(q.CreatedAt)})
		}
		out.Count = len(out.Queries)
		return nil, out, nil
	})
}
