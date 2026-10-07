package cache

// The senders table: one row per (account, bare lowercase address) with what
// the mailbox says about that sender (how much mail, how much of it you
// answered, which kind of sender it is).
//
// Maintenance, so that every message is counted exactly once:
//
//   - The senders job (RunSenders, last in the backfill scheduler) counts the
//     messages that existed when the job row was created, rowid <= max_rowid,
//     in rowid order, each batch in one short write together with its cursor.
//   - Refresh counts the messages it inserts afterwards (rowid > max_rowid),
//     in the same transaction that inserts them.
//
// The two sets are disjoint, so neither can double count nor lose a message.
// Counting is additive: a row is read, merged with the batch's delta and
// written back; the kind is then recomputed from the counts unless the owner
// set it.
//
// n_replied_by_me: a From header is forgeable, so a message counts as the
// owner's reply only when it is a member of the account's Sent folder (the
// \Sent special-use folder, else "[Gmail]/Sent Mail" or "Sent") AND its From is
// an owner address. creditSentReplies finds those messages (messages.
// replied_counted = 0), takes the parent the reply names (In-Reply-To, else the
// last id of References), looks it up through message_ref restricted to the
// message whose own Message-ID is that id, and credits the parent's sender
// +1. It credits nobody unless exactly one distinct sender has that
// Message-ID in the account (a reused id is ambiguous) and the parent arrived
// before the reply. Every message looked at is marked counted, so it is
// examined once: a reply whose parent is not cached yet credits nobody. There
// is no thread-level fallback: it would credit every participant of a
// mailing-list thread.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"net/textproto"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	sendersJobName = "senders"
	maxNameCounts  = 8
	sendersBatch   = 200
)

func flagInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// parseFrom returns the bare lowercase address and display name of the first
// address in a From header value.
func parseFrom(raw string) (addr, name string) {
	addr, name = parseFromRaw(raw)
	if !validSenderAddr(addr) {
		return "", ""
	}
	return addr, name
}

// maxAddrBytes is the longest sender address stored (RFC 5321's limit).
const maxAddrBytes = 320

// validSenderAddr accepts what a stored address may be: valid UTF-8 of at most
// maxAddrBytes with no control, format or space characters and none of the
// header punctuation < > , ; " (so it round-trips through BareAddr and the tools).
func validSenderAddr(a string) bool {
	if a == "" || len(a) > maxAddrBytes || !utf8.ValidString(a) {
		return false
	}
	for _, r := range a {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) || strings.ContainsRune("<>,;\"", r) {
			return false
		}
	}
	return true
}

func parseFromRaw(raw string) (addr, name string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if a, err := mail.ParseAddress(raw); err == nil {
		return strings.ToLower(a.Address), strings.TrimSpace(a.Name)
	}
	if l, err := mail.ParseAddressList(raw); err == nil && len(l) > 0 {
		return strings.ToLower(l[0].Address), strings.TrimSpace(l[0].Name)
	}
	return BareAddr(raw), ""
}

// bareListID reduces "Name <id>" or "id" to the lowercase id.
func bareListID(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			s = s[i+1 : i+j]
		}
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// senderDelta is what a batch of messages adds to one sender.
type senderDelta struct {
	names, lists             map[string]int
	first, last              int64
	n, fromMe, toMe, replied int
	nList, nUnsub, nGH, nTxn int
	nAuto                    int
}

type senderKey struct{ account, addr string }

// senderBatch accumulates deltas for a set of messages and writes them in one
// transaction (flushTx).
type senderBatch struct {
	c      *Cache
	deltas map[senderKey]*senderDelta
	owners map[string][]string
}

func (c *Cache) newSenderBatch() *senderBatch {
	return &senderBatch{c: c, deltas: map[senderKey]*senderDelta{}, owners: map[string][]string{}}
}

func (b *senderBatch) ownersOf(account string) []string {
	if o, ok := b.owners[account]; ok {
		return o
	}
	o := b.c.threadOpts(account).Owners
	b.owners[account] = o
	return o
}

func (b *senderBatch) isOwner(account, addr string) bool {
	for _, o := range b.ownersOf(account) {
		if o == addr {
			return true
		}
	}
	return false
}

func (b *senderBatch) get(account, addr string) *senderDelta {
	k := senderKey{account, addr}
	d := b.deltas[k]
	if d == nil {
		d = &senderDelta{names: map[string]int{}, lists: map[string]int{}}
		b.deltas[k] = d
	}
	return d
}

// observe counts a message refresh just inserted, if it is newer than the
// senders job's snapshot (the job counts the older ones).
func (b *senderBatch) observe(account string, rid int64, info headerInfo, p parsed) {
	if rid <= b.c.sendersMax.Load() {
		return
	}
	var date int64
	if !p.Date.IsZero() {
		date = p.Date.Unix()
	}
	b.add(account, p.From, p.To, p.Subject, date, info.internal.Unix(), p.ListID, p.ListUnsub, p.GitHubReason != "")
}

// add counts one message.
func (b *senderBatch) add(account, from, to, subject string, date, internal int64, listID string, unsub, gh bool) {
	addr, name := parseFrom(from)
	if addr == "" {
		return
	}
	d := b.get(account, addr)
	d.n++
	if name != "" {
		d.names[name]++
	}
	if at := firstPos(internal, date); at > 0 {
		if d.first == 0 || at < d.first {
			d.first = at
		}
		d.last = max(d.last, at)
	}
	if id := bareListID(listID); id != "" {
		d.nList++
		d.lists[id]++
	}
	if unsub {
		d.nUnsub++
	}
	if gh {
		d.nGH++
	}
	if IsTransactionalSubject(subject) {
		d.nTxn++
	}
	if listID != "" && bareListID(listID) != "" || unsub || gh || noreplyRE.MatchString(localPart(addr)) {
		d.nAuto++
	}
	if b.isOwner(account, addr) {
		d.fromMe++
		return
	}
	for _, ta := range toAddrs(to) {
		if slices.Contains(b.ownersOf(account), ta) {
			d.toMe++
			break
		}
	}
}

// toAddrs is the bare lowercase addresses of a To header, compared exactly.
func toAddrs(to string) []string {
	to = strings.TrimSpace(to)
	if to == "" {
		return nil
	}
	var out []string
	if l, err := mail.ParseAddressList(to); err == nil {
		for _, a := range l {
			out = append(out, strings.ToLower(a.Address))
		}
		return out
	}
	for _, part := range strings.Split(to, ",") {
		if a := BareAddr(part); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func firstPos(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

// parentSender is the sender of the message a reply answers: In-Reply-To,
// else the last Reference. It is "" unless exactly one distinct sender has
// that Message-ID in the account and the earliest copy arrived before
// replyAt, so a reused Message-ID credits nobody.
func parentSender(ctx context.Context, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, account, inReplyTo, refsJSON string, replyAt int64) (string, error) {
	id := inReplyTo
	if id == "" {
		var refs []string
		if json.Unmarshal([]byte(refsJSON), &refs) == nil && len(refs) > 0 {
			id = refs[len(refs)-1]
		}
	}
	if id == "" {
		return "", nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT DISTINCT p.stable_id, p.from_addr, p.internal_date, p.date_unix
FROM message_ref r JOIN messages p ON p.account = r.account AND p.stable_id = r.stable_id
WHERE r.account = ? AND r.ref_id = ? AND p.message_id = ? LIMIT 5`, account, id, id)
	if err != nil {
		return "", fmt.Errorf("senders: find parent: %w", err)
	}
	defer rows.Close()
	senders := map[string]bool{}
	earliest := int64(0)
	for rows.Next() {
		var (
			sid, from        string
			internal, dateAt int64
		)
		if err := rows.Scan(&sid, &from, &internal, &dateAt); err != nil {
			return "", fmt.Errorf("senders: find parent: %w", err)
		}
		addr, _ := parseFrom(from)
		senders[addr] = true
		if at := firstPos(internal, dateAt); earliest == 0 || at < earliest {
			earliest = at
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("senders: find parent: %w", err)
	}
	if len(senders) != 1 || earliest <= 0 || replyAt <= 0 || earliest >= replyAt {
		return "", nil
	}
	for a := range senders {
		return a, nil
	}
	return "", nil
}

// sentFolders are the account's Sent folders: those with the \Sent attribute,
// else the conventional names.
func (c *Cache) sentFolders(ctx context.Context, account string) ([]string, error) {
	var out []string
	for _, q := range []string{
		`SELECT folder FROM folders WHERE account = ? AND (' ' || attrs || ' ') LIKE '% \Sent %'`,
		`SELECT folder FROM folders WHERE account = ? AND folder IN ('[Gmail]/Sent Mail', 'Sent')`,
	} {
		rows, err := c.db.QueryContext(ctx, q, account)
		if err != nil {
			return nil, fmt.Errorf("senders: sent folder: %w", err)
		}
		for rows.Next() {
			var f string
			if err := rows.Scan(&f); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("senders: sent folder: %w", err)
			}
			out = append(out, f)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("senders: sent folder: %w", err)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, nil
}

// creditSentReplies credits n_replied_by_me for the account's messages that
// are in a Sent folder, are from an owner address and have not been looked at
// yet. Bounded batches, each its own short transaction.
func (c *Cache) creditSentReplies(ctx context.Context, account string) error {
	owners := c.threadOpts(account).Owners
	if len(owners) == 0 {
		return nil
	}
	sent, err := c.sentFolders(ctx, account)
	if err != nil || len(sent) == 0 {
		return err
	}
	where := `s.account = ? AND s.folder IN (` + tagPlaceholders(len(sent)) + `) AND m.replied_counted = 0`
	args := []any{account}
	for _, f := range sent {
		args = append(args, f)
	}
	for {
		rows, err := c.db.QueryContext(ctx, `
SELECT DISTINCT m.rowid, m.from_addr, COALESCE(m.in_reply_to, ''), COALESCE(m.references_json, ''), m.internal_date, m.date_unix
FROM membership s JOIN messages m ON m.account = s.account AND m.stable_id = s.stable_id
WHERE `+where+` LIMIT ?`, append(append([]any{}, args...), sendersBatch)...)
		if err != nil {
			return fmt.Errorf("senders: sent replies: %w", err)
		}
		type cand struct {
			rid                int64
			from, irt, refs    string
			internal, dateUnix int64
		}
		var cs []cand
		for rows.Next() {
			var x cand
			if err := rows.Scan(&x.rid, &x.from, &x.irt, &x.refs, &x.internal, &x.dateUnix); err != nil {
				_ = rows.Close()
				return fmt.Errorf("senders: sent replies: %w", err)
			}
			cs = append(cs, x)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("senders: sent replies: %w", err)
		}
		if len(cs) == 0 {
			return nil
		}
		sb := c.newSenderBatch()
		for _, x := range cs {
			from, _ := parseFrom(x.from)
			if from == "" || !sb.isOwner(account, from) || (x.irt == "" && (x.refs == "" || x.refs == "[]")) {
				continue
			}
			parent, err := parentSender(ctx, c.db, account, x.irt, x.refs, firstPos(x.internal, x.dateUnix))
			if err != nil {
				return err
			}
			if parent != "" && !sb.isOwner(account, parent) {
				sb.get(account, parent).replied++
			}
		}
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("senders: sent replies: %w", err)
		}
		if err := func() error {
			defer func() { _ = tx.Rollback() }()
			if err := sb.flushTx(ctx, tx); err != nil {
				return err
			}
			for _, x := range cs {
				if _, err := tx.ExecContext(ctx, `UPDATE messages SET replied_counted = 1 WHERE rowid = ?`, x.rid); err != nil {
					return err
				}
			}
			return tx.Commit()
		}(); err != nil {
			return fmt.Errorf("senders: sent replies: %w", err)
		}
		if len(cs) < sendersBatch {
			return nil
		}
	}
}

type sendersCounts struct {
	Names map[string]int `json:"names,omitempty"`
	Lists map[string]int `json:"lists,omitempty"`
}

func trimCounts(m map[string]int) map[string]int {
	if len(m) <= maxNameCounts {
		return m
	}
	for _, k := range topKeys(m, len(m))[maxNameCounts:] {
		delete(m, k)
	}
	return m
}

// flushTx merges every delta into its senders row and writes it. It reads and writes only through tx.
func (b *senderBatch) flushTx(ctx context.Context, tx *sql.Tx) error {
	keys := make([]senderKey, 0, len(b.deltas))
	for k := range b.deltas {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].addr < keys[j].addr
	})
	now := b.c.now().Unix()
	for _, k := range keys {
		if err := mergeSenderTx(ctx, tx, k, b.deltas[k], b.isOwner(k.account, k.addr), now); err != nil {
			return err
		}
	}
	b.deltas = map[senderKey]*senderDelta{}
	return nil
}

func mergeSenderTx(ctx context.Context, tx *sql.Tx, k senderKey, d *senderDelta, owner bool, now int64) error {
	var (
		domain, namesJSON, countsJSON, listID, kind, source       string
		first, last, kindAt                                       int64
		n, fromMe, toMe, replied, nList, nUnsub, nGH, nTxn, nAuto int
		unsub                                                     int
	)
	err := tx.QueryRowContext(ctx, `
SELECT domain, display_names_json, counts_json, first_at, last_at, n_msgs, n_from_me, n_to_me, n_replied_by_me,
       list_id, has_list_unsubscribe, n_list, n_unsub, n_gh, n_txn_subj, n_auto, kind, kind_source, kind_updated_at
FROM senders WHERE account = ? AND addr = ?`, k.account, k.addr).Scan(
		&domain, &namesJSON, &countsJSON, &first, &last, &n, &fromMe, &toMe, &replied,
		&listID, &unsub, &nList, &nUnsub, &nGH, &nTxn, &nAuto, &kind, &source, &kindAt)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("senders: read row: %w", err)
	}
	if !exists {
		domain, kind, source, kindAt = RegistrableDomain(k.addr), KindHuman, SourceRule, now
	}
	var counts sendersCounts
	if exists {
		_ = json.Unmarshal([]byte(countsJSON), &counts)
	}
	if counts.Names == nil {
		counts.Names = map[string]int{}
	}
	if counts.Lists == nil {
		counts.Lists = map[string]int{}
	}
	for s, v := range d.names {
		counts.Names[s] += v
	}
	for s, v := range d.lists {
		counts.Lists[s] += v
	}
	trimCounts(counts.Names)
	trimCounts(counts.Lists)
	if d.first > 0 && (first == 0 || d.first < first) {
		first = d.first
	}
	last = max(last, d.last)
	n += d.n
	toMe += d.toMe
	replied += d.replied
	nList += d.nList
	nUnsub += d.nUnsub
	nGH += d.nGH
	nTxn += d.nTxn
	nAuto += d.nAuto
	if owner {
		fromMe = n
	} else {
		fromMe = 0
	}
	listID = ""
	if l := topKeys(counts.Lists, 1); len(l) > 0 {
		listID = l[0]
	}
	nj, _ := json.Marshal(topKeys(counts.Names, 3))
	cj, _ := json.Marshal(counts)
	if source == SourceRule {
		nk := ClassifySender(KindInputs{Addr: k.addr, Domain: domain, NMsgs: n, NReplied: replied, NList: nList, NUnsub: nUnsub, NGH: nGH, NTxn: nTxn, NAuto: nAuto})
		if nk != kind || !exists {
			kind, kindAt = nk, now
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO senders (account, addr, domain, display_names_json, counts_json, first_at, last_at, n_msgs, n_from_me, n_to_me,
	n_replied_by_me, list_id, has_list_unsubscribe, n_list, n_unsub, n_gh, n_txn_subj, n_auto, kind, kind_source, kind_updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account, addr) DO UPDATE SET
	domain = excluded.domain, display_names_json = excluded.display_names_json, counts_json = excluded.counts_json,
	first_at = excluded.first_at, last_at = excluded.last_at, n_msgs = excluded.n_msgs, n_from_me = excluded.n_from_me,
	n_to_me = excluded.n_to_me, n_replied_by_me = excluded.n_replied_by_me, list_id = excluded.list_id,
	has_list_unsubscribe = excluded.has_list_unsubscribe, n_list = excluded.n_list, n_unsub = excluded.n_unsub,
	n_gh = excluded.n_gh, n_txn_subj = excluded.n_txn_subj, n_auto = excluded.n_auto, kind = excluded.kind, kind_source = excluded.kind_source,
	kind_updated_at = excluded.kind_updated_at`,
		k.account, k.addr, domain, string(nj), string(cj), first, last, n, fromMe, toMe, replied, listID,
		flagInt(nUnsub > 0), nList, nUnsub, nGH, nTxn, nAuto, kind, source, kindAt)
	if err != nil {
		return fmt.Errorf("senders: write row: %w", err)
	}
	return nil
}

// initSenders creates the senders job row on first start, snapshotting the
// highest messages rowid (refresh counts every later message itself), and
// publishes that snapshot.
//
// The counts a sender row holds depend on the rules that counted them (which
// subjects are order-shaped, which headers count), so a new rules version
// recounts instead of re-classifying stored counts: while the sendersRecount
// marker is absent, the first start resets the counts (keeping the owner's and
// the LLM's kind decisions), restarts the job from the first message and notes
// that in sendersRecountStarted. Later starts resume the job. The job writes
// the sendersRecount marker only after it counted everything. A job row that is
// new while senders rows already exist (a schema bump dropped the backfill
// table) resets the counts the same way.
func (c *Cache) initSenders() error {
	res, err := c.db.Exec(`
INSERT OR IGNORE INTO backfill (name, last_rowid, done, updated_at, max_rowid, total, processed)
SELECT ?, 0, CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END, ?, COALESCE(MAX(rowid), 0), COUNT(*), 0 FROM messages`,
		sendersJobName, c.now().Unix())
	if err != nil {
		return fmt.Errorf("init senders: %w", err)
	}
	newRow := false
	if n, _ := res.RowsAffected(); n == 1 {
		newRow = true
	}
	var started, marked int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM backfill WHERE name = ?`, sendersRecountStarted).Scan(&started); err != nil {
		return fmt.Errorf("init senders: %w", err)
	}
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM backfill WHERE name = ?`, sendersRecount).Scan(&marked); err != nil {
		return fmt.Errorf("init senders: %w", err)
	}
	if newRow || (started == 0 && marked == 0) {
		if err := c.restartSendersRecount(newRow); err != nil {
			return err
		}
	} else if marked == 0 {
		c.logger().Info("senders recount resumes after a restart")
	}
	var max int64
	if err := c.db.QueryRow(`SELECT max_rowid FROM backfill WHERE name = ?`, sendersJobName).Scan(&max); err != nil {
		return fmt.Errorf("init senders: %w", err)
	}
	c.sendersMax.Store(max)
	return nil
}

const (
	// sendersRecount is the backfill row that records which version of the
	// kind rules the senders table was counted and classified with. A new
	// version changes the name, so the first start after an upgrade recounts.
	// It is written when the recount has finished, not before.
	sendersRecount = "senders_recount_3"
	// sendersRecountStarted records that the recount of that version has been
	// started (counts reset, job restarted), so a restart resumes it.
	sendersRecountStarted = "senders_recount_3_started"
)

func (c *Cache) logger() *slog.Logger { return slog.Default() }

// restartSendersRecount snapshots the rule-derived kinds, resets the counts and
// restarts the senders job over the messages that exist now, in one transaction
// so a crash leaves either the old state (the next start does it again) or the
// whole new one.
func (c *Cache) restartSendersRecount(fresh bool) error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS senders_prev_kind (account TEXT NOT NULL, addr TEXT NOT NULL, kind TEXT NOT NULL, PRIMARY KEY (account, addr))`,
		`DELETE FROM senders_prev_kind`,
		`INSERT INTO senders_prev_kind SELECT account, addr, kind FROM senders WHERE kind_source = 'rule'`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("senders recount: %w", err)
		}
	}
	if err := resetSendersTx(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE backfill SET last_rowid = 0, updated_at = ?, processed = 0,
	max_rowid = (SELECT COALESCE(MAX(rowid), 0) FROM messages),
	total = (SELECT COUNT(*) FROM messages),
	done = CASE WHEN (SELECT COUNT(*) FROM messages) = 0 THEN 1 ELSE 0 END
WHERE name = ?`, c.now().Unix(), sendersJobName); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO backfill (name, last_rowid, done, updated_at) VALUES (?, 0, 1, ?)`, sendersRecountStarted, c.now().Unix()); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if !fresh {
		c.logger().Info("senders recount started: counts reset, owner and llm kinds kept; senders fill in again in the background")
	}
	return nil
}

// finishSendersRecount runs after the job counted every message: it logs, per
// account, how many rule-derived senders changed kind against the snapshot,
// then drops the snapshot and writes the marker in one transaction. It does
// nothing when the recount is already marked.
func (c *Cache) finishSendersRecount(ctx context.Context, log *slog.Logger) error {
	var marked int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backfill WHERE name = ?`, sendersRecount).Scan(&marked); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if marked > 0 {
		return nil
	}
	changed := map[string]map[string]int{}
	var total, rules int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'senders_prev_kind'`).Scan(&rules); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if rules > 0 {
		rows, err := c.db.QueryContext(ctx, `
SELECT s.account, p.kind, s.kind, COUNT(*) FROM senders s JOIN senders_prev_kind p ON p.account = s.account AND p.addr = s.addr
WHERE s.kind_source = 'rule' AND s.kind <> p.kind GROUP BY s.account, p.kind, s.kind`)
		if err != nil {
			return fmt.Errorf("senders recount: %w", err)
		}
		for rows.Next() {
			var acct, from, to string
			var n int
			if err := rows.Scan(&acct, &from, &to, &n); err != nil {
				_ = rows.Close()
				return fmt.Errorf("senders recount: %w", err)
			}
			if changed[acct] == nil {
				changed[acct] = map[string]int{}
			}
			changed[acct][from+"->"+to] += n
			total += n
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("senders recount: %w", err)
		}
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS senders_prev_kind`); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO backfill (name, last_rowid, done, updated_at) VALUES (?, 0, 1, ?)`, sendersRecount, c.now().Unix()); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("senders recount: %w", err)
	}
	log.Info("senders recount done", "kinds_changed", total)
	accts := make([]string, 0, len(changed))
	for a := range changed {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	for _, a := range accts {
		n := 0
		for _, v := range changed[a] {
			n += v
		}
		log.Info("senders recount changed kinds", "account", a, "changed", n, "transitions", fmt.Sprint(changed[a]))
	}
	return nil
}

// resetSendersTx deletes the counts of every sender, keeping the kind decisions
// of the owner and of the LLM (with their source), and marks every message as
// not yet looked at for replies, so a recount counts each message once.
func resetSendersTx(tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TEMP TABLE IF NOT EXISTS senders_keep AS SELECT account, addr, domain, kind, kind_source, kind_updated_at FROM senders WHERE 0`,
		`DELETE FROM senders_keep`,
		`INSERT INTO senders_keep SELECT account, addr, domain, kind, kind_source, kind_updated_at FROM senders WHERE kind_source IN ('owner', 'llm')`,
		`DELETE FROM senders`,
		`UPDATE messages SET replied_counted = 0 WHERE replied_counted <> 0`,
		`INSERT INTO senders (account, addr, domain, kind, kind_source, kind_updated_at)
SELECT account, addr, domain, kind, kind_source, kind_updated_at FROM senders_keep`,
		`DROP TABLE senders_keep`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("reset senders: %w", err)
		}
	}
	return nil
}

// SendersStatus is the progress of the senders job.
func (c *Cache) SendersStatus(ctx context.Context) (BackfillStatus, error) {
	var (
		st   BackfillStatus
		done int
	)
	err := c.db.QueryRowContext(ctx, `SELECT processed, total, done FROM backfill WHERE name = ?`, sendersJobName).Scan(&st.Done, &st.Total, &done)
	if err != nil {
		return st, fmt.Errorf("cache: senders status: %w", err)
	}
	st.Complete = done == 1
	if st.Complete {
		st.Done = st.Total
	}
	st.State = jobStateName(st.Complete, &c.sendersJob)
	return st, nil
}

// RunSenders is the senders job: first the List-Unsubscribe flag of every
// message that predates the column (a header-only read of its blob), then the
// counting of every message that predates the job. Resumable, short write
// transactions, gives way to foreground writers. It returns when done or ctx
// ends.
func (c *Cache) RunSenders(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if err := c.countSenders(ctx, log); err != nil {
		return err
	}
	// Replies last: they look at the Sent folders' membership.
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT account FROM folders`)
	if err != nil {
		return fmt.Errorf("senders: accounts: %w", err)
	}
	var accts []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			_ = rows.Close()
			return fmt.Errorf("senders: accounts: %w", err)
		}
		accts = append(accts, a)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("senders: accounts: %w", err)
	}
	for _, a := range accts {
		if err := c.creditSentReplies(ctx, a); err != nil {
			return err
		}
	}
	return c.finishSendersRecount(ctx, log)
}

func (c *Cache) countSenders(ctx context.Context, log *slog.Logger) error {
	c.sendersJob.running.Store(true)
	defer c.sendersJob.running.Store(false)
	if err := c.fillListUnsub(ctx, log); err != nil {
		return err
	}
	var (
		last, max int64
		done      int
	)
	if err := c.db.QueryRowContext(ctx, `SELECT last_rowid, max_rowid, done FROM backfill WHERE name = ?`, sendersJobName).Scan(&last, &max, &done); err != nil {
		return fmt.Errorf("senders state: %w", err)
	}
	if done == 1 {
		return nil
	}
	start := time.Now()
	rl := newRateLog("senders job", log, 0)
	total := 0
	for {
		type row struct {
			rid                         int64
			account, from, to, subject  string
			date, internal              int64
			listID, inReplyTo, refsJSON string
			unsub, gh                   bool
		}
		rows, err := c.db.QueryContext(ctx, `
SELECT rowid, account, from_addr, to_addr, subject, date_unix, internal_date, list_id, gh_reason <> '', COALESCE(list_unsub, 0) <> 0
FROM messages WHERE rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?`, last, max, sendersBatch)
		if err != nil {
			return fmt.Errorf("senders read: %w", err)
		}
		var batchRows []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rid, &r.account, &r.from, &r.to, &r.subject, &r.date, &r.internal, &r.listID, &r.gh, &r.unsub); err != nil {
				_ = rows.Close()
				return fmt.Errorf("senders read: %w", err)
			}
			batchRows = append(batchRows, r)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("senders read: %w", err)
		}
		finished := len(batchRows) < sendersBatch
		next := last
		if len(batchRows) > 0 {
			next = batchRows[len(batchRows)-1].rid
		}
		held := time.Duration(0)
		err = c.retryBusy(ctx, log, "senders job", func() error {
			if !c.waitForegroundIdle(ctx) {
				return ctx.Err()
			}
			sb := c.newSenderBatch()
			for _, r := range batchRows {
				sb.add(r.account, r.from, r.to, r.subject, r.date, r.internal, r.listID, r.unsub, r.gh)
			}
			tx, err := c.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			began := time.Now()
			if err := sb.flushTx(ctx, tx); err != nil {
				return err
			}
			if err := setBackfillTx(ctx, tx, sendersJobName, next, finished, c.now(), len(batchRows)); err != nil {
				return err
			}
			held, err = c.commitHeld(tx, began)
			return err
		})
		if err != nil {
			return err
		}
		last = next
		total += len(batchRows)
		if finished {
			log.Info("senders job done", "messages", total, "took", time.Since(start).Round(time.Second).String())
			return nil
		}
		rl.tick(total, 0)
		if !c.yield(ctx, held, backfillMinYield) {
			return ctx.Err()
		}
	}
}

// waitForegroundIdle lets a refresh or apply finish first (bounded by fgWaitMax).
func (c *Cache) waitForegroundIdle(ctx context.Context) bool {
	deadline := time.Now().Add(fgWaitMax)
	for c.fgWriters.Load() > 0 && time.Now().Before(deadline) {
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
	}
	return ctx.Err() == nil
}

// fillListUnsub sets messages.list_unsub for rows that predate the column,
// reading only the header block of each blob. An unreadable blob counts as
// "none" so the job always makes progress.
func (c *Cache) fillListUnsub(ctx context.Context, log *slog.Logger) error {
	var last int64
	filled := 0
	for {
		rows, err := c.db.QueryContext(ctx, `SELECT rowid, blob_sha256 FROM messages WHERE list_unsub IS NULL AND rowid > ? ORDER BY rowid LIMIT ?`, last, sendersBatch)
		if err != nil {
			return fmt.Errorf("list-unsubscribe read: %w", err)
		}
		type item struct {
			rid int64
			val int
		}
		var items []item
		for rows.Next() {
			var (
				rid int64
				sum string
			)
			if err := rows.Scan(&rid, &sum); err != nil {
				_ = rows.Close()
				return fmt.Errorf("list-unsubscribe read: %w", err)
			}
			items = append(items, item{rid, flagInt(blobHasListUnsub(c.BlobPath(sum)))})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("list-unsubscribe read: %w", err)
		}
		if len(items) == 0 {
			if filled > 0 {
				log.Info("list-unsubscribe fill done", "messages", filled)
			}
			return nil
		}
		held := time.Duration(0)
		err = c.retryBusy(ctx, log, "senders job", func() error {
			if !c.waitForegroundIdle(ctx) {
				return ctx.Err()
			}
			tx, err := c.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			began := time.Now()
			for _, it := range items {
				if _, err := tx.ExecContext(ctx, `UPDATE messages SET list_unsub = ? WHERE rowid = ? AND list_unsub IS NULL`, it.val, it.rid); err != nil {
					return err
				}
			}
			held, err = c.commitHeld(tx, began)
			return err
		})
		if err != nil {
			return err
		}
		last = items[len(items)-1].rid
		filled += len(items)
		if !c.yield(ctx, held, backfillMinYield) {
			return ctx.Err()
		}
	}
}

// maxHeaderRead bounds how much of a blob is read to find its headers.
const maxHeaderRead = 256 << 10

// blobHasListUnsub reports whether the blob at path has a List-Unsubscribe
// header, reading no further than the header block.
func blobHasListUnsub(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h, _ := textproto.NewReader(bufio.NewReaderSize(io.LimitReader(f, maxHeaderRead), 32<<10)).ReadMIMEHeader()
	return strings.TrimSpace(h.Get("List-Unsubscribe")) != ""
}
