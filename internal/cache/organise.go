package cache

// The cache side of organising: which cached messages an intent covers, where
// they are now, and re-reading folders after a move. Resolving only reads the
// index; RefreshFolders reads the mailbox with EXAMINE, like every refresh.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// ErrTooMany means a resolve matched more messages than the caller allowed.
var ErrTooMany = errors.New("too many messages match")

// MemberQuery selects the members of one folder. Zero fields do not constrain;
// the rest are ANDed.
type MemberQuery struct {
	Folder          string    // required
	From            string    // bare address (or "Name <addr>"), exact, case-insensitive
	To              string    // substring of the To header
	SubjectContains string    // substring
	ListID          string    // exact, case-insensitive; "<id>" brackets optional
	GitHubReason    string    // exact, case-insensitive
	Since           time.Time // message date, inclusive
	Before          time.Time // message date, exclusive
	// ReceivedAfter keeps only messages the server received after this
	// instant (internal date): "new mail since".
	ReceivedAfter time.Time
}

// Member is one folder membership: where a message is, in the terms IMAP
// needs to act on it.
type Member struct {
	StableID    string
	Folder      string
	UID         uint32
	UIDValidity uint32
}

// BareAddr reduces "Name <addr>" or "addr" to a lowercase bare address, the
// same reduction senderAddrSQL applies to the stored From header.
func BareAddr(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			s = s[i+1 : i+j]
		}
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// listIDSQL is senderAddrSQL's twin for the List-Id header ("Name <id>" or "id").
const listIDSQL = `lower(CASE WHEN instr(m.list_id, '<') > 0
	THEN substr(m.list_id, instr(m.list_id, '<') + 1, instr(m.list_id, '>') - instr(m.list_id, '<') - 1)
	ELSE trim(m.list_id) END)`

const memberSelect = `
SELECT s.stable_id, s.folder, s.uid, s.uidvalidity
FROM membership s JOIN messages m ON m.account = s.account AND m.stable_id = s.stable_id
WHERE s.account = ? AND s.folder = ?`

// ResolveMembers returns the members of q.Folder that match q, newest first.
// It fails with ErrTooMany when more than max match, rather than truncating:
// an intent must act on exactly what it previewed.
func (c *Cache) ResolveMembers(ctx context.Context, account string, q MemberQuery, max int) ([]Member, error) {
	where := ``
	args := []any{account, q.Folder}
	if q.From != "" {
		where += ` AND ` + senderAddrSQL + ` = ?`
		args = append(args, BareAddr(q.From))
	}
	if q.ListID != "" {
		where += ` AND ` + listIDSQL + ` = ?`
		args = append(args, BareAddr(strings.Trim(strings.TrimSpace(q.ListID), "<>")))
	}
	if q.GitHubReason != "" {
		where += ` AND lower(m.gh_reason) = lower(?)`
		args = append(args, strings.TrimSpace(q.GitHubReason))
	}
	if q.To != "" {
		where += ` AND instr(lower(m.to_addr), lower(?)) > 0`
		args = append(args, q.To)
	}
	if q.SubjectContains != "" {
		where += ` AND instr(lower(m.subject), lower(?)) > 0`
		args = append(args, q.SubjectContains)
	}
	if !q.Since.IsZero() {
		where += ` AND ` + dateCol + ` >= ?`
		args = append(args, q.Since.Unix())
	}
	if !q.Before.IsZero() {
		where += ` AND ` + dateCol + ` < ?`
		args = append(args, q.Before.Unix())
	}
	if !q.ReceivedAfter.IsZero() {
		where += ` AND m.internal_date > ?`
		args = append(args, q.ReceivedAfter.Unix())
	}
	args = append(args, max+1)
	return c.queryMembers(ctx, memberSelect+where+` ORDER BY `+dateCol+` DESC, s.stable_id, s.uid LIMIT ?`, args, max)
}

// MembersByID returns the memberships of the given stable ids in one folder.
// The id list is chunked, so any length is fine.
func (c *Cache) MembersByID(ctx context.Context, account, folder string, ids []string) ([]Member, error) {
	var out []Member
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := []any{account, folder}
		for _, id := range part {
			args = append(args, id)
		}
		ms, err := c.queryMembers(ctx, memberSelect+` AND s.stable_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`) ORDER BY s.stable_id, s.uid`, args, -1)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

func (c *Cache) queryMembers(ctx context.Context, query string, args []any, max int) ([]Member, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: resolve members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.StableID, &m.Folder, &m.UID, &m.UIDValidity); err != nil {
			return nil, fmt.Errorf("cache: resolve members: %w", err)
		}
		out = append(out, m)
		if max >= 0 && len(out) > max {
			return nil, ErrTooMany
		}
	}
	return out, rows.Err()
}

// Summaries returns the index data for the given stable ids (folders are not
// filled in), newest first. Unknown ids are left out.
func (c *Cache) Summaries(ctx context.Context, account string, ids []string) ([]SearchHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := []any{account}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT m.stable_id, `+dateCol+`, m.from_addr, m.subject FROM messages m
WHERE m.account = ? AND m.stable_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
ORDER BY `+dateCol+` DESC, m.stable_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: summaries: %w", err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		h := SearchHit{Account: account}
		var d int64
		if err := rows.Scan(&h.StableID, &d, &h.From, &h.Subject); err != nil {
			return nil, fmt.Errorf("cache: summaries: %w", err)
		}
		h.Date = time.Unix(d, 0).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// RefreshFolders brings the cache of the named folders up to date with the
// mailbox behind client, which must already be logged in. It is Refresh for a
// few folders, reusing the same per-folder logic (EXAMINE, BODY.PEEK): after
// an organise it is how membership comes to reflect the move. Blobs and index
// rows are untouched except for messages new to the cache.
func (c *Cache) RefreshFolders(ctx context.Context, client *imapclient.Client, a accounts.Account, folders []string) (Stats, error) {
	defer c.foreground()()
	var st Stats
	var errs []error
	for _, f := range folders {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		fs, err := c.refreshFolder(ctx, a, client, f, folderStatus{})
		st.add(fs)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: folder %q: %w", a.Name, f, err))
			continue
		}
		st.Folders++
	}
	return st, errors.Join(errs...)
}
