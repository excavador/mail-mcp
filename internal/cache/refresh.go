package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	bodyBatchBytes = 32 << 20
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
	size     int64
	internal time.Time
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

	for start := 0; start < len(fresh); start += headerBatch {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		end := min(start+headerBatch, len(fresh))
		bs, err := c.refreshBatch(ctx, a, client, folder, sel.UIDValidity, fresh[start:end])
		st.NewIDs += bs.NewIDs
		st.NewBodies += bs.NewBodies
		st.Skipped += bs.Skipped
		if err != nil {
			return st, err
		}
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
// Membership is written last, only for messages that are now in the index, so
// an interrupted refresh leaves the UID unknown and picks it up next time.
func (c *Cache) refreshBatch(ctx context.Context, a accounts.Account, client *imapclient.Client, folder string, validity uint32, uids []imap.UID) (Stats, error) {
	var st Stats

	hdrSection := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: idHeaderFields,
		Peek:         true,
	}
	set := imap.UIDSetNum(uids...)
	msgs, err := client.Fetch(set, &imap.FetchOptions{
		UID:          true,
		RFC822Size:   true,
		InternalDate: true,
		BodySection:  []*imap.FetchItemBodySection{hdrSection},
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
		id, err := stableID(a.Provider, m.FindBodySection(hdrSection), m.RFC822Size, m.InternalDate)
		if err != nil {
			return st, fmt.Errorf("uid %d: %w", m.UID, err)
		}
		infos[m.UID] = headerInfo{stableID: id, size: m.RFC822Size, internal: m.InternalDate}
	}

	// One body per stable id, however many UIDs (or folders) carry it.
	var needBody []imap.UID
	wanted := map[string]bool{}
	for _, u := range uids {
		info, ok := infos[u]
		if !ok || wanted[info.stableID] {
			continue
		}
		have, err := c.hasMessage(ctx, a.Name, info.stableID)
		if err != nil {
			return st, err
		}
		if !have {
			wanted[info.stableID] = true
			needBody = append(needBody, u)
		}
	}
	st.NewIDs = len(needBody)

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
		n, err := c.fetchBodies(ctx, a, client, needBody[start:end], infos)
		st.NewBodies += n
		if err != nil {
			return st, err
		}
		start = end
	}

	rows := make([]memberRow, 0, len(uids))
	for _, u := range uids {
		info, ok := infos[u]
		if !ok {
			continue // expunged between LIST and FETCH
		}
		have, err := c.hasMessage(ctx, a.Name, info.stableID)
		if err != nil {
			return st, err
		}
		if have {
			rows = append(rows, memberRow{uid: u, stableID: info.stableID})
		}
	}
	return st, c.addMembership(ctx, a.Name, folder, validity, rows)
}

func (c *Cache) fetchBodies(ctx context.Context, a accounts.Account, client *imapclient.Client, uids []imap.UID, infos map[imap.UID]headerInfo) (int, error) {
	section := &imap.FetchItemBodySection{Peek: true} // BODY.PEEK[]: whole message, \Seen untouched
	msgs, err := client.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return 0, fmt.Errorf("fetch bodies: %w", err)
	}
	stored := 0
	for _, m := range msgs {
		raw := m.FindBodySection(section)
		info, ok := infos[m.UID]
		if raw == nil || !ok {
			continue
		}
		sum, err := c.putBlob(raw)
		if err != nil {
			return stored, err
		}
		if err := c.insertMessage(ctx, a.Name, info, sum, parseMessage(raw)); err != nil {
			return stored, err
		}
		stored++
	}
	return stored, nil
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
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES (?, ?, ?, ?, ?)`,
			account, r.stableID, folder, uint32(r.uid), validity); err != nil {
			return fmt.Errorf("add membership: %w", err)
		}
	}
	return tx.Commit()
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

// insertMessage adds the index and FTS rows for a message whose blob is
// already on disk. INSERT OR IGNORE: an existing entry is never rewritten.
func (c *Cache) insertMessage(ctx context.Context, account string, info headerInfo, blobSum string, p parsed) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var dateUnix int64
	if !p.Date.IsZero() {
		dateUnix = p.Date.Unix()
	}
	res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO messages
	(account, stable_id, blob_sha256, from_addr, to_addr, cc_addr, subject, date_unix, list_id, gh_reason, size, internal_date)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		account, info.stableID, blobSum, p.From, p.To, p.Cc, p.Subject, dateUnix, p.ListID, p.GitHubReason, info.size, info.internal.Unix())
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
	return tx.Commit()
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
