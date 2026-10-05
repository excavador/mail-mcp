package organise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

const (
	// chunkUIDs UIDs per MOVE or COPY: one command, bounded size.
	chunkUIDs = 500
)

// Budgets of the IMAP sessions, vars so tests can shorten them. Each is
// enforced by imapx.Do and sized for Gmail answering in ~9s per command, under
// the ~100s cut of the Cloudflare in front.
var (
	// applyBudget bounds the IMAP part of one apply (dial, moves). An apply
	// that runs out keeps what it moved, records it in the history and says so.
	applyBudget = 75 * time.Second
	// createBudget bounds create_folder.
	createBudget = 45 * time.Second
	// undoRefreshBudget bounds the refresh undo does before it looks.
	undoRefreshBudget = 45 * time.Second
	// afterApplyRefreshBudget bounds the re-read after an apply; a folder it
	// cannot refresh in time stays stale until the next scheduled refresh.
	afterApplyRefreshBudget = 15 * time.Second
)

// Budgets are the IMAP time budgets of this package.
type Budgets struct{ Apply, Create, UndoRefresh, AfterApplyRefresh time.Duration }

// SetBudgets replaces the budgets for tests that need a stalled server to
// time out quickly; a zero field is left as it is. It returns a func that
// puts the previous values back. For tests only: it changes package state
// and is not safe to call while sessions are running.
func SetBudgets(b Budgets) (restore func()) {
	prev := Budgets{applyBudget, createBudget, undoRefreshBudget, afterApplyRefreshBudget}
	set := func(dst *time.Duration, v time.Duration) {
		if v > 0 {
			*dst = v
		}
	}
	set(&applyBudget, b.Apply)
	set(&createBudget, b.Create)
	set(&undoRefreshBudget, b.UndoRefresh)
	set(&afterApplyRefreshBudget, b.AfterApplyRefresh)
	return func() {
		applyBudget, createBudget, undoRefreshBudget, afterApplyRefreshBudget = prev.Apply, prev.Create, prev.UndoRefresh, prev.AfterApplyRefresh
	}
}

// Touched is one message an apply acted on, by stable id so that it can be
// found again after UIDs change, and the folder it came from.
type Touched struct {
	StableID   string `json:"stable_id"`
	FromFolder string `json:"from_folder"`
}

// Outcome is what an apply did. It is meaningful even when Apply returns an
// error: Touched is the progress made before the failure.
type Outcome struct {
	Matched int // size of the approved set
	Touched []Touched
	// Skipped is approved ids not acted on: no longer matching, or gone.
	Skipped int
	// NotPreviewed counts current matches that were not in the previewed set
	// (mail that arrived since). They were left alone.
	NotPreviewed int
	// AlreadyInTarget lists acted-on ids the cache showed in the target folder
	// before the apply, with the folder each came from. They are not in
	// Touched: undo restores their source by COPY, never by moving them.
	AlreadyInTarget []Touched
	// CopiedBack (undo only) is the already-in-target ids of the undone apply,
	// restored to their source by COPY.
	CopiedBack []Touched
}

// blockedAttrs are the special-use attributes a target may not carry.
var blockedAttrs = []imap.MailboxAttr{
	imap.MailboxAttrTrash, imap.MailboxAttrJunk, imap.MailboxAttrDrafts,
	imap.MailboxAttrSent, imap.MailboxAttrAll, imap.MailboxAttrFlagged,
}

func isSpecialUse(attrs []imap.MailboxAttr) bool {
	for _, b := range blockedAttrs {
		if hasAttr(attrs, b) {
			return true
		}
	}
	return false
}

// returnedUIDs reads the source UIDs a COPYUID response named. ok is false
// when the server returned none (no UIDPLUS), in which case the caller cannot
// tell which UIDs were acted on.
func returnedUIDs(src imap.NumSet) (map[imap.UID]bool, bool) {
	set, isUID := src.(imap.UIDSet)
	if !isUID {
		return nil, false
	}
	nums, ok := set.Nums()
	if !ok || len(nums) == 0 {
		return nil, false
	}
	m := make(map[imap.UID]bool, len(nums))
	for _, u := range nums {
		m[u] = true
	}
	return m, true
}

func hasAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

// folderInfo looks one folder up by exact name (the name has no wildcards:
// checkName refused them). ok is false when the server has no such folder.
func folderInfo(c *imapclient.Client, name string) (attrs []imap.MailboxAttr, ok bool, err error) {
	list, err := c.List("", name, nil).Collect()
	if err != nil {
		return nil, false, err
	}
	for _, m := range list {
		if m.Mailbox == name && !hasAttr(m.Attrs, imap.MailboxAttrNonExistent) {
			return m.Attrs, true, nil
		}
	}
	return nil, false, nil
}

// CreateFolder sends CREATE for name, which ValidateNewFolder has accepted.
// It is idempotent: a folder that already exists is success.
func (o *Organiser) CreateFolder(ctx context.Context, a accounts.Account, name string) error {
	return imapx.Do(ctx, a, "create_folder", createBudget, func(ctx context.Context, c *imapclient.Client) error {
		imapx.SetPhase(ctx, "list")
		if attrs, ok, err := folderInfo(c, name); err != nil {
			return err
		} else if ok {
			if isSpecialUse(attrs) {
				return ErrSpecialUse
			}
			return o.store.NoteFolder(ctx, a.Name, name)
		}
		imapx.SetPhase(ctx, "create")
		if err := c.Create(name, nil).Wait(); err != nil {
			// Lost a race with another client: fine if it is there now.
			if _, ok, lerr := folderInfo(c, name); lerr == nil && ok {
				return o.store.NoteFolder(ctx, a.Name, name)
			}
			return err
		}
		// Make previews see the folder now, without waiting for a refresh.
		if err := o.store.NoteFolder(ctx, a.Name, name); err != nil {
			return err
		}
		return nil
	})
}

// RefreshFolder refreshes one folder of the cache (undo does this to the
// folder it is about to read memberships from).
func (o *Organiser) RefreshFolder(ctx context.Context, a accounts.Account, folder string) error {
	return imapx.Do(ctx, a, "refresh_folder", undoRefreshBudget, func(ctx context.Context, c *imapclient.Client) error {
		imapx.SetPhase(ctx, "refresh")
		_, err := o.store.RefreshFolders(ctx, c, a, []string{folder})
		return err
	})
}

// Apply executes an approved preview. The approval itself is the caller's
// business; Apply's job is to do no more than the preview said.
//
// It re-resolves the criterion now and acts only on the intersection with the
// previewed set, so mail that arrived after the preview is never touched. The
// caller holds the account's write slot.
func (o *Organiser) Apply(ctx context.Context, a accounts.Account, p *Preview) (out Outcome, err error) {
	out.Matched = len(p.IDs) + len(p.CopyBack)
	in := p.Intent
	if err := in.CheckMove(a.Provider); err != nil {
		return out, err
	}

	now, err := o.resolve(ctx, p)
	if err != nil {
		return out, err
	}
	approved := make(map[string]bool, len(p.IDs))
	for _, id := range p.IDs {
		approved[id] = true
	}
	var members []uidMember
	acting := map[string]bool{}
	notPreviewed := map[string]bool{}
	var validity uint32
	for _, m := range now {
		if !approved[m.StableID] {
			notPreviewed[m.StableID] = true
			continue // arrived or started matching after the preview
		}
		acting[m.StableID] = true
		members = append(members, uidMember{id: m.StableID, uid: imap.UID(m.UID)})
		validity = m.UIDValidity
	}
	out.Skipped = len(p.IDs) - len(acting)
	out.NotPreviewed = len(notPreviewed)
	// Undo of a move: the ids that were already in the target are COPYed back.
	var copyBack []uidMember
	if len(p.CopyBack) > 0 {
		want := map[string]bool{}
		for _, id := range p.CopyBack {
			want[id] = true
		}
		cms, err := o.store.MembersByID(ctx, p.Account, in.Criterion.Folder, p.CopyBack)
		if err != nil {
			return out, err
		}
		got := map[string]bool{}
		for _, m := range cms {
			if want[m.StableID] {
				got[m.StableID] = true
				copyBack = append(copyBack, uidMember{id: m.StableID, uid: imap.UID(m.UID)})
				validity = m.UIDValidity
			}
		}
		out.Skipped += len(p.CopyBack) - len(got)
	}
	if len(members) == 0 && len(copyBack) == 0 {
		return out, nil
	}
	// Messages already in the target stay out of Touched: undo would otherwise
	// pull them out of a place they were in before this apply.
	var actIDs []string
	for id := range acting {
		actIDs = append(actIDs, id)
	}
	inTarget, err := o.store.MembersByID(ctx, p.Account, in.Target, actIDs)
	if err != nil {
		return out, err
	}
	already := map[string]bool{}
	for _, m := range inTarget {
		already[m.StableID] = true
	}

	// The IMAP part runs under imapx.Do: a server that stops answering costs
	// applyBudget, not the request. What the moves reported is kept under mu,
	// because on a timeout Do returns while the session may still be winding
	// down; the snapshot taken after it is what the history records.
	var (
		mu                              sync.Mutex
		touched, alreadyDone, copiedOut []Touched
		sessionStarted                  bool // past the checks: a refresh is due
	)
	actErr := imapx.Do(ctx, a, "apply_intent", applyBudget, func(ctx context.Context, c *imapclient.Client) error {
		imapx.SetPhase(ctx, "checks")
		if in.Action == ActionMove && !c.Caps().Has(imap.CapMove) {
			// go-imap would fall back to COPY, STORE \Deleted and EXPUNGE; this
			// server never expunges, so it refuses instead.
			return ErrNoMoveCap
		}
		srcAttrs, ok, err := folderInfo(c, in.Criterion.Folder)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSourceMissing
		}
		if in.Action == ActionMove && (hasAttr(srcAttrs, imap.MailboxAttrAll) || hasAttr(srcAttrs, imap.MailboxAttrNoSelect)) {
			return SafeError("cannot move out of a folder that holds every message (\\All) or cannot be selected")
		}
		dstAttrs, ok, err := folderInfo(c, in.Target)
		if err != nil {
			return err
		}
		if !ok {
			return ErrTargetMissing
		}
		if isSpecialUse(dstAttrs) {
			return ErrSpecialUse
		}

		// SELECT, not EXAMINE: this is the one place the server writes.
		imapx.SetPhase(ctx, "select")
		sel, err := c.Select(in.Criterion.Folder, nil).Wait()
		if err != nil {
			return err
		}
		if sel.UIDValidity != validity {
			return ErrUIDValidity
		}
		mu.Lock()
		sessionStarted = true
		mu.Unlock()

		// run acts on ms in chunks. Each chunk is one MOVE or COPY; with UIDPLUS the
		// server says which UIDs it acted on and only those count, without it the
		// whole chunk is assumed (a UID that had vanished would be counted; undo
		// copes, it just finds nothing). Ids that were already in the target go to
		// already instead of touched.
		run := func(ms []uidMember, action string, touched, already *[]Touched, inTarget map[string]bool) error {
			for start := 0; start < len(ms); start += chunkUIDs {
				if err := ctx.Err(); err != nil {
					return err
				}
				chunk := ms[start:min(start+chunkUIDs, len(ms))]
				uids := make([]imap.UID, len(chunk))
				for i, m := range chunk {
					uids[i] = m.uid
				}
				set := imap.UIDSetNum(uids...)
				var done map[imap.UID]bool
				var have bool
				var err error
				imapx.SetPhase(ctx, action)
				if action == ActionMove {
					var md *imapclient.MoveData
					md, err = c.Move(set, in.Target).Wait()
					if err == nil && md != nil {
						done, have = returnedUIDs(md.SourceUIDs)
					}
				} else {
					var cd *imap.CopyData
					cd, err = c.Copy(set, in.Target).Wait()
					if err == nil && cd != nil {
						done, have = returnedUIDs(cd.SourceUIDs)
					}
				}
				if err != nil {
					return err
				}
				mu.Lock()
				for _, m := range chunk {
					if have && !done[m.uid] {
						continue
					}
					t := Touched{StableID: m.id, FromFolder: in.Criterion.Folder}
					if inTarget[m.id] {
						*already = append(*already, t)
					} else {
						*touched = append(*touched, t)
					}
				}
				mu.Unlock()
			}
			return nil
		}

		if len(members) > 0 {
			if err := run(members, in.Action, &touched, &alreadyDone, already); err != nil {
				return err
			}
		}
		if len(copyBack) > 0 {
			// COPY, not MOVE: these were in the target before the apply, and a
			// move would strip that label. The copy adds the source back.
			var none []Touched
			if err := run(copyBack, ActionLabel, &copiedOut, &none, nil); err != nil {
				return err
			}
		}
		return nil
	})
	mu.Lock()
	out.Touched, out.AlreadyInTarget, out.CopiedBack = touched, alreadyDone, copiedOut
	started := sessionStarted
	mu.Unlock()
	if !started && actErr != nil {
		// Refused or failed before anything was changed: nothing to re-read.
		return out, actErr
	}

	// Re-read the folders so membership says where things are now, whether or
	// not every chunk went through, over a fresh connection (the one that
	// just failed may be dead or mid-command) and a fresh context (the
	// apply's may be what ended). Its failure is logged, not returned: the
	// apply's own result stands, and the next scheduled refresh catches up.
	if rerr := imapx.Do(context.WithoutCancel(ctx), a, "apply_refresh", afterApplyRefreshBudget, func(ctx context.Context, c *imapclient.Client) error {
		imapx.SetPhase(ctx, "refresh")
		_, err := o.store.RefreshFolders(ctx, c, a, []string{in.Criterion.Folder, in.Target})
		return err
	}); rerr != nil {
		slog.Warn("organise: refresh after apply failed", "account", a.Name, "err", rerr)
	}
	if actErr != nil {
		return out, fmt.Errorf("%w (after %d of %d messages)", actErr, len(out.Touched)+len(out.AlreadyInTarget)+len(out.CopiedBack), len(members)+len(copyBack))
	}
	return out, nil
}

type uidMember struct {
	id  string
	uid imap.UID
}

// IsSafe reports whether err's text may be shown to a client.
func IsSafe(err error) bool {
	var s SafeError
	return errors.As(err, &s)
}
