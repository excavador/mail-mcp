package server

// The tools that change a mailbox, and the history that records them. Writes
// exist only as intents that were previewed and then approved: preview_intent
// (or undo, or reapply) resolves an intent into a fixed set of messages and
// returns a token; apply_intent executes that token after the owner approves.
// None of this is registered on the Read server, and no tool here deletes.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/organise"
)

// writeDeps is everything the write tools share.
type writeDeps struct {
	byName map[string]accounts.Account
	store  *cache.Cache
	hist   *history.Store
	org    *organise.Organiser
}

func ptr[T any](v T) *T { return &v }

// writeFail maps an organise or IMAP error to what the client may see: the
// fixed text of a refusal, or a generic message with the detail in the log.
func writeFail(tool string, err error, account string) error {
	var se organise.SafeError
	if errors.As(err, &se) {
		return se
	}
	return imapFail(tool, err, account)
}

func addWriteTools(s *mcp.Server, d writeDeps) {
	addCreateFolder(s, d)
	addPreviewIntent(s, d)
	addApplyIntent(s, d)
	addUndo(s, d)
	addReapply(s, d)
}

// --- create_folder ----------------------------------------------------------

type createFolderIn struct {
	Account string `json:"account" jsonschema:"account name"`
	Name    string `json:"name" jsonschema:"folder or label name; on Proton it must start with Folders/ or Labels/"`
}

func addCreateFolder(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_folder",
		Description: "Create a folder (Gmail: a label) so an intent can target it. Safe to repeat: one that already " +
			"exists is fine. On Proton the name must start with Folders/ or Labels/. Recorded in the history.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createFolderIn) (*mcp.CallToolResult, map[string]any, error) {
		a, err := account(d.byName, in.Account)
		if err != nil {
			return nil, nil, err
		}
		if err := organise.ValidateNewFolder(a.Provider, in.Name); err != nil {
			return nil, nil, err // organise.SafeError: fixed wording
		}
		release, err := d.org.Acquire(a.Name)
		if err != nil {
			return nil, nil, err
		}
		defer release()
		if err := d.org.CreateFolder(ctx, a, in.Name); err != nil {
			return nil, nil, writeFail("create_folder", err, a.Name)
		}
		rec, err := d.hist.Append(history.Record{Account: a.Name, Kind: history.KindCreateFolder, Target: in.Name})
		if err != nil {
			return nil, nil, fail("create_folder", "folder created but the history could not be written", err)
		}
		return nil, map[string]any{"account": a.Name, "name": field(in.Name), "history_id": rec.ID}, nil
	})
}

// --- preview_intent ---------------------------------------------------------

type criterionIn struct {
	Folder          string `json:"folder" jsonschema:"source folder (Gmail: label) the messages are in now"`
	From            string `json:"from,omitempty" jsonschema:"sender's bare address, exact match, case-insensitive"`
	To              string `json:"to,omitempty" jsonschema:"text contained in the To header"`
	SubjectContains string `json:"subject_contains,omitempty" jsonschema:"text contained in the subject"`
	ListID          string `json:"list_id,omitempty" jsonschema:"List-Id, exact match, case-insensitive"`
	GitHubReason    string `json:"github_reason,omitempty" jsonschema:"X-GitHub-Reason, exact match, case-insensitive"`
	Since           string `json:"since,omitempty" jsonschema:"earliest message date, RFC 3339 or YYYY-MM-DD"`
	Before          string `json:"before,omitempty" jsonschema:"messages dated before this, RFC 3339 or YYYY-MM-DD"`
}

type previewIn struct {
	Account   string      `json:"account" jsonschema:"account name"`
	Criterion criterionIn `json:"criterion" jsonschema:"which messages; at least one of from, list_id, github_reason, subject_contains, to"`
	Target    string      `json:"target" jsonschema:"folder or label to put them in; it must exist (create_folder)"`
	Action    string      `json:"action" jsonschema:"move (leave the source) or label (keep the source and add the target)"`
}

type sampleOut struct {
	StableID string    `json:"stable_id"`
	Date     time.Time `json:"date"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
}

type previewUntrusted struct {
	Samples []sampleOut `json:"samples"`
}

type previewOut struct {
	Notice       string           `json:"notice"`
	PreviewToken string           `json:"preview_token" jsonschema:"pass to apply_intent to execute exactly this preview"`
	ExpiresAt    time.Time        `json:"expires_at"`
	Account      string           `json:"account"`
	Kind         string           `json:"kind"`
	Action       string           `json:"action"`
	Source       string           `json:"source"`
	Target       string           `json:"target"`
	Matched      int              `json:"matched"`
	NotFound     int              `json:"not_found,omitempty" jsonschema:"undo: recorded messages no longer in the folder"`
	Sampled      int              `json:"sampled"`
	Untrusted    previewUntrusted `json:"untrusted"`
}

func previewResult(p *organise.Preview) previewOut {
	out := previewOut{
		Notice:       untrustedFieldsNotice + " Nothing has changed yet: apply_intent with this token, once the owner approves, executes it.",
		PreviewToken: p.Token, ExpiresAt: p.Expires, Account: p.Account, Kind: p.Kind,
		Action: p.Intent.Action, Source: field(p.Intent.Criterion.Folder), Target: field(p.Intent.Target),
		Matched: p.Matched, NotFound: p.Missing, Sampled: len(p.Samples),
		Untrusted: previewUntrusted{Samples: []sampleOut{}},
	}
	for _, h := range p.Samples {
		out.Untrusted.Samples = append(out.Untrusted.Samples, sampleOut{
			StableID: h.StableID, Date: h.Date, From: field(h.From), Subject: field(h.Subject),
		})
	}
	return out
}

func (c criterionIn) toIntent(account, target, action string) (organise.Intent, error) {
	since, err := parseDate("since", c.Since, false)
	if err != nil {
		return organise.Intent{}, err
	}
	before, err := parseDate("before", c.Before, false)
	if err != nil {
		return organise.Intent{}, err
	}
	return organise.Intent{
		Account: account, Target: target, Action: action,
		Criterion: organise.Criterion{
			Folder: c.Folder, From: c.From, To: c.To, SubjectContains: c.SubjectContains, ListID: c.ListID,
			GitHubReason: c.GitHubReason, Since: since, Before: before,
		},
	}, nil
}

func addPreviewIntent(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "preview_intent",
		Description: "Preview an organising intent: which cached messages in a folder match a criterion, what would " +
			"happen to them, and a preview token. Nothing changes. Show the owner the count and samples; only " +
			"apply_intent with the token (and the owner's approval) changes anything. Gmail: move = UID MOVE " +
			"(removes the source label, adds the target), label = copy (keeps the source). Proton: move needs a " +
			"Folders/... target, label a Labels/... target.",
		// Read-only in effect (cache only); registered on the admin server only.
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in previewIn) (*mcp.CallToolResult, previewOut, error) {
		a, err := account(d.byName, in.Account)
		if err != nil {
			return nil, previewOut{}, err
		}
		intent, err := in.Criterion.toIntent(a.Name, in.Target, in.Action)
		if err != nil {
			return nil, previewOut{}, err
		}
		p, err := d.org.PreviewIntent(ctx, a, intent, organise.KindApply, time.Time{}, "")
		if err != nil {
			return nil, previewOut{}, previewFail("preview_intent", err, a.Name)
		}
		return nil, previewResult(p), nil
	})
}

// previewFail: intents are validated with fixed wording (organise.SafeError),
// so a refusal is shown; a cache failure is not.
func previewFail(tool string, err error, account string) error {
	var se organise.SafeError
	if errors.As(err, &se) {
		return se
	}
	return fail(tool, "preview failed", err, "account", account)
}

// --- apply_intent -----------------------------------------------------------

type applyIn struct {
	PreviewToken string `json:"preview_token" jsonschema:"the token preview_intent, undo or reapply returned"`
	Approved     bool   `json:"approved" jsonschema:"must be true, and only after the owner has seen the preview and agreed"`
}

type applyOut struct {
	Account    string `json:"account"`
	Kind       string `json:"kind"`
	Action     string `json:"action"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Matched    int    `json:"matched" jsonschema:"messages in the approved preview"`
	Done       int    `json:"done" jsonschema:"messages acted on"`
	Skipped    int    `json:"skipped" jsonschema:"approved messages no longer matching, or gone"`
	ApprovedBy string `json:"approved_by"`
	HistoryID  string `json:"history_id"`
}

// supportsElicitation reports whether the client declared the elicitation
// capability when it initialised the session: ServerSession.InitializeParams
// (go-sdk v1.7.0 mcp/server.go:1959) carries the client's capabilities, and
// ServerSession.Elicit (mcp/server.go:1654) makes the same check before it
// sends, refusing with "client does not support elicitation" otherwise.
func supportsElicitation(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	ip := ss.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil
}

// approve obtains the owner's approval of p and says through which channel.
//
// Where the client supports elicitation the server asks the owner itself: a
// form naming the account, count, source, target and action, and nothing
// proceeds without an explicit accept. That approval cannot be given by the
// model, whatever it was told to do.
//
// Where it does not, the only gate is the client's own tool-approval prompt:
// apply_intent is not read-only, so a client that asks before running such
// tools asks here. That is weaker (the model fills in "approved"), and is
// recorded as "client-tool-approval" so the history shows which it was.
func approve(ctx context.Context, ss *mcp.ServerSession, p *organise.Preview) (string, error) {
	if !supportsElicitation(ss) {
		return history.ApprovedClientTool, nil
	}
	msg := fmt.Sprintf("Apply %s of %d message(s) in account %s: %s -> %s? "+
		"(%s; undo is available from the history.)",
		p.Intent.Action, p.Matched, p.Account, field(p.Intent.Criterion.Folder), field(p.Intent.Target), p.Kind)
	res, err := ss.Elicit(ctx, &mcp.ElicitParams{
		Mode:    "form",
		Message: msg,
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "title": "Apply this change", "default": false},
			},
			"required": []string{"confirm"},
		},
	})
	if err != nil {
		slog.Warn("apply_intent: elicitation failed", "err", err)
		return "", errors.New("approval could not be obtained; nothing was changed")
	}
	if res.Action != "accept" {
		return "", errors.New("the owner did not approve; nothing was changed")
	}
	if ok, _ := res.Content["confirm"].(bool); !ok {
		return "", errors.New("the owner did not approve; nothing was changed")
	}
	return history.ApprovedElicitation, nil
}

func addApplyIntent(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "apply_intent",
		Description: "Execute a previewed intent (from preview_intent, undo or reapply) after the owner approves it. " +
			"It acts only on messages that were in the preview AND still match, so mail that arrived since is never " +
			"touched, and it never deletes. Requires approved=true. Recorded in the history, from which it can be undone.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in applyIn) (*mcp.CallToolResult, applyOut, error) {
		if !in.Approved {
			return nil, applyOut{}, errors.New("approved must be true, after the owner has seen the preview")
		}
		p, err := d.org.Lookup(in.PreviewToken)
		if err != nil {
			return nil, applyOut{}, err
		}
		a, err := account(d.byName, p.Account)
		if err != nil {
			return nil, applyOut{}, err
		}
		release, err := d.org.Acquire(a.Name)
		if err != nil {
			return nil, applyOut{}, err
		}
		defer release()

		var ss *mcp.ServerSession
		if req != nil {
			ss = req.Session
		}
		approvedBy, err := approve(ctx, ss, p)
		if err != nil {
			return nil, applyOut{}, err
		}
		d.org.Consume(in.PreviewToken) // one token, one apply

		outcome, aerr := d.org.Apply(ctx, a, p)

		intent := p.Intent
		rec := history.Record{
			Account: a.Name, Kind: p.Kind, Intent: &intent, Action: intent.Action, Target: intent.Target,
			Preview: history.PreviewInfo{Matched: p.Matched, Sampled: len(p.Samples), ApprovedBy: approvedBy},
			Touched: outcome.Touched, Skipped: outcome.Skipped, Undoes: p.Undoes, Reapplies: p.Reapplies,
		}
		if aerr != nil {
			rec.Error = applyErrorText(aerr)
		}
		// The record is written before anything is returned, partial progress
		// included: it is the only way to undo what was done.
		saved, herr := d.hist.Append(rec)
		if herr != nil {
			slog.Error("apply_intent: history write failed", "account", a.Name, "touched", len(outcome.Touched), "err", herr)
		}
		if aerr != nil {
			slog.Warn("tool failed", "tool", "apply_intent", "account", a.Name, "err", aerr)
			return nil, applyOut{}, fmt.Errorf("%s; %d of %d messages were changed (history %s)",
				applyErrorText(aerr), len(outcome.Touched), p.Matched-outcome.Skipped, saved.ID)
		}
		if herr != nil {
			return nil, applyOut{}, fmt.Errorf("%d messages were changed but the history could not be written", len(outcome.Touched))
		}
		return nil, applyOut{
			Account: a.Name, Kind: p.Kind, Action: intent.Action,
			Source: field(intent.Criterion.Folder), Target: field(intent.Target),
			Matched: p.Matched, Done: len(outcome.Touched), Skipped: outcome.Skipped,
			ApprovedBy: approvedBy, HistoryID: saved.ID,
		}, nil
	})
}

// applyErrorText is the fixed text recorded and shown for a failed apply.
func applyErrorText(err error) string {
	var se organise.SafeError
	if errors.As(err, &se) {
		return se.Error()
	}
	return "the mail server refused or dropped the operation"
}

// --- list_history -----------------------------------------------------------

type listHistoryIn struct {
	Account string `json:"account,omitempty" jsonschema:"only this account; empty lists every account"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum records, default 50, at most 500"`
}

type historyUntrusted struct {
	Intent *organise.Intent `json:"intent,omitempty"`
	Target string           `json:"target,omitempty"`
}

type historyView struct {
	ID           string              `json:"id"`
	At           time.Time           `json:"at"`
	Account      string              `json:"account"`
	Kind         string              `json:"kind"`
	Action       string              `json:"action,omitempty"`
	Preview      history.PreviewInfo `json:"preview"`
	TouchedCount int                 `json:"touched_count"`
	Skipped      int                 `json:"skipped,omitempty"`
	Error        string              `json:"error,omitempty"`
	Undoes       string              `json:"undoes,omitempty"`
	Reapplies    string              `json:"reapplies,omitempty"`
	Untrusted    historyUntrusted    `json:"untrusted"`
}

type listHistoryOut struct {
	Notice  string        `json:"notice"`
	Records []historyView `json:"records"`
	Count   int           `json:"count"`
}

func cleanIntent(in *organise.Intent) *organise.Intent {
	if in == nil {
		return nil
	}
	c := *in
	c.Account, c.Target, c.Action = field(c.Account), field(c.Target), field(c.Action)
	c.Criterion.Folder, c.Criterion.From, c.Criterion.To = field(c.Criterion.Folder), field(c.Criterion.From), field(c.Criterion.To)
	c.Criterion.SubjectContains, c.Criterion.ListID = field(c.Criterion.SubjectContains), field(c.Criterion.ListID)
	c.Criterion.GitHubReason = field(c.Criterion.GitHubReason)
	return &c
}

// addListHistory is registered in both modes: reading what was done changes
// nothing.
func addListHistory(s *mcp.Server, byName map[string]accounts.Account, hist *history.Store) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_history",
		Description: "List what mail-mcp changed in the mailboxes, newest first: folders created, intents applied, " +
			"undone and reapplied, with the record id to pass to undo or reapply, how many messages each touched, " +
			"and how it was approved.",
		Annotations: readOnly(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listHistoryIn) (*mcp.CallToolResult, listHistoryOut, error) {
		if in.Account != "" {
			if _, err := account(byName, in.Account); err != nil {
				return nil, listHistoryOut{}, err
			}
		}
		limit := in.Limit
		switch {
		case limit <= 0:
			limit = 50
		case limit > 500:
			limit = 500
		}
		out := listHistoryOut{Notice: untrustedFieldsNotice, Records: []historyView{}}
		for _, r := range hist.List(in.Account, limit) {
			out.Records = append(out.Records, historyView{
				ID: r.ID, At: r.At, Account: r.Account, Kind: r.Kind, Action: r.Action, Preview: r.Preview,
				TouchedCount: len(r.Touched), Skipped: r.Skipped, Error: r.Error, Undoes: r.Undoes, Reapplies: r.Reapplies,
				Untrusted: historyUntrusted{Intent: cleanIntent(r.Intent), Target: field(r.Target)},
			})
		}
		out.Count = len(out.Records)
		return nil, out, nil
	})
}

// --- undo and reapply -------------------------------------------------------

type historyIDIn struct {
	HistoryID string `json:"history_id" jsonschema:"record id from list_history"`
}

func addUndo(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "undo",
		Description: "Preview reversing an applied move: the messages it moved go back to where they came from, found " +
			"by stable id so it works after UIDs changed. Returns a preview token; apply_intent executes it, with " +
			"the owner's approval. Undo of a label action is not supported yet.",
		// Only previews (and re-reads the target folder); apply_intent writes.
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in historyIDIn) (*mcp.CallToolResult, previewOut, error) {
		rec, ok := d.hist.Get(in.HistoryID)
		if !ok {
			return nil, previewOut{}, errors.New("no such history record")
		}
		if (rec.Kind != organise.KindApply && rec.Kind != organise.KindReapply) || rec.Intent == nil {
			return nil, previewOut{}, errors.New("only an applied intent can be undone")
		}
		if rec.Action != organise.ActionMove {
			return nil, previewOut{}, errors.New("undo of label not supported yet")
		}
		a, err := account(d.byName, rec.Account)
		if err != nil {
			return nil, previewOut{}, err
		}
		var ids []string
		from := ""
		seen := map[string]bool{}
		for _, t := range rec.Touched {
			if from != "" && t.FromFolder != from {
				return nil, previewOut{}, errors.New("record touched several source folders; cannot undo it as one step")
			}
			from = t.FromFolder
			if !seen[t.StableID] {
				seen[t.StableID] = true
				ids = append(ids, t.StableID)
			}
		}
		if len(ids) == 0 {
			return nil, previewOut{}, errors.New("that record changed nothing; there is nothing to undo")
		}
		// Refresh the folder the messages went to, so membership (and so the
		// UIDs to move) is what the server has now.
		if err := d.org.RefreshFolder(ctx, a, rec.Intent.Target); err != nil {
			return nil, previewOut{}, writeFail("undo", err, a.Name)
		}
		ms, err := d.store.MembersByID(ctx, a.Name, rec.Intent.Target, ids)
		if err != nil {
			return nil, previewOut{}, fail("undo", "preview failed", err, "account", a.Name)
		}
		var found []string
		got := map[string]bool{}
		for _, m := range ms {
			if !got[m.StableID] {
				got[m.StableID] = true
				found = append(found, m.StableID)
			}
		}
		if len(found) == 0 {
			return nil, previewOut{}, errors.New("none of those messages are in the target folder any more")
		}
		rev := organise.Intent{
			Account:   a.Name,
			Criterion: organise.Criterion{Folder: rec.Intent.Target},
			Target:    from,
			Action:    organise.ActionMove,
		}
		p, err := d.org.PreviewIDs(ctx, a, rev, found, len(ids)-len(found), rec.ID)
		if err != nil {
			return nil, previewOut{}, previewFail("undo", err, a.Name)
		}
		return nil, previewResult(p), nil
	})
}

func addReapply(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "reapply",
		Description: "Preview applying a saved intent again to mail that arrived since it was applied: same criterion, " +
			"source, target and action, restricted to messages received after the original apply. Returns a preview " +
			"token; apply_intent executes it, with the owner's approval.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in historyIDIn) (*mcp.CallToolResult, previewOut, error) {
		rec, ok := d.hist.Get(in.HistoryID)
		if !ok {
			return nil, previewOut{}, errors.New("no such history record")
		}
		if (rec.Kind != organise.KindApply && rec.Kind != organise.KindReapply) || rec.Intent == nil {
			return nil, previewOut{}, errors.New("only an applied intent can be reapplied")
		}
		a, err := account(d.byName, rec.Account)
		if err != nil {
			return nil, previewOut{}, err
		}
		p, err := d.org.PreviewIntent(ctx, a, *rec.Intent, organise.KindReapply, rec.At, rec.ID)
		if err != nil {
			return nil, previewOut{}, previewFail("reapply", err, a.Name)
		}
		return nil, previewResult(p), nil
	})
}
