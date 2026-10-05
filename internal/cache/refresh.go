package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
)

const (
	// headerBatch UIDs per header-only FETCH: the response is a few hundred
	// bytes per message, so a large batch keeps round trips (the cost that
	// matters against Bridge) low.
	headerBatch = 200
	// bodyBatch UIDs per full-body FETCH: bodies are read into memory, so
	// this is smaller to bound it when a batch happens to hold attachments.
	bodyBatch = 25
	// bodyBatchBytes caps the summed RFC822.SIZE of one body FETCH, since
	// bodies are read into memory.
	bodyBatchBytes = 16 << 20
	// maxMessageSize is the largest message cached. Anything bigger is
	// skipped (counted, logged, not stored) rather than held in memory.
	maxMessageSize = 50 << 20
)

// Stats counts what one Refresh did.
type Stats struct {
	// Folders examined.
	Folders int `json:"folders"`
	// NewUIDs is folder entries not seen before (a message in three Gmail
	// labels counts three times).
	NewUIDs int `json:"new_uids"`
	// NewIDs is stable ids not yet in the index: messages new to the cache.
	NewIDs int `json:"new_ids"`
	// NewBodies is full bodies fetched and stored. It equals NewIDs unless a
	// message vanished between the header fetch and the body fetch.
	NewBodies int `json:"new_bodies"`
	// Removed is membership rows dropped because the message left the folder
	// (or the folder's UIDVALIDITY changed). Blobs and index rows stay.
	Removed int `json:"removed"`
	// Skipped is messages not cached because they exceed maxMessageSize.
	Skipped int `json:"skipped"`
}

func (s *Stats) add(o Stats) {
	s.NewUIDs += o.NewUIDs
	s.NewIDs += o.NewIDs
	s.NewBodies += o.NewBodies
	s.Removed += o.Removed
	s.Skipped += o.Skipped
}

// Refresh brings the cache of one account up to date with the mailbox behind
// client, which must already be logged in.
//
// It never changes the mailbox: folders are opened with EXAMINE and every
// FETCH uses BODY.PEEK, so no message is marked read. It is cheap on repeat:
// a second refresh with nothing new costs one LIST and, per folder, one
// EXAMINE and one UID-only FETCH, and fetches no bodies.
//
// A failure in one folder does not stop the others; the errors are joined
// and returned after every folder has been tried, and what was already
// stored stays valid, so the next refresh resumes rather than restarts.
func (c *Cache) Refresh(ctx context.Context, a accounts.Account, client *imapclient.Client) (st Stats, err error) {
	defer func() { c.recordRefresh(a.Name, st, err == nil) }()

	list, err := client.List("", "*", nil).Collect()
	if err != nil {
		return st, fmt.Errorf("%s: list folders: %w", a.Name, err)
	}
	var errs []error
	for _, m := range list {
		if hasAttr(m.Attrs, imap.MailboxAttrNoSelect) || hasAttr(m.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return st, err
		}
		fs, err := c.refreshFolder(ctx, a, client, m.Mailbox)
		st.add(fs)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: folder %q: %w", a.Name, m.Mailbox, err))
			continue
		}
		st.Folders++
	}
	return st, errors.Join(errs...)
}

func hasAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

type headerInfo struct {
	stableID string
	// gmThreadID is X-GM-THRID in decimal, "" when not fetched (non-Gmail).
	gmThreadID string
	size       int64
	internal   time.Time
}

func (c *Cache) refreshFolder(ctx context.Context, a accounts.Account, client *imapclient.Client, folder string) (Stats, error) {
	var st Stats

	// EXAMINE, not SELECT: read-only, so nothing here can set \Seen or any
	// other flag, even by accident.
	sel, err := client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return st, fmt.Errorf("examine: %w", err)
	}

	known, validity, haveFolder, err := c.loadFolder(ctx, a.Name, folder)
	if err != nil {
		return st, err
	}
	if haveFolder && validity != sel.UIDValidity {
		// UIDs from the old validity mean nothing now: they may be reused by
		// different messages. Forget them all and rescan.
		known = map[imap.UID]string{}
		n, err := c.dropFolder(ctx, a.Name, folder)
		if err != nil {
			return st, err
		}
		st.Removed += n
	}

	current, err := listUIDs(client, sel.NumMessages)
	if err != nil {
		return st, err
	}
	inFolder := make(map[imap.UID]bool, len(current))
	var fresh []imap.UID
	for _, u := range current {
		inFolder[u] = true
		if _, ok := known[u]; !ok {
			fresh = append(fresh, u)
		}
	}
	var gone []imap.UID
	for u := range known {
		if !inFolder[u] {
			gone = append(gone, u)
		}
	}
	// Membership reflects the server now: what left the folder disappears
	// from it. Its blob and index entry deliberately stay.
	n, err := c.setFolder(ctx, a.Name, folder, sel.UIDValidity, gone)
	if err != nil {
		return st, err
	}
	st.Removed += n
	st.NewUIDs = len(fresh)

	emit := progressFrom(ctx)
	emit(Progress{Folder: folder, Done: 0, Total: len(fresh)})
	for start := 0; start < len(fresh); start += headerBatch {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		end := min(start+headerBatch, len(fresh))
		// step reports progress from inside the batch, so a slow batch of
		// bodies still counts as the refresh being alive.
		step := func(resolved, bodies int) {
			emit(Progress{Folder: folder, Done: start + resolved, Total: len(fresh), NewBodies: st.NewBodies + bodies})
		}
		bs, err := c.refreshBatch(ctx, a, client, folder, sel.UIDValidity, fresh[start:end], step)
		st.NewIDs += bs.NewIDs
		st.NewBodies += bs.NewBodies
		st.Skipped += bs.Skipped
		if err != nil {
			return st, err
		}
		emit(Progress{Folder: folder, Done: end, Total: len(fresh), NewBodies: st.NewBodies})
	}
	return st, nil
}

// listUIDs returns every UID in the selected folder with one UID FETCH asking
// for nothing but UIDs. Bridge has no CONDSTORE, so there is no cheaper "what
// changed" to ask; a per-folder scan is the expected cost.
func listUIDs(client *imapclient.Client, exists uint32) ([]imap.UID, error) {
	if exists == 0 {
		return nil, nil
	}
	var all imap.UIDSet
	all.AddRange(1, 0) // 1:*
	msgs, err := client.Fetch(all, &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("list uids: %w", err)
	}
	out := make([]imap.UID, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.UID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// refreshBatch resolves stable ids for one batch of new UIDs, fetches the
// bodies of those whose id the index has not seen, and records membership.
//
// Membership is written incrementally, never ahead of the index: UIDs whose
// stable id is already indexed are recorded right after the header fetch, and
// the rest are recorded by fetchBodies in the same transaction that indexes
// their message. An interrupted refresh therefore loses at most the batch in
// flight; the UIDs it had not recorded stay unknown and are picked up next
// time, while the ones it had are not fetched again.
func (c *Cache) refreshBatch(ctx context.Context, a accounts.Account, client *imapclient.Client, folder string, validity uint32, uids []imap.UID, step func(resolved, bodies int)) (Stats, error) {
	var st Stats

	hdrSection := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: idHeaderFields,
		Peek:         true,
	}
	// Gmail with X-GM-EXT-1: ask for X-GM-MSGID (the stable id) and
	// X-GM-THRID (stored, not keyed on) in the same fetch. Without the
	// capability the attributes are not requested, as a server that lacks
	// the extension would reject them.
	gmail := a.Provider == accounts.Gmail && client.Caps().Has(imap.CapGmailExt1)
	set := imap.UIDSetNum(uids...)
	msgs, err := client.Fetch(set, &imap.FetchOptions{
		UID:           true,
		RFC822Size:    true,
		InternalDate:  true,
		GmailMsgID:    gmail,
		GmailThreadID: gmail,
		BodySection:   []*imap.FetchItemBodySection{hdrSection},
	}).Collect()
	if err != nil {
		return st, fmt.Errorf("fetch headers: %w", err)
	}

	infos := make(map[imap.UID]headerInfo, len(msgs))
	skipped := map[imap.UID]bool{}
	for _, m := range msgs {
		if m.RFC822Size > maxMessageSize {
			skipped[m.UID] = true
			st.Skipped++
			slog.Warn("cache: message too large, not cached",
				"account", a.Name, "folder", folder, "uid", uint32(m.UID), "size", m.RFC822Size)
			continue
		}
		id, err := stableIDFor(a.Provider, m.FindBodySection(hdrSection), m.RFC822Size, m.InternalDate, m.GmailMsgID)
		if err != nil {
			return st, fmt.Errorf("uid %d: %w", m.UID, err)
		}
		info := headerInfo{stableID: id, size: m.RFC822Size, internal: m.InternalDate}
		if m.GmailThreadID != 0 {
			info.gmThreadID = strconv.FormatUint(m.GmailThreadID, 10)
		}
		infos[m.UID] = info
	}

	// One body per stable id, however many UIDs (or folders) carry it.
	// known rows go to membership now; pending maps each not-yet-indexed id
	// to every UID in this batch that carries it.
	var needBody []imap.UID
	var known []memberRow
	pending := map[string][]imap.UID{}
	for _, u := range uids {
		info, ok := infos[u]
		if !ok {
			continue // expunged between LIST and FETCH, or skipped as too large
		}
		if _, dup := pending[info.stableID]; dup {
			pending[info.stableID] = append(pending[info.stableID], u)
			continue
		}
		have, err := c.hasMessage(ctx, a.Name, info.stableID)
		if err != nil {
			return st, err
		}
		if have {
			known = append(known, memberRow{uid: u, stableID: info.stableID})
			continue
		}
		pending[info.stableID] = []imap.UID{u}
		needBody = append(needBody, u)
	}
	st.NewIDs = len(needBody)
	resolved := len(known)
	if err := c.addMembership(ctx, a.Name, folder, validity, known); err != nil {
		return st, err
	}
	step(resolved, 0)

	for start := 0; start < len(needBody); {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		end, total := start, int64(0)
		for end < len(needBody) && end-start < bodyBatch {
			sz := infos[needBody[end]].size
			if end > start && total+sz > bodyBatchBytes {
				break
			}
			total += sz
			end++
		}
		n, rows, err := c.fetchBodies(ctx, a, client, folder, validity, needBody[start:end], infos, pending)
		st.NewBodies += n
		resolved += rows
		if err != nil {
			return st, err
		}
		step(resolved, st.NewBodies)
		start = end
	}
	return st, nil
}

// fetchBodies fetches and stores the bodies of uids. The index rows of the
// whole batch and the membership rows of every UID in pending that carries one
// of those messages are committed in one transaction, so a UID is a member of
// the folder exactly when its message is indexed. It returns the messages
// stored and the membership rows written.
func (c *Cache) fetchBodies(ctx context.Context, a accounts.Account, client *imapclient.Client, folder string, validity uint32, uids []imap.UID, infos map[imap.UID]headerInfo, pending map[string][]imap.UID) (int, int, error) {
	section := &imap.FetchItemBodySection{Peek: true} // BODY.PEEK[]: whole message, \Seen untouched
	msgs, err := client.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return 0, 0, fmt.Errorf("fetch bodies: %w", err)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored, rows := 0, 0
	for i := range msgs {
		m := msgs[i]
		raw := m.FindBodySection(section)
		info, ok := infos[m.UID]
		if raw == nil || !ok {
			continue
		}
		sum, err := c.putBlob(raw)
		if err != nil {
			return 0, 0, err
		}
		// parseMessage keeps no reference to raw (the indexed text is a
		// bounded copy), so drop the buffer as soon as the message is
		// handled instead of holding the whole batch until Collect's result
		// goes out of scope.
		p := parseMessage(raw)
		raw, m.BodySection = nil, nil
		if err := insertMessageTx(ctx, tx, a.Name, info, sum, p); err != nil {
			return 0, 0, err
		}
		stored++
		for _, u := range pending[info.stableID] {
			if err := addMembershipTx(ctx, tx, a.Name, folder, validity, memberRow{uid: u, stableID: info.stableID}); err != nil {
				return 0, 0, err
			}
			rows++
		}
	}
	// All or nothing: a failure above rolls the batch back (blobs stay, they
	// are write-once and harmless), so no UID is a member without its message.
	return stored, rows, tx.Commit()
}

// --- index access -----------------------------------------------------------

func (c *Cache) loadFolder(ctx context.Context, account, folder string) (map[imap.UID]string, uint32, bool, error) {
	var validity uint32
	err := c.db.QueryRowContext(ctx, `SELECT uidvalidity FROM folders WHERE account = ? AND folder = ?`, account, folder).Scan(&validity)
	have := true
	if errors.Is(err, sql.ErrNoRows) {
		have, err = false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("load folder: %w", err)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT uid, stable_id FROM membership WHERE account = ? AND folder = ?`, account, folder)
	if err != nil {
		return nil, 0, false, fmt.Errorf("load membership: %w", err)
	}
	defer rows.Close()
	known := map[imap.UID]string{}
	for rows.Next() {
		var (
			uid uint32
			id  string
		)
		if err := rows.Scan(&uid, &id); err != nil {
			return nil, 0, false, fmt.Errorf("load membership: %w", err)
		}
		known[imap.UID(uid)] = id
	}
	return known, validity, have, rows.Err()
}

func (c *Cache) dropFolder(ctx context.Context, account, folder string) (int, error) {
	res, err := c.db.ExecContext(ctx, `DELETE FROM membership WHERE account = ? AND folder = ?`, account, folder)
	if err != nil {
		return 0, fmt.Errorf("drop membership: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// setFolder records the folder's UIDVALIDITY and deletes the membership rows
// for UIDs that are gone, atomically.
func (c *Cache) setFolder(ctx context.Context, account, folder string, validity uint32, gone []imap.UID) (int, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO folders (account, folder, uidvalidity) VALUES (?, ?, ?)`, account, folder, validity); err != nil {
		return 0, fmt.Errorf("record folder: %w", err)
	}
	removed := 0
	for _, u := range gone {
		res, err := tx.ExecContext(ctx, `DELETE FROM membership WHERE account = ? AND folder = ? AND uid = ?`, account, folder, uint32(u))
		if err != nil {
			return 0, fmt.Errorf("remove membership: %w", err)
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	return removed, tx.Commit()
}

type memberRow struct {
	uid      imap.UID
	stableID string
}

func (c *Cache) addMembership(ctx context.Context, account, folder string, validity uint32, rows []memberRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		if err := addMembershipTx(ctx, tx, account, folder, validity, r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func addMembershipTx(ctx context.Context, tx *sql.Tx, account, folder string, validity uint32, r memberRow) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES (?, ?, ?, ?, ?)`,
		account, r.stableID, folder, uint32(r.uid), validity); err != nil {
		return fmt.Errorf("add membership: %w", err)
	}
	return nil
}

func (c *Cache) hasMessage(ctx context.Context, account, stableID string) (bool, error) {
	var one int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE account = ? AND stable_id = ?`, account, stableID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up message: %w", err)
	}
	return true, nil
}

// insertMessageTx adds, inside tx, the index and FTS rows for a message whose
// blob is already on disk. INSERT OR IGNORE: an existing entry is never rewritten.
func insertMessageTx(ctx context.Context, tx *sql.Tx, account string, info headerInfo, blobSum string, p parsed) error {
	var dateUnix int64
	if !p.Date.IsZero() {
		dateUnix = p.Date.Unix()
	}
	res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO messages
	(account, stable_id, blob_sha256, from_addr, to_addr, cc_addr, subject, date_unix, list_id, gh_reason, size, internal_date, gm_thread_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		account, info.stableID, blobSum, p.From, p.To, p.Cc, p.Subject, dateUnix, p.ListID, p.GitHubReason, info.size, info.internal.Unix(), info.gmThreadID)
	if err != nil {
		return fmt.Errorf("index message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_fts (subject, from_addr, to_addr, cc_addr, body, account, stable_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.Subject, p.From, p.To, p.Cc, p.Body, account, info.stableID); err != nil {
			return fmt.Errorf("index text: %w", err)
		}
	}
	return nil
}

func (c *Cache) recordRefresh(account string, st Stats, ok bool) {
	o := 0
	if ok {
		o = 1
	}
	// Best effort: failing to note a refresh must not turn a good refresh
	// into a bad one. A fresh context, since the refresh's may be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = c.db.ExecContext(ctx, `
INSERT OR REPLACE INTO refreshes (account, at, ok, folders, new_uids, new_ids, new_bodies, removed)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		account, time.Now().Unix(), o, st.Folders, st.NewUIDs, st.NewIDs, st.NewBodies, st.Removed)
}
