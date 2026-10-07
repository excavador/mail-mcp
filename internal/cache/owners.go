package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// ownerMatcher answers "is this address the owner's own?" for one account.
// Entries are exact lowercase addresses and "@domain" patterns (any local part
// at that domain). A plus-tagged address ("user+tag@host") also matches the
// entry for its base address ("user@host"); a tagged entry matches only itself.
type ownerMatcher struct {
	exact   map[string]bool
	domains map[string]bool
}

func newOwnerMatcher(entries []string) *ownerMatcher {
	m := &ownerMatcher{exact: map[string]bool{}, domains: map[string]bool{}}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
		case strings.HasPrefix(e, "@"):
			m.domains[e[1:]] = true
		default:
			m.exact[e] = true
		}
	}
	return m
}

func (m *ownerMatcher) match(addr string) bool {
	if m == nil || addr == "" {
		return false
	}
	addr = strings.ToLower(strings.TrimSpace(addr))
	if m.exact[addr] {
		return true
	}
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 {
		return false
	}
	local, domain := addr[:at], addr[at+1:]
	if m.domains[domain] {
		return true
	}
	if plus := strings.IndexByte(local, '+'); plus > 0 {
		return m.exact[local[:plus]+"@"+domain]
	}
	return false
}

// ownersFingerprint hashes every account's owner set (names and entries,
// sorted), so any change to a username or alias changes it.
func ownersFingerprint(byAccount map[string][]string) string {
	accts := make([]string, 0, len(byAccount))
	for a := range byAccount {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	h := sha256.New()
	for _, a := range accts {
		es := append([]string(nil), byAccount[a]...)
		sort.Strings(es)
		_, _ = fmt.Fprintf(h, "%s\x00%s\x01", a, strings.Join(es, "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

const (
	// ownersStartedPrefix + fingerprint: the owner-derived data is being
	// recounted for that owner set (resumed after a restart).
	ownersStartedPrefix = "owners_fp_started:"
	// ownersDonePrefix + fingerprint: the recount finished; the data matches
	// that owner set. Written in the same transaction as the senders recount
	// marker, so it never claims more than was done.
	ownersDonePrefix = "owners_fp_done:"
)

// ReconcileOwners compares the owner set given to SetOwners (usernames and
// aliases) with the one the owner-derived data was last computed with, and when
// they differ restarts the senders recount: n_replied_by_me, replied_counted
// and the senders kinds that rest on the owner-reply rule are rebuilt by the
// background senders job (RunBackfills), and the outsider flag of messages sent
// from an owner address is cleared. Synchronously it does only what the
// v0.5.3 recount start does (reset counts, restart the job, write markers) and
// a cursor row for the outsider clear; the counting and the outsider clear run
// in the background job (RunSenders). Call it after SetOwners and before
// RunBackfills. An unchanged set does nothing; an interrupted recount for the
// same set resumes.
func (c *Cache) ReconcileOwners(log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	c.ownerMu.RLock()
	by := make(map[string][]string, len(c.owners))
	for a, es := range c.owners {
		by[a] = es
	}
	c.ownerMu.RUnlock()
	if len(by) == 0 {
		return nil
	}
	fp := ownersFingerprint(by)
	has := func(name string) (bool, error) {
		var n int
		err := c.db.QueryRow(`SELECT COUNT(*) FROM backfill WHERE name = ?`, name).Scan(&n)
		return n > 0, err
	}
	if ok, err := has(ownersDonePrefix + fp); err != nil || ok {
		return err
	}
	if ok, err := has(ownersStartedPrefix + fp); err != nil {
		return err
	} else if ok {
		log.Info("owner recount resumes after a restart", "owners_fingerprint", fp)
		return nil
	}
	var msgs int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs); err != nil {
		return fmt.Errorf("owners: %w", err)
	}
	now := c.now().Unix()
	if msgs == 0 {
		// Nothing was derived from any owner set yet: record the set.
		tx, err := c.db.Begin()
		if err != nil {
			return fmt.Errorf("owners: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := dropOwnerMarkers(tx); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO backfill (name, last_rowid, done, updated_at) VALUES (?, 0, 1, ?)`, ownersDonePrefix+fp, now); err != nil {
			return fmt.Errorf("owners: %w", err)
		}
		return tx.Commit()
	}
	counts := map[string]int{}
	for a, es := range by {
		counts[a] = len(es)
	}
	log.Info("owner set changed: owner-derived data is recounted in the background (counts reset, owner and llm sender kinds kept)",
		"owners_fingerprint", fp, "addresses_per_account", fmt.Sprint(counts), "messages", msgs)
	return c.restartSendersRecount(false, func(tx *sql.Tx) error {
		if err := dropOwnerMarkers(tx); err != nil {
			return err
		}
		// The recount marker goes, so the job's end writes it again.
		if _, err := tx.Exec(`DELETE FROM backfill WHERE name = ?`, sendersRecount); err != nil {
			return fmt.Errorf("owners: %w", err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO backfill (name, last_rowid, done, updated_at) VALUES (?, 0, 1, ?)`, ownersStartedPrefix+fp, now); err != nil {
			return fmt.Errorf("owners: %w", err)
		}
		// The outsider clear is background work with a cursor (see
		// RunOwnerOutsiders); only its cursor row is created here.
		if _, err := tx.Exec(`INSERT OR REPLACE INTO backfill (name, last_rowid, done, updated_at) VALUES (?, 0, 0, ?)`, ownersOutsidersCursor, now); err != nil {
			return fmt.Errorf("owners: %w", err)
		}
		return nil
	})
}

func dropOwnerMarkers(tx *sql.Tx) error {
	// Prefix compared with substr, not LIKE: "_" is a wildcard in LIKE.
	if _, err := tx.Exec(`DELETE FROM backfill WHERE substr(name, 1, ?) = ? OR substr(name, 1, ?) = ? OR name = ?`,
		len(ownersStartedPrefix), ownersStartedPrefix, len(ownersDonePrefix), ownersDonePrefix, ownersOutsidersCursor); err != nil {
		return fmt.Errorf("owners: %w", err)
	}
	return nil
}

// ownersOutsidersCursor is the backfill row of the outsider clear: last_rowid
// is the highest messages.rowid looked at, done = 1 when it has finished.
const ownersOutsidersCursor = "owners_outsiders_cursor"

// ownerOutsiderBatch is how many flagged messages one transaction looks at.
var ownerOutsiderBatch = 2000

// ownerOutsiderStep looks at the next batch of outsider-flagged messages after
// the cursor, clears the flag of those sent from an owner address and advances
// the cursor, in one short transaction. more is false when the clear is done
// (or was never scheduled).
func (c *Cache) ownerOutsiderStep(ctx context.Context) (more bool, err error) {
	var cursor int64
	var done int
	err = c.db.QueryRowContext(ctx, `SELECT last_rowid, done FROM backfill WHERE name = ?`, ownersOutsidersCursor).Scan(&cursor, &done)
	if err == sql.ErrNoRows || (err == nil && done == 1) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("owners: outsiders: %w", err)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("owners: outsiders: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT m.rowid, t.account, t.stable_id, m.from_addr FROM message_thread t
JOIN messages m ON m.account = t.account AND m.stable_id = t.stable_id
WHERE t.outsider = 1 AND m.rowid > ? ORDER BY m.rowid LIMIT ?`, cursor, ownerOutsiderBatch)
	if err != nil {
		return false, fmt.Errorf("owners: outsiders: %w", err)
	}
	type key struct{ account, id string }
	var clear []key
	matchers := map[string]*ownerMatcher{}
	n := 0
	last := cursor
	for rows.Next() {
		var rid int64
		var a, id, from string
		if err := rows.Scan(&rid, &a, &id, &from); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("owners: outsiders: %w", err)
		}
		n++
		last = rid
		m, ok := matchers[a]
		if !ok {
			m = newOwnerMatcher(c.threadOpts(a).Owners)
			matchers[a] = m
		}
		if addr, _ := parseFrom(from); m.match(addr) {
			clear = append(clear, key{a, id})
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, fmt.Errorf("owners: outsiders: %w", err)
	}
	for _, k := range clear {
		if _, err := tx.ExecContext(ctx, `UPDATE message_thread SET outsider = 0 WHERE account = ? AND stable_id = ?`, k.account, k.id); err != nil {
			return false, fmt.Errorf("owners: outsiders: %w", err)
		}
	}
	finished := n < ownerOutsiderBatch
	if _, err := tx.ExecContext(ctx, `UPDATE backfill SET last_rowid = ?, done = ?, updated_at = ? WHERE name = ?`,
		last, flagInt(finished), c.now().Unix(), ownersOutsidersCursor); err != nil {
		return false, fmt.Errorf("owners: outsiders: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("owners: outsiders: %w", err)
	}
	return !finished, nil
}

// RunOwnerOutsiders is the background half of ReconcileOwners: it clears the
// outsider flag of messages sent from an owner address (the owner is never an
// outsider in their own thread), in batches, resumable by its cursor. It runs
// before the senders recount can finish, so the owner-derived done marker means
// both are complete. Adding an alias can only clear flags; a removed alias
// leaves existing flags as they were.
func (c *Cache) RunOwnerOutsiders(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	for {
		more, err := c.ownerOutsiderStep(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// finishOwnersTx promotes the started markers to done ones; it runs in the
// transaction that writes the senders recount marker.
func finishOwnersTx(ctx context.Context, tx *sql.Tx) (int, error) {
	res, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO backfill (name, last_rowid, done, updated_at)
SELECT ? || substr(name, ?), 0, 1, updated_at FROM backfill WHERE substr(name, 1, ?) = ?`,
		ownersDonePrefix, len(ownersStartedPrefix)+1, len(ownersStartedPrefix), ownersStartedPrefix)
	if err != nil {
		return 0, fmt.Errorf("owners: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM backfill WHERE substr(name, 1, ?) = ?`, len(ownersStartedPrefix), ownersStartedPrefix); err != nil {
		return 0, fmt.Errorf("owners: %w", err)
	}
	return int(n), nil
}
