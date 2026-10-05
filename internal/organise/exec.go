package organise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

const (
	// chunkUIDs UIDs per MOVE or COPY: one command, bounded size.
	chunkUIDs = 500
	// applyBudget bounds one apply end to end (dial, moves, re-reading).
	applyBudget = 10 * time.Minute
	// createBudget bounds create_folder.
	createBudget = 30 * time.Second
	// undoRefreshBudget bounds the refresh undo does before it looks.
	undoRefreshBudget = 2 * time.Minute
)

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
	ctx, cancel := context.WithTimeout(ctx, createBudget)
	defer cancel()
	c, err := imapx.Dial(ctx, a)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, ok, err := folderInfo(c, name); err != nil {
		return err
	} else if ok {
		return nil
	}
	if err := c.Create(name, nil).Wait(); err != nil {
		// Lost a race with another client: fine if it is there now.
		if _, ok, lerr := folderInfo(c, name); lerr == nil && ok {
			return nil
		}
		return err
	}
	_ = c.Logout().Wait()
	return nil
}

// RefreshFolder refreshes one folder of the cache (undo does this to the
// folder it is about to read memberships from).
func (o *Organiser) RefreshFolder(ctx context.Context, a accounts.Account, folder string) error {
	ctx, cancel := context.WithTimeout(ctx, undoRefreshBudget)
	defer cancel()
	c, err := imapx.Dial(ctx, a)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = o.store.RefreshFolders(ctx, c, a, []string{folder})
	return err
}

// Apply executes an approved preview. The approval itself is the caller's
// business; Apply's job is to do no more than the preview said.
//
// It re-resolves the criterion now and acts only on the intersection with the
// previewed set, so mail that arrived after the preview is never touched. The
// caller holds the account's write slot.
func (o *Organiser) Apply(ctx context.Context, a accounts.Account, p *Preview) (out Outcome, err error) {
	out.Matched = len(p.IDs)
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
	var validity uint32
	for _, m := range now {
		if !approved[m.StableID] {
			continue // arrived or started matching after the preview
		}
		acting[m.StableID] = true
		members = append(members, uidMember{id: m.StableID, uid: imap.UID(m.UID)})
		validity = m.UIDValidity
	}
	out.Skipped = len(p.IDs) - len(acting)
	if len(members) == 0 {
		return out, nil
	}

	ctx, cancel := context.WithTimeout(ctx, applyBudget)
	defer cancel()
	c, err := imapx.Dial(ctx, a)
	if err != nil {
		return out, err
	}
	defer func() { _ = c.Close() }()

	if in.Action == ActionMove && !c.Caps().Has(imap.CapMove) {
		// go-imap would fall back to COPY, STORE \Deleted and EXPUNGE; this
		// server never expunges, so it refuses instead.
		return out, ErrNoMoveCap
	}
	srcAttrs, ok, err := folderInfo(c, in.Criterion.Folder)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrSourceMissing
	}
	if in.Action == ActionMove && (hasAttr(srcAttrs, imap.MailboxAttrAll) || hasAttr(srcAttrs, imap.MailboxAttrNoSelect)) {
		return out, SafeError("cannot move out of a folder that holds every message (\\All) or cannot be selected")
	}
	if _, ok, err := folderInfo(c, in.Target); err != nil {
		return out, err
	} else if !ok {
		return out, ErrTargetMissing
	}

	// SELECT, not EXAMINE: this is the one place the server writes.
	sel, err := c.Select(in.Criterion.Folder, nil).Wait()
	if err != nil {
		return out, err
	}
	if sel.UIDValidity != validity {
		return out, ErrUIDValidity
	}

	var actErr error
	for start := 0; start < len(members); start += chunkUIDs {
		if err := ctx.Err(); err != nil {
			actErr = err
			break
		}
		chunk := members[start:min(start+chunkUIDs, len(members))]
		uids := make([]imap.UID, len(chunk))
		for i, m := range chunk {
			uids[i] = m.uid
		}
		set := imap.UIDSetNum(uids...)
		if in.Action == ActionMove {
			_, err = c.Move(set, in.Target).Wait()
		} else {
			_, err = c.Copy(set, in.Target).Wait()
		}
		if err != nil {
			actErr = err
			break
		}
		for _, m := range chunk {
			out.Touched = append(out.Touched, Touched{StableID: m.id, FromFolder: in.Criterion.Folder})
		}
	}

	// Re-read the folders so membership says where things are now, whether or
	// not every chunk went through. A fresh context: the apply's may be what
	// ended.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), undoRefreshBudget)
	defer rcancel()
	if _, rerr := o.store.RefreshFolders(rctx, c, a, []string{in.Criterion.Folder, in.Target}); rerr != nil {
		slog.Warn("organise: refresh after apply failed", "account", a.Name, "err", rerr)
	}
	if actErr != nil {
		return out, fmt.Errorf("%w (after %d of %d messages)", actErr, len(out.Touched), len(members))
	}
	_ = c.Logout().Wait()
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
