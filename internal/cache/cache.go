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
	"sync"
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

	fts2Ready atomic.Bool // the message_fts2 backfill is complete

	fts2Job, threadsJob jobState // in-process state of the backfill jobs
	sendersJob          jobState
	entitiesJob         jobState
	pdfJob              jobState
	pdfMu               sync.RWMutex
	pdfX                PDFExtractor   // nil: the feature is off
	pdfStage            string         // staging root; empty means the cache dir
	pdfAttempts         map[string]int // transport failures per file hash, across passes
	sendersMax          atomic.Int64   // messages with a higher rowid are counted into senders by refresh itself
	fgWriters           atomic.Int64   // refreshes and applies in progress; backfills give way
	ws                  writeStats

	folderQueries atomic.Int64 // membership lookups for folders of messages

	searches searchTracker // recent search results, for the search log

	ownerMu sync.RWMutex
	owners  map[string][]string // account -> the owner's own addresses

	// now is the clock; tests replace it to exercise the full-scan interval.
	now func() time.Time

	// Per-account refresh coordination (see refreshnow.go). bgCtx is the
	// parent of refreshes started on demand: they outlive the request that
	// started them, and Close cancels and waits for them.
	rfMu     sync.Mutex
	rf       map[string]*acctRefresh
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
}

// columnSet is a table's columns added after the first release. They are
// added to an existing database with ALTER TABLE, not by bumping
// schemaVersion: a bump drops the index and re-fetches every mailbox.
type columnSet struct {
	table string
	cols  []struct{ name, def string }
}

var addedColumns = []columnSet{
	// folders: only an optimisation hint (0 means "never completed", so the
	// folder is scanned once and the hint fills in).
	{"folders", []struct{ name, def string }{
		{"uidnext", "INTEGER NOT NULL DEFAULT 0"},
		{"messages", "INTEGER NOT NULL DEFAULT 0"},
		{"scanned_at", "INTEGER NOT NULL DEFAULT 0"},
		{"attrs", "TEXT NOT NULL DEFAULT ''"},
	}},
	// backfill: the full column list main created it with, as defence for a
	// database whose row was made by a narrower definition.
	{"backfill", []struct{ name, def string }{
		{"max_rowid", "INTEGER NOT NULL DEFAULT 0"},
		{"total", "INTEGER NOT NULL DEFAULT 0"},
		{"processed", "INTEGER NOT NULL DEFAULT 0"},
	}},
	{"message_thread", []struct{ name, def string }{
		{"outsider", "INTEGER NOT NULL DEFAULT 0"},
	}},
	// messages: the threading headers. NULL means "not read from the blob
	// yet" (the thread backfill fills it); '' means "read, there is none".
	{"messages", []struct{ name, def string }{
		{"message_id", "TEXT"},
		{"in_reply_to", "TEXT"},
		{"references_json", "TEXT"},
		// list_unsub: 1 when the message has a List-Unsubscribe header. NULL
		// means "not read from the blob yet" (the senders job fills it).
		{"list_unsub", "INTEGER"},
		// replied_counted: 1 once the message has been looked at as a possible
		// owner reply (senders.go), so it is credited at most once.
		{"replied_counted", "INTEGER NOT NULL DEFAULT 0"},
	}},
	{"senders", []struct{ name, def string }{
		{"n_auto", "INTEGER NOT NULL DEFAULT 0"},
	}},
}

// addMissingColumns adds, to tables created by an earlier version, the
// columns they lack. Guarded by PRAGMA table_info so it is idempotent.
func addMissingColumns(db *sql.DB) error {
	for _, set := range addedColumns {
		have, err := tableColumns(db, set.table)
		if err != nil {
			return err
		}
		for _, c := range set.cols {
			if have[c.name] {
				continue
			}
			if _, err := db.Exec(`ALTER TABLE ` + set.table + ` ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return fmt.Errorf("add %s.%s: %w", set.table, c.name, err)
			}
		}
	}
	return nil
}

// tableColumns returns the column names of a table.
func tableColumns(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("inspect %s: %w", table, err)
		}
		have[name] = true
	}
	return have, rows.Err()
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

-- Added after v0.2.6, additively (IF NOT EXISTS, no schemaVersion bump: a bump
-- discards the index and re-fetches every mailbox). message_fts2 splits the
-- body: body_new is the text with quotes, reply headers and signatures
-- stripped (CleanBody), body_full is the whole text, left empty when it equals
-- body_new so the common case is not stored twice. Its rowid is the rowid of
-- the messages row, so joins are O(1) and the backfill can tell what is done.
-- The original message_fts table is gone: it is no longer written or read, and
-- ensureSchema drops it from older files (dropLegacyFTS).
CREATE VIRTUAL TABLE IF NOT EXISTS message_fts2 USING fts5 (
	subject, from_addr, to_addr, cc_addr, body_new, body_full,
	account UNINDEXED, stable_id UNINDEXED,
	tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS attachments (
	account        TEXT    NOT NULL,
	stable_id      TEXT    NOT NULL,
	part           TEXT    NOT NULL,
	filename       TEXT    NOT NULL DEFAULT '',
	mime           TEXT    NOT NULL DEFAULT '',
	size           INTEGER NOT NULL DEFAULT 0,
	sha256         TEXT    NOT NULL DEFAULT '',
	is_inline      INTEGER NOT NULL DEFAULT 0,
	content_id     TEXT    NOT NULL DEFAULT '',
	text_extracted INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account, stable_id, part)
);
CREATE INDEX IF NOT EXISTS attachments_by_cid ON attachments (account, content_id) WHERE content_id <> '';

-- rowid = the attachments rowid of the part.
CREATE VIRTUAL TABLE IF NOT EXISTS attachment_fts USING fts5 (
	account UNINDEXED, stable_id UNINDEXED, part UNINDEXED, filename, text,
	tokenize = 'unicode61 remove_diacritics 2'
);

-- Progress of long background jobs over existing rows, resumable across
-- restarts. max_rowid is the highest messages rowid when the job was created:
-- later messages are indexed by refresh itself, so the job never races it.
CREATE TABLE IF NOT EXISTS backfill (
	name       TEXT PRIMARY KEY,
	last_rowid INTEGER NOT NULL DEFAULT 0,
	done       INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	max_rowid  INTEGER NOT NULL DEFAULT 0,
	total      INTEGER NOT NULL DEFAULT 0,
	processed  INTEGER NOT NULL DEFAULT 0
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

-- Threads. message_thread is derived data (rebuilt from the header columns of
-- messages); message_ref indexes every id a message mentions (its own
-- Message-ID, In-Reply-To, References, and a "subj:" key for header-less
-- replies) so that a late parent finds its children.
CREATE TABLE IF NOT EXISTS threads (
	account           TEXT    NOT NULL,
	tid               TEXT    NOT NULL,
	root_stable_id    TEXT    NOT NULL,
	subject_norm      TEXT    NOT NULL DEFAULT '',
	first_at          INTEGER NOT NULL DEFAULT 0,
	last_at           INTEGER NOT NULL DEFAULT 0,
	n_msgs            INTEGER NOT NULL DEFAULT 0,
	participants_json TEXT    NOT NULL DEFAULT '[]',
	PRIMARY KEY (account, tid)
);
CREATE INDEX IF NOT EXISTS threads_by_subject ON threads (account, subject_norm, first_at);

CREATE TABLE IF NOT EXISTS message_thread (
	account          TEXT    NOT NULL,
	stable_id        TEXT    NOT NULL,
	tid              TEXT    NOT NULL,
	parent_stable_id TEXT    NOT NULL DEFAULT '',
	depth            INTEGER NOT NULL DEFAULT 0,
	outsider         INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account, stable_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS message_thread_by_tid ON message_thread (account, tid);

CREATE TABLE IF NOT EXISTS message_ref (
	account   TEXT NOT NULL,
	ref_id    TEXT NOT NULL,
	stable_id TEXT NOT NULL,
	PRIMARY KEY (account, ref_id, stable_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS message_ref_by_msg ON message_ref (account, stable_id);

-- One row per search call; see searchlog.go. Not derived from messages, so it
-- is not dropped with the index.
CREATE TABLE IF NOT EXISTS search_log (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	at              INTEGER NOT NULL,
	account         TEXT    NOT NULL DEFAULT '',
	query_hash      TEXT    NOT NULL DEFAULT '',
	query           TEXT    NOT NULL DEFAULT '',
	mode            TEXT    NOT NULL DEFAULT '',
	hits            INTEGER NOT NULL DEFAULT 0,
	total           INTEGER NOT NULL DEFAULT 0,
	fetched         INTEGER NOT NULL DEFAULT 0,
	reformulated_of INTEGER
);
CREATE INDEX IF NOT EXISTS search_log_by_at ON search_log (at);

-- Senders, tags and saved queries (added after v0.3.1, additively, no
-- schemaVersion bump). senders is derived from messages (see senders.go) except
-- for kind_source = 'owner', which is the owner's decision. tags and
-- saved_queries are the owner's own data: local only, never written to the
-- mailbox, and never dropped with the index.
CREATE TABLE IF NOT EXISTS senders (
	account              TEXT    NOT NULL,
	addr                 TEXT    NOT NULL,
	domain               TEXT    NOT NULL DEFAULT '',
	display_names_json   TEXT    NOT NULL DEFAULT '[]',
	counts_json          TEXT    NOT NULL DEFAULT '{}',
	first_at             INTEGER NOT NULL DEFAULT 0,
	last_at              INTEGER NOT NULL DEFAULT 0,
	n_msgs               INTEGER NOT NULL DEFAULT 0,
	n_from_me            INTEGER NOT NULL DEFAULT 0,
	n_to_me              INTEGER NOT NULL DEFAULT 0,
	n_replied_by_me      INTEGER NOT NULL DEFAULT 0,
	list_id              TEXT    NOT NULL DEFAULT '',
	has_list_unsubscribe INTEGER NOT NULL DEFAULT 0,
	n_list               INTEGER NOT NULL DEFAULT 0,
	n_unsub              INTEGER NOT NULL DEFAULT 0,
	n_gh                 INTEGER NOT NULL DEFAULT 0,
	n_txn_subj           INTEGER NOT NULL DEFAULT 0,
	n_auto               INTEGER NOT NULL DEFAULT 0,
	kind                 TEXT    NOT NULL DEFAULT 'human',
	kind_source          TEXT    NOT NULL DEFAULT 'rule',
	kind_updated_at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account, addr)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS senders_by_kind ON senders (account, kind, n_msgs);
CREATE INDEX IF NOT EXISTS senders_by_last ON senders (account, last_at);

CREATE TABLE IF NOT EXISTS tags (
	account    TEXT    NOT NULL,
	stable_id  TEXT    NOT NULL,
	tag        TEXT    NOT NULL,
	added_at   INTEGER NOT NULL,
	history_id TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (account, stable_id, tag)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS tags_by_tag ON tags (account, tag, added_at);

CREATE TABLE IF NOT EXISTS saved_queries (
	account    TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	query_json TEXT    NOT NULL,
	note       TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account, name)
) WITHOUT ROWID;

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

-- Added after v1.1, additively (IF NOT EXISTS, no schemaVersion bump). Text
-- extracted from PDF attachments by the sidecar (see pdftext.go), keyed by the
-- SHA-256 of the decoded attachment so the same file forwarded a hundred times
-- is extracted once. status is the sidecar's: ok, timeout, too_large, failed,
-- not_pdf. A row is written for every outcome, so a hostile file is never
-- retried.
CREATE TABLE IF NOT EXISTS pdf_text (
	sha256       TEXT    PRIMARY KEY,
	status       TEXT    NOT NULL,
	text         TEXT    NOT NULL DEFAULT '',
	truncated    INTEGER NOT NULL DEFAULT 0,
	pages_capped INTEGER NOT NULL DEFAULT 0,
	updated_at   INTEGER NOT NULL DEFAULT 0,
	-- strikes: how many times a provisional (status pending) row was found
	-- again after the process died mid-file; the third makes the file failed.
	strikes      INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// schemaVersion is stored in PRAGMA user_version. The index is derived data
// (blobs are the source of truth and stay), so a different version is not
// migrated: the index tables are dropped and recreated, and the next refresh
// re-indexes from the server. Bump it whenever the schema or the meaning of a
// stable id changes.
//
// WARNING: bumping this makes the NEW pod drop the index tables the OLD pod is
// still serving from during a RollingUpdate (and a rollback does the same the
// other way). Deploy the release that bumps it with the chart's
// forceRecreate: true (strategy Recreate), then switch it back.
const schemaVersion = 2

// indexTables are the tables the index owns, FTS first (dropping the virtual
// table removes its shadow tables).
var indexTables = []string{"message_fts2", "attachment_fts", "attachments", "backfill", "messages", "membership", "folders", "refreshes", "threads", "message_thread", "message_ref", "pdf_text"}

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
	if err := dropLegacyFTS(db); err != nil {
		return err
	}
	if have != schemaVersion {
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

// dropLegacyFTS removes the original message_fts table (and its shadow tables)
// from a file written before it was retired. It is idempotent and needs no
// schemaVersion bump: message_fts2 holds everything it did. The freed pages go
// to SQLite's freelist and are reused by later writes; the file does not shrink
// (auto_vacuum is off, and a VACUUM at startup would block on a large file).
func dropLegacyFTS(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'message_fts'`).Scan(&n); err != nil {
		return fmt.Errorf("inspect message_fts: %w", err)
	}
	if n == 0 {
		return nil
	}
	slog.Info("dropping legacy message_fts")
	start := time.Now()
	if _, err := db.Exec(`DROP TABLE IF EXISTS message_fts`); err != nil {
		return fmt.Errorf("drop message_fts: %w", err)
	}
	slog.Info("dropped legacy message_fts", "duration_ms", time.Since(start).Milliseconds())
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
	c := &Cache{dir: dir, db: db, now: time.Now}
	c.bgCtx, c.bgCancel = context.WithCancel(context.Background())
	if err := c.initBackfill(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: initialise index: %w", err)
	}
	if err := c.initSenders(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: initialise index: %w", err)
	}
	if err := c.initEntities(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: initialise index: %w", err)
	}
	return c, nil
}

// Close releases the index.
func (c *Cache) Close() error {
	if c.bgCancel != nil {
		c.bgCancel()
		c.bgWG.Wait()
	}
	return c.db.Close()
}

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
