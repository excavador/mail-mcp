// Package cache keeps an immutable local copy of every message mail-mcp has
// seen, so that searching never has to go back to the mailbox.
//
// The shape follows from one fact: a message does not change, but where it
// lives does. Gmail labels, Proton folders and a "move" all rewrite location
// and leave the bytes alone. So the cache is split along that line:
//
//	blobs/    sha256(raw RFC 822) -> bytes        write-once, never rewritten
//	messages  (account, stable_id) -> blob, headers, FTS5 over text
//	membership(account, folder, uid) -> stable_id  the ONLY mutable table
//
// Everything a refresh learns about "where is this message now" lands in
// membership; blobs and the message index only ever grow. A message that
// leaves a folder loses its membership row and keeps its blob and index entry,
// which is what lets history and undo (and search over mail you archived)
// work without asking the server.
//
// The cache is read-only with respect to the mailbox: folders are opened with
// EXAMINE and bodies fetched with BODY.PEEK, so refreshing never marks a
// message read or touches any flag.
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite (with FTS5): the binary is CGO_ENABLED=0 on a distroless
	// static image, so a cgo driver cannot be linked.
	_ "modernc.org/sqlite"
)

// Cache is the on-disk store. It is safe for concurrent use; refreshes of
// different accounts may run at the same time.
type Cache struct {
	dir string
	db  *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	account       TEXT    NOT NULL,
	stable_id     TEXT    NOT NULL,
	blob_sha256   TEXT    NOT NULL,
	from_addr     TEXT    NOT NULL DEFAULT '',
	to_addr       TEXT    NOT NULL DEFAULT '',
	cc_addr       TEXT    NOT NULL DEFAULT '',
	subject       TEXT    NOT NULL DEFAULT '',
	date_unix     INTEGER NOT NULL DEFAULT 0,
	list_id       TEXT    NOT NULL DEFAULT '',
	gh_reason     TEXT    NOT NULL DEFAULT '',
	size          INTEGER NOT NULL DEFAULT 0,
	internal_date INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account, stable_id)
);
CREATE INDEX IF NOT EXISTS messages_by_blob ON messages (blob_sha256);

CREATE VIRTUAL TABLE IF NOT EXISTS message_fts USING fts5 (
	subject, from_addr, to_addr, cc_addr, body,
	account UNINDEXED, stable_id UNINDEXED
);

CREATE TABLE IF NOT EXISTS membership (
	account     TEXT    NOT NULL,
	stable_id   TEXT    NOT NULL,
	folder      TEXT    NOT NULL,
	uid         INTEGER NOT NULL,
	uidvalidity INTEGER NOT NULL,
	PRIMARY KEY (account, folder, uid)
);
CREATE INDEX IF NOT EXISTS membership_by_id ON membership (account, stable_id);

CREATE TABLE IF NOT EXISTS folders (
	account     TEXT    NOT NULL,
	folder      TEXT    NOT NULL,
	uidvalidity INTEGER NOT NULL,
	PRIMARY KEY (account, folder)
);

CREATE TABLE IF NOT EXISTS refreshes (
	account    TEXT PRIMARY KEY,
	at         INTEGER NOT NULL,
	ok         INTEGER NOT NULL,
	folders    INTEGER NOT NULL DEFAULT 0,
	new_uids   INTEGER NOT NULL DEFAULT 0,
	new_ids    INTEGER NOT NULL DEFAULT 0,
	new_bodies INTEGER NOT NULL DEFAULT 0,
	removed    INTEGER NOT NULL DEFAULT 0
);
`

// Open creates dir if needed and opens (or initialises) the cache in it.
func Open(dir string) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("cache: directory is empty")
	}
	blobs := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		return nil, fmt.Errorf("cache: create directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone and is subject to
	// the umask; the cache holds mail, so make it owner-only regardless.
	for _, d := range []string{dir, blobs} {
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("cache: restrict directory: %w", err)
		}
	}
	sweepTemp(blobs)
	// WAL so a status read never waits on a refresh; busy_timeout and an
	// immediate txlock so two accounts refreshing at once queue for the
	// write lock instead of failing with SQLITE_BUSY mid-transaction.
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Opaque: url.PathEscape(filepath.Join(dir, "index.db")), RawQuery: q.Encode()}
	dsn := u.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: open index: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: initialise index: %w", err)
	}
	return &Cache{dir: dir, db: db}, nil
}

// Close releases the index.
func (c *Cache) Close() error { return c.db.Close() }

// AccountStatus summarises what the cache holds for one account.
type AccountStatus struct {
	Account     string     `json:"account"`
	Messages    int        `json:"messages"`
	Memberships int        `json:"memberships"`
	Folders     int        `json:"folders"`
	LastRefresh *time.Time `json:"last_refresh,omitempty"`
	LastOK      bool       `json:"last_refresh_ok"`
}

// Status reports per-account counts and the time of the last refresh, for
// every account that has ever been refreshed or has cached messages.
func (c *Cache) Status(ctx context.Context) ([]AccountStatus, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT a.account,
       (SELECT COUNT(*) FROM messages   m WHERE m.account = a.account),
       (SELECT COUNT(*) FROM membership s WHERE s.account = a.account),
       (SELECT COUNT(*) FROM folders    f WHERE f.account = a.account),
       COALESCE(r.at, 0), COALESCE(r.ok, 0)
FROM (SELECT account FROM messages UNION SELECT account FROM refreshes) a
LEFT JOIN refreshes r ON r.account = a.account
ORDER BY a.account`)
	if err != nil {
		return nil, fmt.Errorf("cache: status: %w", err)
	}
	defer rows.Close()

	var out []AccountStatus
	for rows.Next() {
		var (
			s  AccountStatus
			at int64
			ok int
		)
		if err := rows.Scan(&s.Account, &s.Messages, &s.Memberships, &s.Folders, &at, &ok); err != nil {
			return nil, fmt.Errorf("cache: status: %w", err)
		}
		if at != 0 {
			t := time.Unix(at, 0).UTC()
			s.LastRefresh = &t
		}
		s.LastOK = ok == 1
		out = append(out, s)
	}
	return out, rows.Err()
}
