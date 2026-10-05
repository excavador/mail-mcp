package server

// The admin tools that change only the cache's own tables: sender kinds, tags
// and saved queries. They never touch a mailbox, need no IMAP and no
// per-account write slot, are recorded in the history log (all but
// save_query) and are undone through the undo tool.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/history"
)

func localWrite() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)}
}

// --- set_sender_kind --------------------------------------------------------

type setSenderKindIn struct {
	Account string `json:"account" jsonschema:"account name"`
	Addr    string `json:"addr" jsonschema:"the sender's address, as in senders output"`
	Kind    string `json:"kind" jsonschema:"human, list, transactional or notification"`
}

type setSenderKindOut struct {
	Account       string `json:"account"`
	Addr          string `json:"addr"`
	OldKind       string `json:"old_kind"`
	OldKindSource string `json:"old_kind_source"`
	NewKind       string `json:"new_kind"`
	HistoryID     string `json:"history_id"`
	Notice        string `json:"notice"`
}

func addSetSenderKind(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "set_sender_kind",
		Description: "Set the kind of a sender (human, list, transactional, notification) as the owner's decision: " +
			"it is never overwritten by the rules. Local to the cache, nothing is sent to the mailbox. Recorded in the " +
			"history with the old and new kind; undo restores the old one.",
		Annotations: localWrite(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setSenderKindIn) (*mcp.CallToolResult, setSenderKindOut, error) {
		a, err := account(d.byName, in.Account)
		if err != nil {
			return nil, setSenderKindOut{}, err
		}
		if !cache.ValidSenderKind(in.Kind) {
			return nil, setSenderKindOut{}, errors.New("kind must be human, list, transactional or notification")
		}
		addr := cache.BareAddr(in.Addr)
		if addr == "" || len(addr) > 320 {
			return nil, setSenderKindOut{}, errors.New("addr must be a sender address")
		}
		old, err := d.store.SetSenderKind(ctx, a.Name, addr, in.Kind)
		switch {
		case errors.Is(err, cache.ErrSenderNotFound):
			return nil, setSenderKindOut{}, errors.New("no such sender in this account; find it with senders")
		case err != nil:
			return nil, setSenderKindOut{}, fail("set_sender_kind", "setting the kind failed", err, "account", a.Name)
		}
		rec, err := d.hist.Append(history.Record{
			Account: a.Name, Kind: history.KindSetSenderKind, Target: addr,
			OldKind: old.Kind, OldKindSource: old.Source, NewKind: in.Kind,
		})
		if err != nil {
			// Put it back: a change that is not in the history cannot be undone.
			if rerr := d.store.RestoreSenderKind(ctx, a.Name, addr, in.Kind, old); rerr != nil {
				slog.Warn("set_sender_kind: could not roll back", "err", rerr)
			}
			return nil, setSenderKindOut{}, fail("set_sender_kind", "the history could not be written; nothing was changed", err)
		}
		return nil, setSenderKindOut{
			Account: a.Name, Addr: field(addr), OldKind: old.Kind, OldKindSource: old.Source, NewKind: in.Kind,
			HistoryID: rec.ID, Notice: untrustedFieldsNotice,
		}, nil
	})
}

// --- tag_messages and untag_messages ----------------------------------------

type tagIn struct {
	Account   string   `json:"account" jsonschema:"account name"`
	Tag       string   `json:"tag" jsonschema:"lowercase letters, digits and . _ / - , up to 100 characters, starting with a letter or digit; case/<name> for an investigation"`
	StableIDs []string `json:"stable_ids,omitempty" jsonschema:"the messages, by stable id (at most 5000)"`
	TID       string   `json:"tid,omitempty" jsonschema:"every message of this thread"`
	Query     string   `json:"query,omitempty" jsonschema:"every message this search matches (same words as search)"`
	FTSSyntax bool     `json:"fts_syntax,omitempty" jsonschema:"treat query as a raw FTS5 expression"`
	Folder    string   `json:"folder,omitempty" jsonschema:"query filter: only messages currently in this folder or label"`
	From      string   `json:"from,omitempty" jsonschema:"query filter: only messages whose From contains this text"`
	Since     string   `json:"since,omitempty" jsonschema:"query filter: earliest date, RFC 3339 or YYYY-MM-DD"`
	Until     string   `json:"until,omitempty" jsonschema:"query filter: latest date, RFC 3339 or YYYY-MM-DD"`
	DryRun    bool     `json:"dry_run,omitempty" jsonschema:"count and sample what would change, change nothing"`
}

type tagUntrusted struct {
	Samples []sampleOut `json:"samples"`
}

type tagOut struct {
	Notice    string       `json:"notice"`
	Account   string       `json:"account"`
	Tag       string       `json:"tag"`
	DryRun    bool         `json:"dry_run"`
	Matched   int          `json:"matched" jsonschema:"messages the selector resolved to"`
	Changed   int          `json:"changed" jsonschema:"messages newly tagged (or untagged); 0 on a dry run"`
	Unchanged int          `json:"unchanged,omitempty" jsonschema:"messages that already had (or lacked) the tag"`
	HistoryID string       `json:"history_id,omitempty" jsonschema:"pass to undo"`
	Untrusted tagUntrusted `json:"untrusted"`
}

const tagSampleN = 10

// resolveTagTargets turns the selector in in into stable ids: exactly one of
// stable_ids, tid or query-with-filters, at most MaxTagTargets. requireTag
// restricts a query to messages already carrying that tag (untag).
func resolveTagTargets(ctx context.Context, d writeDeps, account string, in tagIn, requireTag string) ([]string, error) {
	hasQuery := in.Query != "" || in.Folder != "" || in.From != "" || in.Since != "" || in.Until != ""
	n := 0
	for _, b := range []bool{len(in.StableIDs) > 0, in.TID != "", hasQuery} {
		if b {
			n++
		}
	}
	if n != 1 {
		return nil, errors.New("give exactly one of stable_ids, tid, or query with its filters")
	}
	tooMany := fmt.Errorf("that selects more than %d messages; narrow it", cache.MaxTagTargets)
	switch {
	case len(in.StableIDs) > 0:
		if len(in.StableIDs) > cache.MaxTagTargets {
			return nil, tooMany
		}
		ids, err := d.store.ExistingIDs(ctx, account, in.StableIDs)
		if err != nil {
			return nil, fail("tag", "lookup failed", err, "account", account)
		}
		return ids, nil
	case in.TID != "":
		ids, err := d.store.ThreadIDs(ctx, account, in.TID, cache.MaxTagTargets)
		if err != nil {
			return nil, fail("tag", "lookup failed", err, "account", account)
		}
		if len(ids) > cache.MaxTagTargets {
			return nil, tooMany
		}
		return ids, nil
	}
	since, err := parseDate("since", in.Since, false)
	if err != nil {
		return nil, err
	}
	until, err := parseDate("until", in.Until, true)
	if err != nil {
		return nil, err
	}
	ids, err := d.store.ResolveSearch(ctx, cache.SearchQuery{
		Account: account, Text: in.Query, FTSSyntax: in.FTSSyntax, Folder: in.Folder, From: in.From,
		Since: since, Until: until, Tag: requireTag,
	}, cache.MaxTagTargets)
	switch {
	case errors.Is(err, cache.ErrQueryLimit):
		return nil, err
	case errors.Is(err, cache.ErrQuerySyntax):
		slog.Warn("tool failed", "tool", "tag", "msg", "invalid full-text query", "err", err)
		return nil, errors.New("invalid full-text query" + syntaxHint(err))
	case err != nil:
		return nil, fail("tag", "search failed", err, "account", account)
	}
	if len(ids) > cache.MaxTagTargets {
		return nil, tooMany
	}
	return ids, nil
}

func tagSamples(ctx context.Context, d writeDeps, account string, ids []string) []sampleOut {
	out := []sampleOut{}
	if len(ids) > tagSampleN {
		ids = ids[:tagSampleN]
	}
	hits, err := d.store.Summaries(ctx, account, ids)
	if err != nil {
		slog.Warn("tag samples failed", "err", err)
		return out
	}
	for _, h := range hits {
		out = append(out, sampleOut{StableID: h.StableID, Date: h.Date, From: field(h.From), Subject: field(h.Subject)})
	}
	return out
}

func tagFail(err error) error {
	if errors.Is(err, cache.ErrBadTag) || errors.Is(err, cache.ErrTagLimit) {
		return err // fixed text, safe to show
	}
	return nil
}

func addTagMessages(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "tag_messages",
		Description: "Tag messages with a local tag (never written to the mailbox). Select them by stable_ids, by tid " +
			"(a whole thread) or by query plus filters (the same search as the search tool); at most 5000. " +
			"dry_run=true returns the count and a few samples and changes nothing. Recorded in the history; undo " +
			"removes the tags this call added. Use case/<name> for an investigation; search(tag=...) lists the messages.",
		Annotations: localWrite(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in tagIn) (*mcp.CallToolResult, tagOut, error) {
		a, err := account(d.byName, in.Account)
		if err != nil {
			return nil, tagOut{}, err
		}
		tag, err := cache.NormalizeTag(in.Tag)
		if err != nil {
			return nil, tagOut{}, err
		}
		ids, err := resolveTagTargets(ctx, d, a.Name, in, "")
		if err != nil {
			return nil, tagOut{}, err
		}
		out := tagOut{Notice: untrustedFieldsNotice, Account: a.Name, Tag: tag, DryRun: in.DryRun, Matched: len(ids), Untrusted: tagUntrusted{Samples: tagSamples(ctx, d, a.Name, ids)}}
		if in.DryRun || len(ids) == 0 {
			return nil, out, nil
		}
		hid := history.NewID()
		added, err := d.store.AddTags(ctx, a.Name, tag, ids, hid)
		if err != nil {
			if e := tagFail(err); e != nil {
				return nil, tagOut{}, e
			}
			return nil, tagOut{}, fail("tag_messages", "tagging failed", err, "account", a.Name)
		}
		out.Changed, out.Unchanged = len(added), len(ids)-len(added)
		if len(added) == 0 {
			return nil, out, nil
		}
		if _, err := d.hist.Append(history.Record{ID: hid, Account: a.Name, Kind: history.KindTagMessages, Target: tag, Touched: map[string][]string{"tag": added}}); err != nil {
			if _, rerr := d.store.RemoveTags(ctx, a.Name, tag, added, hid); rerr != nil {
				slog.Warn("tag_messages: could not roll back", "err", rerr)
			}
			return nil, tagOut{}, fail("tag_messages", "the history could not be written; nothing was changed", err)
		}
		out.HistoryID = hid
		return nil, out, nil
	})
}

func addUntagMessages(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "untag_messages",
		Description: "Remove a local tag from messages, selected like tag_messages (stable_ids, tid, or query plus " +
			"filters; a query only looks at messages that carry the tag). dry_run=true counts and samples and changes " +
			"nothing. Recorded in the history; undo puts the tag back.",
		Annotations: localWrite(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in tagIn) (*mcp.CallToolResult, tagOut, error) {
		a, err := account(d.byName, in.Account)
		if err != nil {
			return nil, tagOut{}, err
		}
		tag, err := cache.NormalizeTag(in.Tag)
		if err != nil {
			return nil, tagOut{}, err
		}
		ids, err := resolveTagTargets(ctx, d, a.Name, in, tag)
		if err != nil {
			return nil, tagOut{}, err
		}
		out := tagOut{Notice: untrustedFieldsNotice, Account: a.Name, Tag: tag, DryRun: in.DryRun, Matched: len(ids), Untrusted: tagUntrusted{Samples: tagSamples(ctx, d, a.Name, ids)}}
		if in.DryRun || len(ids) == 0 {
			return nil, out, nil
		}
		removed, err := d.store.RemoveTags(ctx, a.Name, tag, ids, "")
		if err != nil {
			return nil, tagOut{}, fail("untag_messages", "untagging failed", err, "account", a.Name)
		}
		out.Changed, out.Unchanged = len(removed), len(ids)-len(removed)
		if len(removed) == 0 {
			return nil, out, nil
		}
		rec, err := d.hist.Append(history.Record{Account: a.Name, Kind: history.KindUntagMessages, Target: tag, Touched: map[string][]string{"tag": removed}})
		if err != nil {
			if _, rerr := d.store.AddTags(ctx, a.Name, tag, removed, ""); rerr != nil {
				slog.Warn("untag_messages: could not roll back", "err", rerr)
			}
			return nil, tagOut{}, fail("untag_messages", "the history could not be written; nothing was changed", err)
		}
		out.HistoryID = rec.ID
		return nil, out, nil
	})
}

// --- save_query -------------------------------------------------------------

type saveQueryIn struct {
	Account   string `json:"account" jsonschema:"account name"`
	Name      string `json:"name" jsonschema:"lowercase letters, digits and . _ / - , up to 100 characters; saving an existing name replaces it"`
	Query     string `json:"query,omitempty" jsonschema:"search words"`
	FTSSyntax bool   `json:"fts_syntax,omitempty" jsonschema:"query is a raw FTS5 expression"`
	Folder    string `json:"folder,omitempty" jsonschema:"only messages currently in this folder or label"`
	From      string `json:"from,omitempty" jsonschema:"only messages whose From contains this text"`
	Since     string `json:"since,omitempty" jsonschema:"earliest date, RFC 3339 or YYYY-MM-DD"`
	Until     string `json:"until,omitempty" jsonschema:"latest date, RFC 3339 or YYYY-MM-DD"`
	Tag       string `json:"tag,omitempty" jsonschema:"only messages carrying this tag"`
	GroupBy   string `json:"group_by,omitempty" jsonschema:"thread (default) or message"`
	Note      string `json:"note,omitempty" jsonschema:"what this query is for (at most 500 characters)"`
}

func addSaveQuery(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "save_query",
		Description: "Save a search under a name for this account (local, cache only). Run it later with " +
			"search(account, saved=<name>); list_saved_queries shows them. Saving an existing name replaces it.",
		Annotations: localWrite(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in saveQueryIn) (*mcp.CallToolResult, map[string]any, error) {
		a, err := account(d.byName, in.Account)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range []struct{ f, v string }{{"since", in.Since}, {"until", in.Until}} {
			if _, err := parseDate(p.f, p.v, false); err != nil {
				return nil, nil, err
			}
		}
		switch in.GroupBy {
		case "", "thread", "message":
		default:
			return nil, nil, errors.New(`group_by must be "thread" or "message"`)
		}
		f := cache.SavedFilters{Query: in.Query, FTSSyntax: in.FTSSyntax, Folder: in.Folder, From: in.From, Since: in.Since, Until: in.Until, Tag: in.Tag, GroupBy: in.GroupBy}
		if err := d.store.SaveQuery(ctx, a.Name, in.Name, f, in.Note); err != nil {
			if errors.Is(err, cache.ErrBadTag) || errors.Is(err, cache.ErrTagLimit) || errors.Is(err, cache.ErrQueryLimit) {
				return nil, nil, err
			}
			return nil, nil, fail("save_query", "saving the query failed", err, "account", a.Name)
		}
		name, _ := cache.NormalizeTag(in.Name)
		return nil, map[string]any{"account": a.Name, "name": name, "saved_at": time.Now().UTC().Format(time.RFC3339)}, nil
	})
}

// --- undo of the local kinds ------------------------------------------------

// undoLocal reverses a set_sender_kind, tag_messages or untag_messages record
// at once (no preview token: there is no mailbox to change) and logs the undo.
func undoLocal(ctx context.Context, d writeDeps, rec history.Record) (map[string]any, error) {
	a, err := account(d.byName, rec.Account)
	if err != nil {
		return nil, err
	}
	if d.hist.Undone(rec.ID) {
		return nil, errors.New("that record was already undone")
	}
	undoID := history.NewID()
	ids := rec.Touched["tag"]
	out := map[string]any{"account": a.Name, "undoes": rec.ID, "kind": rec.Kind, "notice": untrustedFieldsNotice}
	undo := history.Record{ID: undoID, Account: a.Name, Kind: history.KindUndoLocal, Target: rec.Target, Action: rec.Kind, Undoes: rec.ID}
	switch rec.Kind {
	case history.KindTagMessages:
		removed, err := d.store.RemoveTags(ctx, a.Name, rec.Target, ids, rec.ID)
		if err != nil {
			return nil, fail("undo", "undo failed", err, "account", a.Name)
		}
		out["untagged"] = len(removed)
		undo.Touched = map[string][]string{"tag": removed}
	case history.KindUntagMessages:
		added, err := d.store.AddTags(ctx, a.Name, rec.Target, ids, undoID)
		if err != nil {
			if e := tagFail(err); e != nil {
				return nil, e
			}
			return nil, fail("undo", "undo failed", err, "account", a.Name)
		}
		out["retagged"] = len(added)
		undo.Touched = map[string][]string{"tag": added}
	case history.KindSetSenderKind:
		err := d.store.RestoreSenderKind(ctx, a.Name, rec.Target, rec.NewKind, cache.SenderKindState{Kind: rec.OldKind, Source: rec.OldKindSource})
		switch {
		case errors.Is(err, cache.ErrSenderChanged):
			return nil, errors.New("the sender's kind was changed again since; undo the later change first")
		case errors.Is(err, cache.ErrSenderNotFound):
			return nil, errors.New("the sender is no longer in the cache")
		case err != nil:
			return nil, fail("undo", "undo failed", err, "account", a.Name)
		}
		out["restored_kind_source"] = rec.OldKindSource
	default:
		return nil, errors.New("only an applied intent, a tag change or a sender kind change can be undone")
	}
	if _, err := d.hist.Append(undo); err != nil {
		return nil, fail("undo", "undone, but the history could not be written", err)
	}
	out["history_id"] = undoID
	return out, nil
}
