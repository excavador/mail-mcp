package cache

// Tags and saved queries: the mutable layer over the immutable messages, in
// the notmuch pattern. Both are local: nothing here reaches a mailbox. A tag
// is keyed by stable id, so it follows a message through moves and label
// changes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds.
const (
	// MaxTagTargets is the most messages one tag or untag call may change.
	MaxTagTargets = 5000
	maxTagsPerAcc = 1000
	maxTagRows    = 100000 // tag rows per account
	maxSavedPerAc = 200
	maxSavedJSON  = 4096
	maxNoteRunes  = 500
	tagIDChunk    = 400
)

var (
	tagRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,99}$`)

	// ErrBadTag is an invalid tag or saved-query name; the text is safe to show.
	ErrBadTag = errors.New("a tag is lowercase letters, digits and . _ / - , starts with a letter or digit, and is at most 100 characters")
	// ErrTagLimit is a refused write for size; the text is safe to show.
	ErrTagLimit = errors.New("limit reached")
)

// NormalizeTag lowercases and trims s and checks it against the tag syntax.
func NormalizeTag(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !tagRE.MatchString(s) {
		return "", ErrBadTag
	}
	return s, nil
}

func tagPlaceholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func tagChunks(ids []string, n int) [][]string {
	var out [][]string
	for len(ids) > 0 {
		k := min(n, len(ids))
		out = append(out, ids[:k])
		ids = ids[k:]
	}
	return out
}

// ExistingIDs returns the given stable ids that the account has, in input
// order, without duplicates.
func (c *Cache) ExistingIDs(ctx context.Context, account string, ids []string) ([]string, error) {
	have := map[string]bool{}
	for _, part := range tagChunks(ids, tagIDChunk) {
		args := []any{account}
		for _, id := range part {
			args = append(args, id)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT stable_id FROM messages WHERE account = ? AND stable_id IN (`+tagPlaceholders(len(part))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("cache: look up ids: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("cache: look up ids: %w", err)
			}
			have[id] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("cache: look up ids: %w", err)
		}
	}
	var out []string
	for _, id := range ids {
		if have[id] {
			out = append(out, id)
			delete(have, id)
		}
	}
	return out, nil
}

// ThreadIDs returns the stable ids of one thread, at most max+1 (so a caller
// can see it is over the cap).
func (c *Cache) ThreadIDs(ctx context.Context, account, tid string, max int) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT stable_id FROM message_thread WHERE account = ? AND tid = ? ORDER BY stable_id LIMIT ?`, account, tid, max+1)
	if err != nil {
		return nil, fmt.Errorf("cache: thread ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cache: thread ids: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResolveSearch returns the stable ids a search matches (newest first), at
// most max+1 so that a caller can tell it is over the cap. It uses the same
// match set as SearchV2, tag filter included.
func (c *Cache) ResolveSearch(ctx context.Context, q SearchQuery, max int) ([]string, error) {
	plan, err := c.matchSQL(q)
	if err != nil {
		return nil, err
	}
	if plan.empty {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `WITH `+plan.ctes+`
SELECT stable_id FROM match GROUP BY account, stable_id ORDER BY MIN(att), MAX(d) DESC, stable_id LIMIT ?`, append(append([]any{}, plan.args...), max+1)...)
	if err != nil {
		if q.FTSSyntax && strings.TrimSpace(q.Text) != "" {
			return nil, fmt.Errorf("%w: %v", ErrQuerySyntax, err)
		}
		return nil, fmt.Errorf("cache: resolve search: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cache: resolve search: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddTags tags the given messages (which must exist) and returns the ids that
// were not already tagged. historyID is stored with the new rows so that an
// undo removes only what this call added.
func (c *Cache) AddTags(ctx context.Context, account, tag string, ids []string, historyID string) ([]string, error) {
	tag, err := NormalizeTag(tag)
	if err != nil {
		return nil, err
	}
	if len(ids) > MaxTagTargets {
		return nil, fmt.Errorf("%w: at most %d messages per call", ErrTagLimit, MaxTagTargets)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cache: add tags: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT tag) FROM tags WHERE account = ? AND tag <> ?`, account, tag).Scan(&known); err != nil {
		return nil, fmt.Errorf("cache: add tags: %w", err)
	}
	if known >= maxTagsPerAcc {
		return nil, fmt.Errorf("%w: at most %d different tags per account", ErrTagLimit, maxTagsPerAcc)
	}
	var rowsNow int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE account = ?`, account).Scan(&rowsNow); err != nil {
		return nil, fmt.Errorf("cache: add tags: %w", err)
	}
	if rowsNow+len(ids) > maxTagRows {
		return nil, fmt.Errorf("%w: at most %d tagged messages per account", ErrTagLimit, maxTagRows)
	}
	now := c.now().Unix()
	var added []string
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO tags (account, stable_id, tag, added_at, history_id)
SELECT account, stable_id, ?, ?, ? FROM messages WHERE account = ? AND stable_id = ?`, tag, now, historyID, account, id)
		if err != nil {
			return nil, fmt.Errorf("cache: add tags: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			added = append(added, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("cache: add tags: %w", err)
	}
	return added, nil
}

// RemoveTags removes the tag from the given messages. It returns the ids that
// carried it and, by the history id each row was written under, the same ids
// grouped (so an undo can put each back under its original id). With
// onlyHistoryID set, only rows written by that history record are removed
// (undo of tag_messages).
func (c *Cache) RemoveTags(ctx context.Context, account, tag string, ids []string, onlyHistoryID string) ([]string, map[string][]string, error) {
	tag, err := NormalizeTag(tag)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) > MaxTagTargets {
		return nil, nil, fmt.Errorf("%w: at most %d messages per call", ErrTagLimit, MaxTagTargets)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("cache: remove tags: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var removed []string
	byHist := map[string][]string{}
	for _, id := range ids {
		var hid string
		q := `SELECT history_id FROM tags WHERE account = ? AND stable_id = ? AND tag = ?`
		args := []any{account, id, tag}
		if onlyHistoryID != "" {
			q += ` AND history_id = ?`
			args = append(args, onlyHistoryID)
		}
		switch err := tx.QueryRowContext(ctx, q, args...).Scan(&hid); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("cache: remove tags: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE account = ? AND stable_id = ? AND tag = ?`, account, id, tag); err != nil {
			return nil, nil, fmt.Errorf("cache: remove tags: %w", err)
		}
		removed = append(removed, id)
		byHist[hid] = append(byHist[hid], id)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("cache: remove tags: %w", err)
	}
	return removed, byHist, nil
}

// TagCount is a tag with how many messages carry it.
type TagCount struct {
	Tag       string
	Count     int
	LastAdded time.Time
}

// ListTags lists an account's tags, most used first.
func (c *Cache) ListTags(ctx context.Context, account string) ([]TagCount, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT tag, COUNT(*), MAX(added_at) FROM tags WHERE account = ? GROUP BY tag ORDER BY COUNT(*) DESC, tag LIMIT ?`, account, maxTagsPerAcc)
	if err != nil {
		return nil, fmt.Errorf("cache: list tags: %w", err)
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var (
			t  TagCount
			at int64
		)
		if err := rows.Scan(&t.Tag, &t.Count, &at); err != nil {
			return nil, fmt.Errorf("cache: list tags: %w", err)
		}
		t.LastAdded = time.Unix(at, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// SavedFilters is a saved search: the search tool's inputs that select
// messages. Dates are kept as the caller wrote them (RFC 3339 or YYYY-MM-DD).
type SavedFilters struct {
	Query     string `json:"query,omitempty"`
	FTSSyntax bool   `json:"fts_syntax,omitempty"`
	Folder    string `json:"folder,omitempty"`
	From      string `json:"from,omitempty"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
	Tag       string `json:"tag,omitempty"`
	GroupBy   string `json:"group_by,omitempty"`
}

// SavedQuery is a named SavedFilters.
type SavedQuery struct {
	Name      string
	Filters   SavedFilters
	Note      string
	CreatedAt time.Time
}

// SaveQuery stores (or replaces) a saved query.
func (c *Cache) SaveQuery(ctx context.Context, account, name string, f SavedFilters, note string) error {
	name, err := NormalizeTag(name)
	if err != nil {
		return err
	}
	if len(f.Query) > maxQueryBytes || len(f.From) > maxFromBytes {
		return fmt.Errorf("%w: query or from filter is too long", ErrQueryLimit)
	}
	if f.Tag != "" {
		if f.Tag, err = NormalizeTag(f.Tag); err != nil {
			return err
		}
	}
	if utf8.RuneCountInString(note) > maxNoteRunes {
		return fmt.Errorf("%w: note is longer than %d characters", ErrTagLimit, maxNoteRunes)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("cache: save query: %w", err)
	}
	if len(b) > maxSavedJSON {
		return fmt.Errorf("%w: saved query is too large", ErrTagLimit)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cache: save query: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_queries WHERE account = ? AND name <> ?`, account, name).Scan(&n); err != nil {
		return fmt.Errorf("cache: save query: %w", err)
	}
	if n >= maxSavedPerAc {
		return fmt.Errorf("%w: at most %d saved queries per account", ErrTagLimit, maxSavedPerAc)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO saved_queries (account, name, query_json, note, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(account, name) DO UPDATE SET query_json = excluded.query_json, note = excluded.note`,
		account, name, string(b), note, c.now().Unix()); err != nil {
		return fmt.Errorf("cache: save query: %w", err)
	}
	return tx.Commit()
}

// ErrNoSavedQuery is a saved query that does not exist.
var ErrNoSavedQuery = errors.New("no such saved query")

// GetSavedQuery returns one saved query.
func (c *Cache) GetSavedQuery(ctx context.Context, account, name string) (SavedQuery, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	name = strings.ToLower(strings.TrimSpace(name))
	var (
		q    SavedQuery
		js   string
		made int64
	)
	err := c.db.QueryRowContext(ctx, `SELECT name, query_json, note, created_at FROM saved_queries WHERE account = ? AND name = ?`, account, name).Scan(&q.Name, &js, &q.Note, &made)
	if errors.Is(err, sql.ErrNoRows) {
		return q, ErrNoSavedQuery
	}
	if err != nil {
		return q, fmt.Errorf("cache: saved query: %w", err)
	}
	if err := json.Unmarshal([]byte(js), &q.Filters); err != nil {
		return q, fmt.Errorf("cache: saved query: %w", err)
	}
	q.CreatedAt = time.Unix(made, 0).UTC()
	return q, nil
}

// ListSavedQueries lists an account's saved queries by name.
func (c *Cache) ListSavedQueries(ctx context.Context, account string) ([]SavedQuery, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT name, query_json, note, created_at FROM saved_queries WHERE account = ? ORDER BY name LIMIT ?`, account, maxSavedPerAc)
	if err != nil {
		return nil, fmt.Errorf("cache: saved queries: %w", err)
	}
	defer rows.Close()
	out := []SavedQuery{}
	for rows.Next() {
		var (
			q    SavedQuery
			js   string
			made int64
		)
		if err := rows.Scan(&q.Name, &js, &q.Note, &made); err != nil {
			return nil, fmt.Errorf("cache: saved queries: %w", err)
		}
		_ = json.Unmarshal([]byte(js), &q.Filters)
		q.CreatedAt = time.Unix(made, 0).UTC()
		out = append(out, q)
	}
	return out, rows.Err()
}
