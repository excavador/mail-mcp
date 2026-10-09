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
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
	"github.com/excavador/mail-mcp/internal/calendar"
	"github.com/excavador/mail-mcp/internal/history"
	"github.com/excavador/mail-mcp/internal/imapx"
	"github.com/excavador/mail-mcp/internal/organise"
)

// writeDeps is everything the write tools share.
type writeDeps struct {
	byName map[string]accounts.Account
	store  *cache.Cache
	hist   *history.Store
	org    *organise.Organiser
	// maxUnelicited is the most messages apply_intent will change when the
	// client cannot show the owner a confirmation of its own.
	maxUnelicited int
	// maxUnelicitedLabel is the same for action label (and the undo of one):
	// a label adds and never removes, so it may be larger.
	maxUnelicitedLabel int
	// approvalMode decides whether apply_intent may elicit at all.
	approvalMode ApprovalMode
	// cal: the Google Calendar clients (event tools exist only for accounts
	// whose calendar has write: true).
	cal calendar.Registry
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
	addSetSenderKind(s, d)
	addTagMessages(s, d)
	addUntagMessages(s, d)
	addSaveQuery(s, d)
	addDraftTools(s, d)
	addEventTools(s, d)
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
	Tag             string `json:"tag,omitempty" jsonschema:"a local tag (set with tag_messages): only messages in the folder that carry it; same syntax as tags"`
	Since           string `json:"since,omitempty" jsonschema:"earliest message date, RFC 3339 or YYYY-MM-DD"`
	Before          string `json:"before,omitempty" jsonschema:"messages dated before this, RFC 3339 or YYYY-MM-DD"`
}

type previewIn struct {
	Account   string      `json:"account" jsonschema:"account name"`
	Criterion criterionIn `json:"criterion" jsonschema:"which messages; at least one of from, list_id, github_reason, subject_contains, to, tag"`
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
	MoveBack     int              `json:"move_back,omitempty" jsonschema:"undo: messages moved back to their source"`
	Unlabel      int              `json:"unlabel,omitempty" jsonschema:"undo of a label: messages the label is removed from (only those the apply labelled)"`
	CopyBack     int              `json:"copy_back,omitempty" jsonschema:"undo: messages copied back to their source because they were already in the target before the apply, so they keep it"`
	NotFound     int              `json:"not_found,omitempty" jsonschema:"undo: recorded messages no longer in the folder"`
	Sampled      int              `json:"sampled"`
	Untrusted    previewUntrusted `json:"untrusted"`
}

func previewResult(p *organise.Preview) previewOut {
	out := previewOut{
		Notice:       untrustedFieldsNotice + " Nothing has changed yet: apply_intent with this token, once the owner approves, executes it.",
		PreviewToken: p.Token, ExpiresAt: p.Expires, Account: p.Account, Kind: p.Kind,
		Action: p.Intent.Action, Source: field(p.Intent.Criterion.Folder), Target: field(p.Intent.Target),
		Matched: p.Matched, NotFound: p.Missing, CopyBack: len(p.CopyBack), Sampled: len(p.Samples),
		Untrusted: previewUntrusted{Samples: []sampleOut{}},
	}
	if p.Kind == organise.KindUndo {
		if p.Intent.Action == organise.ActionUnlabel {
			out.Unlabel = len(p.IDs)
		} else {
			out.MoveBack = len(p.IDs)
		}
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
	tag := c.Tag
	if tag != "" {
		if tag, err = cache.NormalizeTag(tag); err != nil {
			return organise.Intent{}, err
		}
	}
	return organise.Intent{
		Account: account, Target: target, Action: action,
		Criterion: organise.Criterion{
			Folder: c.Folder, From: c.From, To: c.To, SubjectContains: c.SubjectContains, ListID: c.ListID,
			GitHubReason: c.GitHubReason, Tag: tag, Since: since, Before: before,
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
			"Folders/... target, label a Labels/... target. criterion.tag selects the messages of the folder that " +
			"carry a local tag (set by tag_messages), ANDed with the other criteria; it needs the folder like the rest. " +
			"A label adds only and is undoable (undo removes the label from just the messages that apply labelled, " +
			"never from one that had it before), so a client without elicitation may apply it to more messages " +
			"than a move.",
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
	// The echo fields restate the preview. They exist so that the client's
	// approval prompt, which shows a tool call's arguments, shows the owner
	// what is about to happen and not just an opaque token.
	ExpectAccount string `json:"expect_account" jsonschema:"the account, as the preview shows it"`
	ExpectAction  string `json:"expect_action" jsonschema:"move, label or unlabel (undo of a label), as the preview shows it"`
	ExpectSource  string `json:"expect_source" jsonschema:"the source folder, as the preview shows it"`
	ExpectTarget  string `json:"expect_target" jsonschema:"the target folder, as the preview shows it"`
	ExpectMatched int    `json:"expect_matched" jsonschema:"the matched count, as the preview shows it"`
}

// checkEcho refuses an apply whose echo fields do not restate the preview.
func checkEcho(in applyIn, p *organise.Preview) error {
	switch {
	case in.ExpectAccount != p.Account:
		return organise.SafeError("expect_account does not match the preview")
	case in.ExpectAction != p.Intent.Action:
		return organise.SafeError("expect_action does not match the preview")
	case in.ExpectSource != p.Intent.Criterion.Folder:
		return organise.SafeError("expect_source does not match the preview")
	case in.ExpectTarget != p.Intent.Target:
		return organise.SafeError("expect_target does not match the preview")
	case in.ExpectMatched != p.Matched:
		return organise.SafeError("expect_matched does not match the preview")
	}
	return nil
}

type applyOut struct {
	Account      string `json:"account"`
	Kind         string `json:"kind"`
	Action       string `json:"action"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	Matched      int    `json:"matched" jsonschema:"messages in the approved preview"`
	Done         int    `json:"done" jsonschema:"messages acted on"`
	CopiedBack   int    `json:"copied_back,omitempty" jsonschema:"undo: messages copied back to their source (already in the target before the apply); done counts the messages moved back"`
	Skipped      int    `json:"skipped" jsonschema:"approved messages no longer matching, or gone"`
	NotPreviewed int    `json:"not_previewed" jsonschema:"messages matching now that were not in the preview (mail that arrived since); left alone"`
	ApprovedBy   string `json:"approved_by"`
	HistoryID    string `json:"history_id"`
}

// protocol20260728 is the first MCP protocol version in which a server may not
// send elicitation/create in the middle of a tool call.
const protocol20260728 = "2026-07-28"

// clientElicits reports whether the client declared the elicitation capability.
// CallToolRequest.ClientCapabilities (go-sdk v1.7.0 mcp/shared.go:684) answers
// for both protocols: from the per-request _meta on >= 2026-07-28, where there
// is no initialize handshake to read it from, and from the session's
// InitializeParams (mcp/server.go:1959) before that.
func clientElicits(req *mcp.CallToolRequest) bool {
	if req == nil {
		return false
	}
	caps := req.ClientCapabilities()
	return caps != nil && caps.Elicitation != nil
}

// approve obtains the owner's approval of p and says through which channel.
//
// Where the client supports elicitation the server asks the owner itself: a
// form naming the account, count, source, target and action, and nothing
// proceeds without an explicit accept. That approval cannot be given by the
// model, whatever it was told to do.
//
// How the question travels depends on the protocol the client negotiated:
//
//   - Before 2026-07-28 the handler calls ServerSession.Elicit mid-call
//     (mcp/server.go:1654).
//   - From 2026-07-28 the SDK refuses that (mcp/server.go:1544-1551: "cannot be
//     sent while serving a request on protocol version ...", SEP-2322). The
//     handler instead returns a CallToolResult carrying InputRequests
//     (mcp/protocol.go:311); the SDK marks it input_required (mcp/mrtr.go:57)
//     and the client retries the same call with the answer in
//     CallToolParamsRaw.InputResponses (mcp/protocol.go:255), echoing
//     RequestState (mcp/protocol.go:316). So on that
//     protocol approve returns pending the first time, and the handler runs
//     again, every check included, when the answer comes back. The token is
//     consumed only after the answer is accepted, so a retry is the same apply.
//
// Where the client cannot elicit, the only gate is the client's own
// tool-approval prompt: apply_intent is not read-only, so a client that asks
// before running such tools asks here, and the echo fields make that prompt
// readable. That is weaker (the model fills in "approved"), so it is capped at
// maxUnelicited messages and recorded as "client-tool-approval".
func approve(ctx context.Context, req *mcp.CallToolRequest, d writeDeps, p *organise.Preview) (by string, pending *mcp.CallToolResult, err error) {
	return approveSpec(ctx, req, d, approvalSpec{
		token: p.Token, key: d.org.QuestionKey(p), message: elicitMessage(d, p), title: "Apply this change",
		unelicited: func() error {
			if limit := d.unelicitedLimit(p.Intent.Action, d.byName[p.Account].Provider); p.Matched > limit {
				return organise.SafeError(fmt.Sprintf(
					"more than %d messages cannot be applied on the client's tool approval alone (%s)", limit, p.Intent.Action))
			}
			return nil
		},
	})
}

// approvalSpec is what approveSpec needs to know about the thing being approved:
// its token, the key and text of the question, and the check that applies when
// the client's own tool approval is the only gate.
type approvalSpec struct {
	token, key, message, title string
	unelicited                 func() error
}

func approveSpec(ctx context.Context, req *mcp.CallToolRequest, d writeDeps, sp approvalSpec) (by string, pending *mcp.CallToolResult, err error) {
	if d.approvalMode != ApprovalElicitation || !clientElicits(req) {
		if err := sp.unelicited(); err != nil {
			return "", nil, err
		}
		return history.ApprovedClientTool, nil, nil
	}
	params := &mcp.ElicitParams{
		Mode:    "form",
		Message: sp.message,
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "title": sp.title, "default": false},
			},
			"required": []string{"confirm"},
		},
	}
	var res *mcp.ElicitResult
	if req.ProtocolVersion() >= protocol20260728 {
		// The question is bound to this preview twice over: it is keyed by a
		// value derived from the token, and RequestState carries an HMAC over
		// the token and a single-use nonce. An answer is accepted only under
		// the key, with a state that verifies for this token and has not been
		// used. Anything else (an answer given for another token, a forged or
		// replayed one, none) is not an answer: a new question is asked, and
		// nothing is applied on it.
		key := sp.key
		got, answered := req.Params.InputResponses[key]
		if !answered || !d.org.TakeQuestion(sp.token, req.Params.RequestState) {
			state, err := d.org.NewQuestion(sp.token)
			if err != nil {
				return "", nil, organise.ErrExpired
			}
			return "", &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{key: params}, RequestState: state}, nil
		}
		er, ok := got.(*mcp.ElicitResult)
		if !ok || er == nil {
			return "", nil, errors.New("approval could not be obtained; nothing was changed")
		}
		res = er
	} else {
		ss := req.Session
		res, err = ss.Elicit(ctx, params)
		if err != nil {
			slog.Warn("apply_intent: elicitation failed", "err", err)
			return "", nil, errors.New("approval could not be obtained; nothing was changed")
		}
	}
	if res.Action != "accept" {
		return "", nil, errors.New("the owner did not approve; nothing was changed")
	}
	if ok, _ := res.Content["confirm"].(bool); !ok {
		return "", nil, errors.New("the owner did not approve; nothing was changed")
	}
	return history.ApprovedElicitation, nil, nil
}

// newTargetWindow is how long a folder made by create_folder still counts as new.
const newTargetWindow = 24 * time.Hour

// warnings are the things the owner should weigh first: a move that takes mail
// out of the inbox, and a target nothing has been put in before (a fresh,
// possibly hidden, destination is how mail gets buried).
func warnings(d writeDeps, p *organise.Preview) []string {
	var w []string
	if p.Intent.Action == organise.ActionMove && strings.EqualFold(p.Intent.Criterion.Folder, "INBOX") {
		w = append(w, "moves mail OUT OF INBOX")
	}
	applied, created := d.hist.TargetHistory(p.Account, p.Intent.Target)
	if p.Intent.Action == organise.ActionUnlabel {
		return w // nothing is put in the target: it is where the messages stay
	}
	if !applied || (!created.IsZero() && time.Since(created) < newTargetWindow) {
		w = append(w, "target folder is new")
	}
	return w
}

// elicitMessage is what the owner reads: warnings first, then what, where, how
// many, by which criterion, and a few of the messages. Every third-party or
// caller-supplied string is sanitised and capped.
func elicitMessage(d writeDeps, p *organise.Preview) string {
	var b strings.Builder
	for _, w := range warnings(d, p) {
		fmt.Fprintf(&b, "WARNING: %s\n", w)
	}
	in := p.Intent
	fmt.Fprintf(&b, "Apply %s of %d message(s) in account %s: %s -> %s (%s; undo is available from the history).\n",
		field(in.Action), p.Matched, field(p.Account), field(in.Criterion.Folder), field(in.Target), field(p.Kind))
	c := in.Criterion
	for _, kv := range [][2]string{
		{"from", c.From}, {"to", c.To}, {"subject contains", c.SubjectContains},
		{"list-id", c.ListID}, {"github reason", c.GitHubReason}, {"tag", c.Tag},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "Criterion %s: %s\n", kv[0], field(kv[1]))
		}
	}
	if !c.Since.IsZero() {
		fmt.Fprintf(&b, "Criterion since: %s\n", c.Since.UTC().Format(time.RFC3339))
	}
	if !c.Before.IsZero() {
		fmt.Fprintf(&b, "Criterion before: %s\n", c.Before.UTC().Format(time.RFC3339))
	}
	for i, h := range p.Samples {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "Sample: %s | %s\n", field(h.From), field(h.Subject))
	}
	return b.String()
}

// unelicitedLimit is how many messages an apply of action may change on the
// client's own tool approval: label (add only) and its undo get the larger cap.
func (d writeDeps) unelicitedLimit(action string, p accounts.Provider) int {
	if action == organise.ActionUnlabel && p == accounts.Proton {
		// Proton removes a label by expunge, not yet verified by hand against
		// Bridge (TODO in organise.unlabel): keep the move cap.
		return d.maxUnelicited
	}
	if action == organise.ActionLabel || action == organise.ActionUnlabel {
		return d.maxUnelicitedLabel
	}
	return d.maxUnelicited
}

func applyDescription(d writeDeps) string {
	desc := "Execute a previewed intent (from preview_intent, undo or reapply) after the owner approves it. " +
		"It acts only on messages that were in the preview AND still match, so mail that arrived since is never " +
		"touched, and it never deletes. Requires approved=true. Recorded in the history, from which it can be undone."
	if d.approvalMode != ApprovalElicitation {
		desc += fmt.Sprintf(" Approval is the client's own tool-approval prompt, which must show the account, action, "+
			"source, target and count (restate them in the expect_* fields); above %d messages the apply is refused "+
			"(for label, and the undo of a Gmail label: above %d).", d.maxUnelicited, d.maxUnelicitedLabel)
	}
	return desc
}

func addApplyIntent(s *mcp.Server, d writeDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "apply_intent",
		Description: applyDescription(d),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in applyIn) (*mcp.CallToolResult, applyOut, error) {
		if !in.Approved {
			return nil, applyOut{}, errors.New("approved must be true, after the owner has seen the preview")
		}
		// Order matters. The slot comes first, so two applies of one token
		// cannot both get past the checks; the token is looked up again after
		// approval (the owner may have taken minutes) and consumed before
		// anything is changed, so it approves exactly one apply.
		first, err := d.org.Lookup(in.PreviewToken)
		if err != nil {
			return nil, applyOut{}, err
		}
		a, err := account(d.byName, first.Account)
		if err != nil {
			return nil, applyOut{}, err
		}
		release, err := d.org.Acquire(a.Name)
		if err != nil {
			return nil, applyOut{}, err
		}
		defer release()
		p, err := d.org.Lookup(in.PreviewToken)
		if err != nil {
			return nil, applyOut{}, err
		}
		if err := checkEcho(in, p); err != nil {
			return nil, applyOut{}, err
		}

		approvedBy, pending, err := approve(ctx, req, d, p)
		if err != nil {
			return nil, applyOut{}, err
		}
		if pending != nil {
			return pending, applyOut{}, nil // input_required: the client retries with the answer
		}
		if !d.org.Consume(in.PreviewToken) { // expired while waiting, or already used
			return nil, applyOut{}, organise.ErrExpired
		}

		outcome, aerr := d.org.Apply(ctx, a, p)

		intent := p.Intent
		rec := history.Record{
			Account: a.Name, Kind: p.Kind, Intent: &intent, Action: intent.Action, Target: intent.Target,
			Preview: history.PreviewInfo{Matched: p.Matched, Sampled: len(p.Samples), ApprovedBy: approvedBy},
			Touched: history.Group(outcome.Touched), AlreadyInTarget: history.Group(outcome.AlreadyInTarget),
			CopiedBack: history.Group(outcome.CopiedBack),
			Skipped:    outcome.Skipped, NotPreviewed: outcome.NotPreviewed, Undoes: p.Undoes, Reapplies: p.Reapplies,
		}
		if aerr != nil {
			rec.Error = applyErrorText(aerr)
		}
		// The record is written before anything is returned, partial progress
		// included: it is the only way to undo what was done.
		parts, herr := d.hist.AppendGroup(rec)
		saved := history.Record{}
		if len(parts) > 0 {
			saved = parts[0]
		}
		if herr != nil {
			slog.Error("apply_intent: history write failed", "account", a.Name, "touched", len(outcome.Touched), "err", herr)
		}
		if aerr != nil {
			slog.Warn("tool failed", "tool", "apply_intent", "account", a.Name, "err", aerr)
			return nil, applyOut{}, fmt.Errorf("%s; %d of %d messages were changed (history %s)",
				applyErrorText(aerr), distinct(outcome.Touched), p.Matched-outcome.Skipped, saved.ID)
		}
		if herr != nil {
			return nil, applyOut{}, fmt.Errorf("%d messages were changed but the history could not be written", distinct(outcome.Touched))
		}
		return nil, applyOut{
			Account: a.Name, Kind: p.Kind, Action: intent.Action,
			Source: field(intent.Criterion.Folder), Target: field(intent.Target),
			Matched: p.Matched, Done: distinct(outcome.Touched), CopiedBack: distinct(outcome.CopiedBack), Skipped: outcome.Skipped, NotPreviewed: outcome.NotPreviewed,
			ApprovedBy: approvedBy, HistoryID: saved.ID,
		}, nil
	})
}

// applyErrorText is the fixed text recorded and shown for a failed apply.
func applyErrorText(err error) string {
	if errors.Is(err, imapx.ErrTimeout) {
		return "mail server did not answer in time"
	}
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
	Intent *organise.Intent   `json:"intent,omitempty"`
	Target string             `json:"target,omitempty"`
	Draft  *history.DraftInfo `json:"draft,omitempty"`
}

// cleanDraft sanitises a recorded draft for display: its recipients and subject
// were partly taken from third-party mail.
func cleanDraft(in *history.DraftInfo) *history.DraftInfo {
	if in == nil {
		return nil
	}
	c := *in
	c.Folder, c.MessageID, c.From, c.Subject, c.ReplyToID = field(c.Folder), field(c.MessageID), field(c.From), field(c.Subject), field(c.ReplyToID)
	c.To, c.Cc, c.Bcc = fieldAll(c.To), fieldAll(c.Cc), fieldAll(c.Bcc)
	return &c
}

type historyView struct {
	ID              string              `json:"id"`
	At              time.Time           `json:"at"`
	Account         string              `json:"account"`
	Kind            string              `json:"kind"`
	Action          string              `json:"action,omitempty"`
	Preview         history.PreviewInfo `json:"preview"`
	OldKind         string              `json:"old_kind,omitempty"`
	NewKind         string              `json:"new_kind,omitempty"`
	TouchedCount    int                 `json:"touched_count"`
	AlreadyInTarget int                 `json:"already_in_target,omitempty"`
	CopiedBack      int                 `json:"copied_back,omitempty"`
	Skipped         int                 `json:"skipped,omitempty"`
	NotPreviewed    int                 `json:"not_previewed,omitempty"`
	Error           string              `json:"error,omitempty"`
	Undoes          string              `json:"undoes,omitempty"`
	Reapplies       string              `json:"reapplies,omitempty"`
	Untrusted       historyUntrusted    `json:"untrusted"`
}

type listHistoryOut struct {
	Notice  string        `json:"notice"`
	Records []historyView `json:"records"`
	Count   int           `json:"count"`
}

// countIDs counts distinct stable ids: a message in two folders, or with two
// UIDs, is one message.
func countIDs(m map[string][]string) int {
	seen := map[string]bool{}
	for _, ids := range m {
		for _, id := range ids {
			seen[id] = true
		}
	}
	return len(seen)
}

// distinct counts the distinct stable ids in an outcome's list.
func distinct(ts []organise.Touched) int {
	seen := map[string]bool{}
	for _, t := range ts {
		seen[t.StableID] = true
	}
	return len(seen)
}

func touchedCount(r history.Record) int { return countIDs(r.Touched) }

func cleanIntent(in *organise.Intent) *organise.Intent {
	if in == nil {
		return nil
	}
	c := *in
	c.Account, c.Target, c.Action = field(c.Account), field(c.Target), field(c.Action)
	c.Criterion.Folder, c.Criterion.From, c.Criterion.To = field(c.Criterion.Folder), field(c.Criterion.From), field(c.Criterion.To)
	c.Criterion.SubjectContains, c.Criterion.ListID = field(c.Criterion.SubjectContains), field(c.Criterion.ListID)
	c.Criterion.GitHubReason, c.Criterion.Tag = field(c.Criterion.GitHubReason), field(c.Criterion.Tag)
	return &c
}

// addListHistory is registered in both modes: reading what was done changes
// nothing.
func addListHistory(s *mcp.Server, byName map[string]accounts.Account, hist *history.Store) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_history",
		Description: "List what mail-mcp changed in the mailboxes, newest first: folders created, drafts saved, intents applied, " +
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
		for _, r := range hist.Grouped(in.Account, limit) {
			out.Records = append(out.Records, historyView{
				ID: field(r.ID), At: r.At, Account: field(r.Account), Kind: field(r.Kind), Action: field(r.Action),
				Preview: history.PreviewInfo{
					Matched: r.Preview.Matched, Sampled: r.Preview.Sampled, ApprovedBy: field(r.Preview.ApprovedBy),
				},
				OldKind: field(r.OldKind), NewKind: field(r.NewKind),
				TouchedCount: touchedCount(r), AlreadyInTarget: countIDs(r.AlreadyInTarget), CopiedBack: countIDs(r.CopiedBack), Skipped: r.Skipped, NotPreviewed: r.NotPreviewed,
				Error: field(r.Error), Undoes: field(r.Undoes), Reapplies: field(r.Reapplies),
				Untrusted: historyUntrusted{Intent: cleanIntent(r.Intent), Target: field(r.Target), Draft: cleanDraft(r.Draft)},
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
		Description: "Preview reversing an applied move or label. Move: the messages it moved go back to where they " +
			"came from, found by stable id so it works after UIDs changed. Label: the label is removed from exactly " +
			"the messages that intent labelled (Gmail: STORE -X-GM-LABELS; Proton: the copy in the Labels/... folder " +
			"is expunged by UID) and never from a message that already had it before the intent; the preview's " +
			"action is unlabel. Returns a preview token; apply_intent executes it, with " +
			"the owner's approval. Local changes are different: " +
			"undo of tag_messages, untag_messages or set_sender_kind takes effect at once (no preview token, no mailbox " +
			"access) and the result says what was restored.",
		// Only previews (and re-reads the target folder) for an intent;
		// apply_intent writes. A local kind is reversed here, in the cache only.
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in historyIDIn) (*mcp.CallToolResult, any, error) {
		rec, ok := d.hist.Get(in.HistoryID)
		if !ok {
			return nil, nil, errors.New("no such history record")
		}
		switch rec.Kind {
		case history.KindCreateDraft:
			return nil, nil, errors.New("a draft cannot be undone from here (mail-mcp deletes nothing): discard it in your mail client")
		case history.KindCreateEvent:
			return nil, nil, errors.New("an event cannot be undone from here (there is no calendar delete tool, and the invitations are already sent): cancel it in the calendar")
		case history.KindSetSenderKind, history.KindTagMessages, history.KindUntagMessages:
			out, err := undoLocal(ctx, d, rec)
			return nil, out, err
		}
		p, err := undoIntent(ctx, d, rec)
		if err != nil {
			return nil, nil, err
		}
		return nil, p, nil
	})
}

// undoIntent previews reversing an applied move.
func undoIntent(ctx context.Context, d writeDeps, rec history.Record) (previewOut, error) {
	{
		if (rec.Kind != organise.KindApply && rec.Kind != organise.KindReapply) || rec.Intent == nil {
			return previewOut{}, errors.New("only an applied intent can be undone")
		}
		if rec.Action != organise.ActionMove && rec.Action != organise.ActionLabel {
			return previewOut{}, errors.New("only a move or a label can be undone")
		}
		a, err := account(d.byName, rec.Account)
		if err != nil {
			return previewOut{}, err
		}
		if d.hist.Undone(rec.ID) {
			return previewOut{}, errors.New("that record was already undone")
		}
		if rec.Action == organise.ActionLabel {
			return undoLabel(ctx, d, a, rec)
		}
		folders := map[string]bool{}
		for f := range rec.Touched {
			folders[f] = true
		}
		for f := range rec.AlreadyInTarget {
			folders[f] = true
		}
		if len(folders) > 1 {
			return previewOut{}, errors.New("record touched several source folders; cannot undo it as one step")
		}
		from := ""
		for f := range folders {
			from = f
		}
		uniq := func(m map[string][]string) []string {
			var out []string
			seen := map[string]bool{}
			for _, ids := range m {
				for _, id := range ids {
					if !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
				}
			}
			return out
		}
		ids, copyIDs := uniq(rec.Touched), uniq(rec.AlreadyInTarget)
		if len(ids)+len(copyIDs) == 0 {
			return previewOut{}, errors.New("that record changed nothing; there is nothing to undo")
		}
		// Refresh the folder the messages went to, so membership (and so the
		// UIDs to move) is what the server has now.
		if err := d.org.RefreshFolder(ctx, a, rec.Intent.Target); err != nil {
			return previewOut{}, writeFail("undo", err, a.Name)
		}
		present := func(want []string) ([]string, error) {
			ms, err := d.store.MembersByID(ctx, a.Name, rec.Intent.Target, want)
			if err != nil {
				return nil, err
			}
			var found []string
			got := map[string]bool{}
			for _, m := range ms {
				if !got[m.StableID] {
					got[m.StableID] = true
					found = append(found, m.StableID)
				}
			}
			return found, nil
		}
		found, err := present(ids)
		if err != nil {
			return previewOut{}, fail("undo", "preview failed", err, "account", a.Name)
		}
		copyFound, err := present(copyIDs)
		if err != nil {
			return previewOut{}, fail("undo", "preview failed", err, "account", a.Name)
		}
		if len(found)+len(copyFound) == 0 {
			return previewOut{}, errors.New("none of those messages are in the target folder any more")
		}
		rev := organise.Intent{
			Account:   a.Name,
			Criterion: organise.Criterion{Folder: rec.Intent.Target},
			Target:    from,
			Action:    organise.ActionMove,
		}
		p, err := d.org.PreviewIDs(ctx, a, rev, found, copyFound, len(ids)+len(copyIDs)-len(found)-len(copyFound), rec.ID)
		if err != nil {
			return previewOut{}, previewFail("undo", err, a.Name)
		}
		return previewResult(p), nil
	}
}

// undoLabel previews removing the label an applied label intent added. The
// record's Touched is exactly the messages that intent labelled; the ones that
// already carried the label (AlreadyInTarget, found against the server at apply
// time) are not in it, so they keep it. Messages are found again by stable id
// (Gmail: X-GM-MSGID) in the label folder after a refresh, so it works after
// UIDs changed.
func undoLabel(ctx context.Context, d writeDeps, a accounts.Account, rec history.Record) (previewOut, error) {
	var ids []string
	seen := map[string]bool{}
	for _, list := range rec.Touched {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return previewOut{}, errors.New("that record labelled nothing; there is nothing to undo")
	}
	label := rec.Intent.Target
	if err := d.org.RefreshFolder(ctx, a, label); err != nil {
		return previewOut{}, writeFail("undo", err, a.Name)
	}
	ms, err := d.store.MembersByID(ctx, a.Name, label, ids)
	if err != nil {
		return previewOut{}, fail("undo", "preview failed", err, "account", a.Name)
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
		return previewOut{}, errors.New("none of those messages carry the label any more")
	}
	rev := organise.Intent{
		Account:   a.Name,
		Criterion: organise.Criterion{Folder: label},
		Target:    rec.Intent.Criterion.Folder,
		Action:    organise.ActionUnlabel,
	}
	p, err := d.org.PreviewIDs(ctx, a, rev, found, nil, len(ids)-len(found), rec.ID)
	if err != nil {
		return previewOut{}, previewFail("undo", err, a.Name)
	}
	return previewResult(p), nil
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
