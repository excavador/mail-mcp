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
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
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

	folderQueries atomic.Int64 // membership lookups for folders of messages

	// now is the clock; tests replace it to exercise the full-scan interval.
	now func() time.Time
}

// folderColumns are columns of folders added after the first release. They
// are added to an existing database with ALTER TABLE, not by bumping
// schemaVersion: a bump drops the index and re-fetches every mailbox, and
// these columns are only an optimisation hint (0 means "never completed", so
// the folder is scanned once and the hint fills in).
var folderColumns = []struct{ name, def string }{
	{"uidnext", "INTEGER NOT NULL DEFAULT 0"},
	{"messages", "INTEGER NOT NULL DEFAULT 0"},
	{"scanned_at", "INTEGER NOT NULL DEFAULT 0"},
	{"attrs", "TEXT NOT NULL DEFAULT ''"},
}

// addMissingColumns adds, to a folders table created by an earlier version,
// the columns it lacks. Guarded by PRAGMA table_info so it is idempotent.
func addMissingColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(folders)`)
	if err != nil {
		return fmt.Errorf("inspect folders: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect folders: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("inspect folders: %w", err)
	}
	_ = rows.Close()
	for _, c := range folderColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE folders ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return fmt.Errorf("add folders.%s: %w", c.name, err)
		}
	}
	return nil
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
	gm_thread_id  TEXT    NOT NULL DEFAULT '',
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
	uidnext     INTEGER NOT NULL DEFAULT 0,
	messages    INTEGER NOT NULL DEFAULT 0,
	scanned_at  INTEGER NOT NULL DEFAULT 0,
	attrs       TEXT    NOT NULL DEFAULT '',
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

// schemaVersion is stored in PRAGMA user_version. The index is derived data
// (blobs are the source of truth and stay), so a different version is not
// migrated: the index tables are dropped and recreated, and the next refresh
// re-indexes from the server. Bump it whenever the schema or the meaning of a
// stable id changes.
const schemaVersion = 2

// indexTables are the tables the index owns, FTS first (dropping the virtual
// table removes its shadow tables).
var indexTables = []string{"message_fts", "messages", "membership", "folders", "refreshes"}

// ensureSchema creates the schema, first wiping the index tables when the
// database carries a different user_version (0 for a fresh or pre-versioned
// file). Blobs on disk are untouched.
func ensureSchema(db *sql.DB) error {
	var have int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if have != schemaVersion {
		existed := false
		for _, t := range indexTables {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, t).Scan(&n); err != nil {
				return fmt.Errorf("inspect schema: %w", err)
			}
			if n > 0 {
				existed = true
			}
			if _, err := db.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
				return fmt.Errorf("drop %s: %w", t, err)
			}
		}
		if existed {
			slog.Warn("cache: schema version changed, index discarded; it will be rebuilt by the next refresh (blobs kept)",
				"have", have, "want", schemaVersion)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if err := addMissingColumns(db); err != nil {
		return err
	}
	if have != schemaVersion {
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

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
	if err := ensureSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: initialise index: %w", err)
	}
	return &Cache{dir: dir, db: db, now: time.Now}, nil
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
