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
// n_replied_by_me: for each message from the owner (an address in c.owners,
// the account's username) the parent is the message In-Reply-To names, else
// the last id of References. The parent is found through message_ref (which
// indexes every id a message mentions) restricted to the message whose own
// Message-ID is that id; the sender of the parent gets +1, at most once per
// owner message. Limits: the parent must be in the cache when the reply is
// counted (a reply counted before its parent arrived credits nobody); there
// is no thread-level fallback, because a thread-level credit would also credit
// every other participant of a mailing-list thread.

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
	"sort"
	"strings"
	"time"
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
}

type senderKey struct{ account, addr string }

type replyRef struct{ account, inReplyTo, refsJSON string }

// senderBatch accumulates deltas for a set of messages and writes them in one
// transaction (flushTx).
type senderBatch struct {
	c       *Cache
	deltas  map[senderKey]*senderDelta
	replies []replyRef
	owners  map[string][]string
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
	b.add(account, p.From, p.To, p.Subject, date, info.internal.Unix(), p.ListID, p.ListUnsub, p.GitHubReason != "",
		p.Thr.InReplyTo, p.Thr.refsJSON())
}

// add counts one message.
func (b *senderBatch) add(account, from, to, subject string, date, internal int64, listID string, unsub, gh bool, inReplyTo, refsJSON string) {
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
	if b.isOwner(account, addr) {
		d.fromMe++
		if inReplyTo != "" || (refsJSON != "" && refsJSON != "[]") {
			b.replies = append(b.replies, replyRef{account, inReplyTo, refsJSON})
		}
		return
	}
	lt := strings.ToLower(to)
	for _, o := range b.ownersOf(account) {
		if o != "" && strings.Contains(lt, o) {
			d.toMe++
			break
		}
	}
}

func firstPos(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// parentAddr is the sender of the message a reply answers: In-Reply-To, else
// the last Reference. "" when the parent is not in the cache.
func parentAddr(ctx context.Context, q rowQuerier, account, inReplyTo, refsJSON string) (string, error) {
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
	var from string
	err := q.QueryRowContext(ctx, `
SELECT p.from_addr FROM message_ref r JOIN messages p ON p.account = r.account AND p.stable_id = r.stable_id
WHERE r.account = ? AND r.ref_id = ? AND p.message_id = ? LIMIT 1`, account, id, id).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("senders: find parent: %w", err)
	}
	addr, _ := parseFrom(from)
	return addr, nil
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

// flushTx resolves the batch's replies, merges every delta into its senders
// row and writes it. It reads and writes only through tx.
func (b *senderBatch) flushTx(ctx context.Context, tx *sql.Tx) error {
	for _, r := range b.replies {
		addr, err := parentAddr(ctx, tx, r.account, r.inReplyTo, r.refsJSON)
		if err != nil {
			return err
		}
		if addr != "" && !b.isOwner(r.account, addr) {
			b.get(r.account, addr).replied++
		}
	}
	b.replies = nil
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
		domain, namesJSON, countsJSON, listID, kind, source string
		first, last, kindAt                                 int64
		n, fromMe, toMe, replied, nList, nUnsub, nGH, nTxn  int
		unsub                                               int
	)
	err := tx.QueryRowContext(ctx, `
SELECT domain, display_names_json, counts_json, first_at, last_at, n_msgs, n_from_me, n_to_me, n_replied_by_me,
       list_id, has_list_unsubscribe, n_list, n_unsub, n_gh, n_txn_subj, kind, kind_source, kind_updated_at
FROM senders WHERE account = ? AND addr = ?`, k.account, k.addr).Scan(
		&domain, &namesJSON, &countsJSON, &first, &last, &n, &fromMe, &toMe, &replied,
		&listID, &unsub, &nList, &nUnsub, &nGH, &nTxn, &kind, &source, &kindAt)
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
		nk := ClassifySender(KindInputs{Addr: k.addr, Domain: domain, NMsgs: n, NReplied: replied, NList: nList, NUnsub: nUnsub, NGH: nGH, NTxn: nTxn})
		if nk != kind || !exists {
			kind, kindAt = nk, now
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO senders (account, addr, domain, display_names_json, counts_json, first_at, last_at, n_msgs, n_from_me, n_to_me,
	n_replied_by_me, list_id, has_list_unsubscribe, n_list, n_unsub, n_gh, n_txn_subj, kind, kind_source, kind_updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account, addr) DO UPDATE SET
	domain = excluded.domain, display_names_json = excluded.display_names_json, counts_json = excluded.counts_json,
	first_at = excluded.first_at, last_at = excluded.last_at, n_msgs = excluded.n_msgs, n_from_me = excluded.n_from_me,
	n_to_me = excluded.n_to_me, n_replied_by_me = excluded.n_replied_by_me, list_id = excluded.list_id,
	has_list_unsubscribe = excluded.has_list_unsubscribe, n_list = excluded.n_list, n_unsub = excluded.n_unsub,
	n_gh = excluded.n_gh, n_txn_subj = excluded.n_txn_subj, kind = excluded.kind, kind_source = excluded.kind_source,
	kind_updated_at = excluded.kind_updated_at`,
		k.account, k.addr, domain, string(nj), string(cj), first, last, n, fromMe, toMe, replied, listID,
		flagInt(nUnsub > 0), nList, nUnsub, nGH, nTxn, kind, source, kindAt)
	if err != nil {
		return fmt.Errorf("senders: write row: %w", err)
	}
	return nil
}

// initSenders creates the senders job row on first start, snapshotting the
// highest messages rowid (refresh counts every later message itself), and
// publishes that snapshot. A job row that is new while senders rows already
// exist (a schema bump dropped the backfill table) resets the counts first,
// keeping only the owner's kind decisions, so nothing is counted twice.
func (c *Cache) initSenders() error {
	res, err := c.db.Exec(`
INSERT OR IGNORE INTO backfill (name, last_rowid, done, updated_at, max_rowid, total, processed)
SELECT ?, 0, CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END, ?, COALESCE(MAX(rowid), 0), COUNT(*), 0 FROM messages`,
		sendersJobName, c.now().Unix())
	if err != nil {
		return fmt.Errorf("init senders: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := c.resetSenders(); err != nil {
			return err
		}
	}
	var max int64
	if err := c.db.QueryRow(`SELECT max_rowid FROM backfill WHERE name = ?`, sendersJobName).Scan(&max); err != nil {
		return fmt.Errorf("init senders: %w", err)
	}
	c.sendersMax.Store(max)
	return nil
}

func (c *Cache) resetSenders() error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
CREATE TEMP TABLE IF NOT EXISTS senders_keep AS SELECT account, addr, domain, kind, kind_updated_at FROM senders WHERE 0`); err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM senders_keep`); err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO senders_keep SELECT account, addr, domain, kind, kind_updated_at FROM senders WHERE kind_source = 'owner'`); err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM senders`); err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO senders (account, addr, domain, kind, kind_source, kind_updated_at)
SELECT account, addr, domain, kind, 'owner', kind_updated_at FROM senders_keep`); err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE senders_keep`); err != nil {
		return fmt.Errorf("reset senders: %w", err)
	}
	return tx.Commit()
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
SELECT rowid, account, from_addr, to_addr, subject, date_unix, internal_date, list_id, gh_reason <> '', COALESCE(list_unsub, 0) <> 0,
       COALESCE(in_reply_to, ''), COALESCE(references_json, '')
FROM messages WHERE rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?`, last, max, sendersBatch)
		if err != nil {
			return fmt.Errorf("senders read: %w", err)
		}
		var batchRows []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rid, &r.account, &r.from, &r.to, &r.subject, &r.date, &r.internal, &r.listID, &r.gh, &r.unsub, &r.inReplyTo, &r.refsJSON); err != nil {
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
				sb.add(r.account, r.from, r.to, r.subject, r.date, r.internal, r.listID, r.unsub, r.gh, r.inReplyTo, r.refsJSON)
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
