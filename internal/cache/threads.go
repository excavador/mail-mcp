package cache

// Threads in the database: header columns, the id index, assignment of
// messages to threads (incremental), and the resumable backfill.
//
// The invariants:
//   - message_thread holds one row per threaded message; threads holds one
//     row per tid that has at least one member, and nothing else;
//   - Gmail messages with an X-GM-THRID are threaded by it ("g:<id>"); every
//     other message is threaded by JWZ over a connected component found
//     through message_ref (see BuildThreads);
//   - threading is idempotent: re-running it on any message of a component
//     rewrites that component to the same answer, which is how a late parent
//     re-links its children.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"
)

const (
	// maxComponent bounds how many messages one threading pass loads, so a
	// mailing list whose headers link everything cannot make one new message
	// cost a full-table pass. A component cut by the bound is threaded as
	// far as it was loaded.
	// maxRefs bounds the References kept per message: the first id (it names
	// the thread) and the nearest ancestors.
	maxRefs = 100
	// maxRefsJSON caps the stored references_json.
	maxRefsJSON = 16 << 10
	// idChunk is how many ids go in one IN (...) list.
	idChunk = 400
	// maxParticipants is how many distinct senders a thread row keeps.
	maxParticipants = 20
)

// maxComponent is a variable so that a test can lower it.
var maxComponent = 3000

// Test hooks, nil in production: gatherHook runs at the start of every
// gatherComponent, componentHook before each component is threaded, batchHook
// at the start of each backfill batch.
var (
	gatherHook    func()
	componentHook func(comp []threadRow)
	batchHook     func()
)

// threadHeaders are the three headers threading reads, normalised.
type threadHeaders struct {
	MessageID string
	InReplyTo string
	Refs      []string
}

func headersFromTextproto(h textproto.Header) threadHeaders {
	var th threadHeaders
	if t := idTokens(h.Get("Message-Id")); len(t) > 0 {
		th.MessageID = t[0]
	}
	if t := idTokens(h.Get("In-Reply-To")); len(t) > 0 {
		th.InReplyTo = t[0]
	}
	for _, v := range h.Values("References") {
		th.Refs = append(th.Refs, idTokens(v)...)
	}
	if len(th.Refs) > maxRefs {
		th.Refs = append([]string{th.Refs[0]}, th.Refs[len(th.Refs)-maxRefs+1:]...)
	}
	// references_json is capped too: drop the oldest ids after the first until
	// it fits.
	for len(th.refsJSON()) > maxRefsJSON && len(th.Refs) > 2 {
		th.Refs = append(th.Refs[:1], th.Refs[2:]...)
	}
	return th
}

func (th threadHeaders) refsJSON() string {
	b, _ := json.Marshal(append([]string{}, th.Refs...))
	return string(b)
}

// refKeys are the ids a message is indexed under in message_ref.
func (th threadHeaders) refKeys(subject string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	add(th.MessageID)
	add(th.InReplyTo)
	for _, r := range th.Refs {
		add(r)
	}
	if len(th.Refs) == 0 && th.InReplyTo == "" && isReplySubject(subject) {
		if ns := normSubject(subject); ns != "" {
			add("subj:" + ns)
		}
	}
	return out
}

func addRefsTx(ctx context.Context, tx *sql.Tx, account, stableID, subject string, th threadHeaders) error {
	for _, k := range th.refKeys(subject) {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO message_ref (account, ref_id, stable_id) VALUES (?, ?, ?)`, account, k, stableID); err != nil {
			return fmt.Errorf("index message ids: %w", err)
		}
	}
	return nil
}

// threadNew threads freshly inserted messages, in a transaction of its own
// after the insert committed. A failure is logged and otherwise ignored: the
// messages stay unthreaded (search treats them as single-message threads)
// until the backfill, which scans for unthreaded rows, picks them up.
func (c *Cache) threadNew(ctx context.Context, account string, ids []string) {
	if len(ids) == 0 {
		return
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("cache: thread new messages", "account", account, "err", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err := threadTxOpts(ctx, tx, account, ids, c.threadOpts(account)); err != nil {
		slog.Warn("cache: thread new messages", "account", account, "err", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("cache: thread new messages", "account", account, "err", err)
	}
}

type threadRow struct {
	stableID string
	gm       string
	filled   bool
	msgID    string
	irt      string
	refs     string
	subject  string
	date     int64
	arrival  int64
	from     string
	to, cc   string
}

func (r threadRow) msg() ThreadMsg {
	var refs []string
	if r.refs != "" {
		_ = json.Unmarshal([]byte(r.refs), &refs)
	}
	return ThreadMsg{StableID: r.stableID, MsgID: r.msgID, Refs: threadChain(refs, "<"+r.irt+">"), Subject: r.subject, Date: r.date,
		Arrival: r.arrival, Sender: strings.ToLower(BareAddr(r.from)), Recipients: recipientAddrs(r.to, r.cc)}
}

func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func chunks[T any](s []T, n int) [][]T {
	var out [][]T
	for len(s) > 0 {
		k := min(n, len(s))
		out = append(out, s[:k])
		s = s[k:]
	}
	return out
}

func strArgs(account string, ids []string) []any {
	a := make([]any, 0, len(ids)+1)
	a = append(a, account)
	for _, id := range ids {
		a = append(a, id)
	}
	return a
}

// loadThreadRows loads the threading view of the given messages.
func loadThreadRows(ctx context.Context, tx dbq, account string, ids []string) ([]threadRow, error) {
	var out []threadRow
	for _, part := range chunks(ids, idChunk) {
		rows, err := tx.QueryContext(ctx, `
SELECT stable_id, gm_thread_id, message_id IS NOT NULL, COALESCE(message_id,''), COALESCE(in_reply_to,''), COALESCE(references_json,''),
       subject, (CASE WHEN date_unix > 0 THEN date_unix ELSE internal_date END), internal_date, from_addr, to_addr, cc_addr
FROM messages WHERE account = ? AND stable_id IN (`+inList(len(part))+`)`, strArgs(account, part)...)
		if err != nil {
			return nil, fmt.Errorf("load messages to thread: %w", err)
		}
		for rows.Next() {
			var r threadRow
			if err := rows.Scan(&r.stableID, &r.gm, &r.filled, &r.msgID, &r.irt, &r.refs, &r.subject, &r.date, &r.arrival, &r.from, &r.to, &r.cc); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("load messages to thread: %w", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("load messages to thread: %w", err)
		}
		_ = rows.Close()
	}
	return out, nil
}

// threadTx (re)threads the components of the given messages inside tx.
func threadTx(ctx context.Context, tx *sql.Tx, account string, seeds []string) error {
	return threadTxOpts(ctx, tx, account, seeds, ThreadOpts{})
}

// soloTID is the thread id of a message threaded alone.
func soloTID(stableID string) string { return threadTID("\x00solo:" + stableID) }

// threadTxOpts is threadTx with the owner's addresses.
//
// Each component runs in a savepoint. A panic in the threading code (a header
// shape nobody thought of) rolls that component back, is logged, and the
// component's messages are written as single-message threads, so they count as
// threaded and are not retried forever; the rest of the batch goes on. When a
// component hits maxComponent, the seeds not in it are threaded alone rather
// than each starting another 3000-message pass.
func threadTxOpts(ctx context.Context, tx *sql.Tx, account string, seeds []string, opts ThreadOpts) error {
	rows, err := loadThreadRows(ctx, tx, account, seeds)
	if err != nil {
		return err
	}
	gmSeen := map[string]bool{}
	var jSeeds []string
	for _, r := range rows {
		switch {
		case r.gm != "":
			if !gmSeen[r.gm] {
				gmSeen[r.gm] = true
				if err := threadGmail(ctx, tx, account, r.gm, opts.Owners); err != nil {
					return err
				}
			}
		case r.filled:
			jSeeds = append(jSeeds, r.stableID)
		}
	}
	sort.Strings(jSeeds)
	visited := map[string]bool{}
	capped := false
	for _, seed := range jSeeds {
		if visited[seed] {
			continue
		}
		if capped {
			visited[seed] = true
			if err := applyAssigns(ctx, tx, account, []string{seed}, []ThreadAssign{{StableID: seed, TID: soloTID(seed)}}); err != nil {
				return err
			}
			continue
		}
		var comp []threadRow
		err := safeComponent(ctx, tx, func() error {
			var gerr error
			comp, capped, gerr = gatherComponent(ctx, tx, account, seed, visited)
			if gerr != nil {
				return gerr
			}
			return applyComponent(ctx, tx, account, comp, opts)
		}, func(r any) error {
			slog.Error("cache: threading panicked; component threaded alone", "account", account, "seed", seed, "panic", fmt.Sprint(r))
			ids := []string{seed}
			for _, m := range comp {
				ids = append(ids, m.stableID)
			}
			var as []ThreadAssign
			for _, id := range ids {
				visited[id] = true
				as = append(as, ThreadAssign{StableID: id, TID: soloTID(id)})
			}
			return applyAssigns(ctx, tx, account, ids, as)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// safeComponent runs fn in a savepoint. On error it rolls the savepoint back
// and returns the error; on panic it rolls back and calls onPanic.
func safeComponent(ctx context.Context, tx *sql.Tx, fn func() error, onPanic func(any) error) (err error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT thr_comp`); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				_, _ = tx.ExecContext(ctx, `ROLLBACK TO thr_comp`)
				err = onPanic(r)
			}
		}()
		err = fn()
	}()
	if !panicked && err != nil {
		_, _ = tx.ExecContext(ctx, `ROLLBACK TO thr_comp`)
	}
	if _, rerr := tx.ExecContext(ctx, `RELEASE thr_comp`); rerr != nil && err == nil {
		err = fmt.Errorf("release savepoint: %w", rerr)
	}
	return err
}

// threadGmail puts every message of a Gmail thread under "g:<id>".
func threadGmail(ctx context.Context, tx *sql.Tx, account, gm string, owners []string) error {
	old, err := distinctTids(ctx, tx, `SELECT DISTINCT tid FROM message_thread WHERE account = ? AND stable_id IN
		(SELECT stable_id FROM messages WHERE account = ? AND gm_thread_id = ?)`, account, account, gm)
	if err != nil {
		return err
	}
	tid := "g:" + gm
	if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO message_thread (account, stable_id, tid, parent_stable_id, depth)
SELECT account, stable_id, ?, '', 0 FROM messages WHERE account = ? AND gm_thread_id = ?`, tid, account, gm); err != nil {
		return fmt.Errorf("thread gmail: %w", err)
	}
	var others []string
	for _, o := range old {
		if o != tid {
			others = append(others, o)
		}
	}
	if err := refreshAggregates(ctx, tx, account, others); err != nil {
		return err
	}
	return refreshGmailThreads(ctx, tx, account, []string{tid}, owners)
}

func distinctTids(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("thread ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("thread ids: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// refMembersSQL lists the messages indexed under one id, in stable_id order.
const refMembersSQL = `SELECT stable_id FROM message_ref WHERE account = ? AND ref_id = ? ORDER BY stable_id LIMIT ?`

// gatherComponent loads the connected component of seed: every unthreadable-
// by-Gmail message that shares an id with a member, transitively, plus the
// threads a header-less reply could join by subject. Members are marked
// visited.
func gatherComponent(ctx context.Context, tx dbq, account, seed string, visited map[string]bool) ([]threadRow, bool, error) {
	if gatherHook != nil {
		gatherHook()
	}
	set := map[string]threadRow{}
	var order []string
	idsSeen := map[string]bool{}
	var refStmt *sql.Stmt
	queue := []string{seed}
	for len(queue) > 0 && len(set) < maxComponent {
		rows, err := loadThreadRows(ctx, tx, account, queue)
		if err != nil {
			return nil, false, err
		}
		queue = nil
		var added []string
		var extra []string // more stable ids to load, found by subject
		for _, r := range rows {
			if _, ok := set[r.stableID]; ok || r.gm != "" || !r.filled || len(set) >= maxComponent {
				continue
			}
			set[r.stableID] = r
			order = append(order, r.stableID)
			added = append(added, r.stableID)
			visited[r.stableID] = true
		}
		var frontier []string
		for _, part := range chunks(added, idChunk) {
			rs, err := tx.QueryContext(ctx, `SELECT ref_id FROM message_ref WHERE account = ? AND stable_id IN (`+inList(len(part))+`)`, strArgs(account, part)...)
			if err != nil {
				return nil, false, fmt.Errorf("gather thread: %w", err)
			}
			for rs.Next() {
				var id string
				if err := rs.Scan(&id); err != nil {
					_ = rs.Close()
					return nil, false, fmt.Errorf("gather thread: %w", err)
				}
				frontier = append(frontier, id)
			}
			if err := rs.Err(); err != nil {
				_ = rs.Close()
				return nil, false, fmt.Errorf("gather thread: %w", err)
			}
			_ = rs.Close()
		}
		for _, id := range added {
			r := set[id]
			ns := normSubject(r.subject)
			if ns != "" {
				frontier = append(frontier, "subj:"+ns) // header-less replies to this subject
			}
			m := r.msg()
			if len(m.Refs) == 0 && isReplySubject(r.subject) && ns != "" {
				// A header-less reply: the top-level messages it may join.
				roots, err := rootsBySubject(ctx, tx, account, ns, r.date)
				if err != nil {
					return nil, false, err
				}
				extra = append(extra, roots...)
			}
		}
		var fresh []string
		for _, id := range frontier {
			if !idsSeen[id] {
				idsSeen[id] = true
				fresh = append(fresh, id)
			}
		}
		for _, part := range chunks(fresh, idChunk) {
			// One bounded lookup per id, served in order by the primary key
			// (account, ref_id, stable_id), so a hot id (a shared "subj:" key)
			// costs at most maxComponent+1 rows. A single IN (...) query with
			// DISTINCT and LIMIT makes SQLite prefer a covering scan of
			// message_ref_by_msg, a walk of the whole account per component.
			// The first maxComponent+1 distinct ids of the union are within the
			// union of each id's first maxComponent+1, so merging in Go gives
			// the answer the single query gave.
			if refStmt == nil {
				if refStmt, err = tx.PrepareContext(ctx, refMembersSQL); err != nil {
					return nil, false, fmt.Errorf("gather thread: %w", err)
				}
				defer refStmt.Close()
			}
			var sids []string
			for _, ref := range part {
				rs, err := refStmt.QueryContext(ctx, account, ref, maxComponent+1)
				if err != nil {
					return nil, false, fmt.Errorf("gather thread: %w", err)
				}
				for rs.Next() {
					var sid string
					if err := rs.Scan(&sid); err != nil {
						_ = rs.Close()
						return nil, false, fmt.Errorf("gather thread: %w", err)
					}
					sids = append(sids, sid)
				}
				if err := rs.Err(); err != nil {
					_ = rs.Close()
					return nil, false, fmt.Errorf("gather thread: %w", err)
				}
				_ = rs.Close()
			}
			sort.Strings(sids)
			sids = slices.Compact(sids)
			if len(sids) > maxComponent+1 {
				sids = sids[:maxComponent+1]
			}
			for _, sid := range sids {
				if _, ok := set[sid]; !ok {
					queue = append(queue, sid)
				}
			}
		}
		for _, sid := range extra {
			if _, ok := set[sid]; !ok {
				queue = append(queue, sid)
			}
		}
	}
	out := make([]threadRow, 0, len(order))
	for _, id := range order {
		out = append(out, set[id])
	}
	return out, len(set) >= maxComponent, nil
}

// rootsBySubject returns the root messages of JWZ threads with this
// normalised subject whose first message is within the subject window of date.
func rootsBySubject(ctx context.Context, tx dbq, account, ns string, date int64) ([]string, error) {
	rs, err := tx.QueryContext(ctx, `SELECT root_stable_id FROM threads
WHERE account = ? AND subject_norm = ? AND tid LIKE 'j:%' AND first_at BETWEEN ? AND ? LIMIT 20`,
		account, ns, date-subjectWindow, date+subjectWindow)
	if err != nil {
		return nil, fmt.Errorf("gather thread by subject: %w", err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, fmt.Errorf("gather thread by subject: %w", err)
		}
		out = append(out, s)
	}
	return out, rs.Err()
}

// applyComponent threads comp and rewrites message_thread and threads for it.
func applyComponent(ctx context.Context, tx *sql.Tx, account string, comp []threadRow, opts ThreadOpts) error {
	if len(comp) == 0 {
		return nil
	}
	if componentHook != nil {
		componentHook(comp)
	}
	msgs := make([]ThreadMsg, len(comp))
	ids := make([]string, len(comp))
	for i, r := range comp {
		msgs[i], ids[i] = r.msg(), r.stableID
	}
	return applyAssigns(ctx, tx, account, ids, BuildThreadsOpts(msgs, opts))
}

// dbq is what the read side of threading needs: a *sql.DB (a plan computed
// outside any transaction) or a *sql.Tx.
type dbq interface {
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// loadHave reads the current assignment of ids.
func loadHave(ctx context.Context, q dbq, account string, ids []string) (map[string]assignRow, error) {
	have := map[string]assignRow{}
	for _, part := range chunks(ids, idChunk) {
		rows, err := q.QueryContext(ctx, `SELECT stable_id, tid, parent_stable_id, depth, outsider FROM message_thread
WHERE account = ? AND stable_id IN (`+inList(len(part))+`)`, strArgs(account, part)...)
		if err != nil {
			return nil, fmt.Errorf("rewrite threads: %w", err)
		}
		for rows.Next() {
			var id string
			var r assignRow
			var o int
			if err := rows.Scan(&id, &r.tid, &r.parent, &r.depth, &o); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("rewrite threads: %w", err)
			}
			r.outsider = o == 1
			have[id] = r
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("rewrite threads: %w", err)
		}
		_ = rows.Close()
	}
	return have, nil
}

// threadPlan is the result of threading one component (or one Gmail thread),
// computed with reads only, to be applied later in a short transaction.
type threadPlan struct {
	account string
	opts    ThreadOpts
	gm      string // a Gmail thread: applied by threadGmail
	ids     []string
	assigns []ThreadAssign
	have    map[string]assignRow // the assignments the plan was computed against
}

// planThreads is threadTxOpts without the writes: it returns what each
// component would be written as. Panics in the threading code turn the
// component into single-message threads, as in threadTxOpts.
func planThreads(ctx context.Context, q dbq, account string, seeds []string, opts ThreadOpts) ([]threadPlan, error) {
	rows, err := loadThreadRows(ctx, q, account, seeds)
	if err != nil {
		return nil, err
	}
	var plans []threadPlan
	gmSeen := map[string]bool{}
	var jSeeds []string
	for _, r := range rows {
		switch {
		case r.gm != "":
			if !gmSeen[r.gm] {
				gmSeen[r.gm] = true
				plans = append(plans, threadPlan{account: account, opts: opts, gm: r.gm})
			}
		case r.filled:
			jSeeds = append(jSeeds, r.stableID)
		}
	}
	sort.Strings(jSeeds)
	solo := func(ids ...string) threadPlan {
		as := make([]ThreadAssign, len(ids))
		for i, id := range ids {
			as[i] = ThreadAssign{StableID: id, TID: soloTID(id)}
		}
		return threadPlan{account: account, opts: opts, ids: ids, assigns: as}
	}
	visited := map[string]bool{}
	capped := false
	for _, seed := range jSeeds {
		if visited[seed] {
			continue
		}
		if capped {
			visited[seed] = true
			plans = append(plans, solo(seed))
			continue
		}
		var (
			comp []threadRow
			plan *threadPlan
			gerr error
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("cache: threading panicked; component threaded alone", "account", account, "seed", seed, "panic", fmt.Sprint(r))
					ids := []string{seed}
					for _, m := range comp {
						ids = append(ids, m.stableID)
					}
					for _, id := range ids {
						visited[id] = true
					}
					p := solo(ids...)
					plan, gerr = &p, nil
				}
			}()
			comp, capped, gerr = gatherComponent(ctx, q, account, seed, visited)
			if gerr != nil || len(comp) == 0 {
				return
			}
			if componentHook != nil {
				componentHook(comp)
			}
			msgs := make([]ThreadMsg, len(comp))
			ids := make([]string, len(comp))
			for i, r := range comp {
				msgs[i], ids[i] = r.msg(), r.stableID
			}
			plan = &threadPlan{account: account, opts: opts, ids: ids, assigns: BuildThreadsOpts(msgs, opts)}
		}()
		if gerr != nil {
			return nil, gerr
		}
		if plan != nil {
			plans = append(plans, *plan)
		}
	}
	for i := range plans {
		if plans[i].gm != "" {
			continue
		}
		if plans[i].have, err = loadHave(ctx, q, account, plans[i].ids); err != nil {
			return nil, err
		}
	}
	return plans, nil
}

// applyPlan writes a plan inside tx. If any member's assignment changed since
// the plan was computed (a refresh threaded it meanwhile), the plan is stale
// and the component is planned again inside tx, where nothing can move.
func applyPlan(ctx context.Context, tx *sql.Tx, p threadPlan) error {
	if p.gm != "" {
		return threadGmail(ctx, tx, p.account, p.gm, p.opts.Owners)
	}
	cur, err := loadHave(ctx, tx, p.account, p.ids)
	if err != nil {
		return err
	}
	if len(cur) == len(p.have) {
		fresh := true
		for id, a := range cur {
			if b, ok := p.have[id]; !ok || a != b {
				fresh = false
				break
			}
		}
		if fresh {
			return applyAssigns(ctx, tx, p.account, p.ids, p.assigns)
		}
	}
	again, err := planThreads(ctx, tx, p.account, p.ids, p.opts)
	if err != nil {
		return err
	}
	for _, q := range again {
		if err := applyAssigns(ctx, tx, q.account, q.ids, q.assigns); err != nil {
			return err
		}
		if q.gm != "" {
			if err := threadGmail(ctx, tx, q.account, q.gm, q.opts.Owners); err != nil {
				return err
			}
		}
	}
	return nil
}

type assignRow struct {
	tid, parent string
	depth       int
	outsider    bool
}

// applyAssigns writes assigns for ids, touching only the rows whose
// assignment changed, and refreshes the thread rows of the tids those rows
// left or joined. An unchanged component costs reads only.
func applyAssigns(ctx context.Context, tx *sql.Tx, account string, ids []string, assigns []ThreadAssign) error {
	have := map[string]assignRow{}
	for _, part := range chunks(ids, idChunk) {
		rows, err := tx.QueryContext(ctx, `SELECT stable_id, tid, parent_stable_id, depth, outsider FROM message_thread
WHERE account = ? AND stable_id IN (`+inList(len(part))+`)`, strArgs(account, part)...)
		if err != nil {
			return fmt.Errorf("rewrite threads: %w", err)
		}
		for rows.Next() {
			var id string
			var r assignRow
			var o int
			if err := rows.Scan(&id, &r.tid, &r.parent, &r.depth, &o); err != nil {
				_ = rows.Close()
				return fmt.Errorf("rewrite threads: %w", err)
			}
			r.outsider = o == 1
			have[id] = r
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("rewrite threads: %w", err)
		}
		_ = rows.Close()
	}
	tidSet := map[string]bool{}
	for _, a := range assigns {
		want := assignRow{a.TID, a.Parent, a.Depth, a.Outsider}
		old, existed := have[a.StableID]
		if existed && old == want {
			continue
		}
		o := 0
		if a.Outsider {
			o = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO message_thread (account, stable_id, tid, parent_stable_id, depth, outsider) VALUES (?, ?, ?, ?, ?, ?)`,
			account, a.StableID, a.TID, a.Parent, a.Depth, o); err != nil {
			return fmt.Errorf("rewrite threads: %w", err)
		}
		tidSet[a.TID] = true
		if existed {
			tidSet[old.tid] = true
		}
	}
	tids := make([]string, 0, len(tidSet))
	for t := range tidSet {
		tids = append(tids, t)
	}
	sort.Strings(tids)
	return refreshAggregates(ctx, tx, account, tids)
}

// arrivalCol is when the server received a message (the sender does not
// control it), else its Date header.
const arrivalCol = `(CASE WHEN m.internal_date > 0 THEN m.internal_date ELSE m.date_unix END)`

// refreshAggregates recomputes the threads row of each tid from its members,
// and removes the row of a tid that has none left. Time and root follow
// arrival, not the Date header, which the sender controls.
func refreshAggregates(ctx context.Context, tx *sql.Tx, account string, tids []string) error {
	seen := map[string]bool{}
	for _, tid := range tids {
		if seen[tid] {
			continue
		}
		seen[tid] = true
		// INDEXED BY: without table statistics SQLite walks every row of the
		// account in the primary key and filters on tid, which makes each thread
		// cost a pass over message_thread (and the backfill slower as it fills it).
		rows, err := tx.QueryContext(ctx, `
SELECT m.stable_id, m.from_addr, m.subject, `+arrivalCol+`, t.depth
FROM message_thread t INDEXED BY message_thread_by_tid JOIN messages m ON m.account = t.account AND m.stable_id = t.stable_id
WHERE t.account = ? AND t.tid = ? ORDER BY `+arrivalCol+`, m.stable_id`, account, tid)
		if err != nil {
			return fmt.Errorf("thread aggregate: %w", err)
		}
		var (
			n           int
			first, last int64
			root        string
			rootSubj    string
			rootDepth   = -1
			people      []string
			have        = map[string]bool{}
		)
		for rows.Next() {
			var sid, from, subj string
			var d int64
			var depth int
			if err := rows.Scan(&sid, &from, &subj, &d, &depth); err != nil {
				_ = rows.Close()
				return fmt.Errorf("thread aggregate: %w", err)
			}
			if n == 0 {
				first = d
			}
			last = max(last, d)
			if rootDepth < 0 || (depth == 0 && rootDepth > 0) {
				root, rootSubj, rootDepth = sid, subj, depth
			}
			n++
			if name := personName(from); name != "" && !have[name] && len(people) < maxParticipants {
				have[name] = true
				people = append(people, name)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("thread aggregate: %w", err)
		}
		_ = rows.Close()
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE account = ? AND tid = ?`, account, tid); err != nil {
				return fmt.Errorf("thread aggregate: %w", err)
			}
			continue
		}
		pj, _ := json.Marshal(people)
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO threads (account, tid, root_stable_id, subject_norm, first_at, last_at, n_msgs, participants_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, account, tid, root, normSubject(rootSubj), first, last, n, string(pj)); err != nil {
			return fmt.Errorf("thread aggregate: %w", err)
		}
	}
	return nil
}

// personName is the display name of an address header, else its address.
func personName(from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		return ""
	}
	if a, err := mail.ParseAddress(from); err == nil {
		if a.Name != "" {
			return a.Name
		}
		return a.Address
	}
	return from
}

// --- backfill ---------------------------------------------------------------

const (
	threadBackfillName  = "threads"
	threadBackfillBatch = 500
)

// backfillPause is the rest between batches, so the backfill never competes
// with a refresh or a search for long; tests set it to zero.
var backfillPause = 100 * time.Millisecond

func (c *Cache) backfillState(ctx context.Context, name string) (last int64, done bool, err error) {
	var d int
	err = c.db.QueryRowContext(ctx, `SELECT last_rowid, done FROM backfill WHERE name = ?`, name).Scan(&last, &d)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return last, d == 1, err
}

// setBackfillTx records progress of the threads job. It touches only its own
// row (name = "threads"); the fts2 job's row is never read or written here.
// n is how many messages this batch covered, added to processed.
func setBackfillTx(ctx context.Context, tx *sql.Tx, name string, last int64, done bool, now time.Time, n int) error {
	d := 0
	if done {
		d = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO backfill (name, last_rowid, done, updated_at, processed) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET last_rowid = excluded.last_rowid, done = excluded.done, updated_at = excluded.updated_at,
	processed = backfill.processed + excluded.processed`,
		name, last, d, now.Unix(), n)
	return err
}

// notBulkThreaded selects the messages the per-message backfill still has to
// visit: everything except a Gmail message BulkThreadGmail already placed
// (its thread is its gm_thread_id, nothing else is needed).
const notBulkThreaded = `NOT (gm_thread_id <> '' AND EXISTS
	(SELECT 1 FROM message_thread t WHERE t.account = messages.account AND t.stable_id = messages.stable_id))`

// startThreadBackfill (re)starts the threads job from the beginning: position
// 0, not done, total = the messages there are now, processed = those the
// Gmail bulk pass has already threaded (the loop skips them, so they count as
// done from the start).
func (c *Cache) startThreadBackfill(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO backfill (name, last_rowid, done, updated_at, total, processed)
VALUES (?, 0, 0, ?, (SELECT COUNT(*) FROM messages),
	(SELECT COUNT(*) FROM messages WHERE NOT `+notBulkThreaded+`))
ON CONFLICT(name) DO UPDATE SET last_rowid = 0, done = 0, updated_at = excluded.updated_at, total = excluded.total, processed = excluded.processed`,
		threadBackfillName, c.now().Unix())
	return err
}

// ThreadsBackfillStatus is the progress of the threads job, like BackfillStatus.
func (c *Cache) ThreadsBackfillStatus(ctx context.Context) (BackfillStatus, error) {
	var (
		st   BackfillStatus
		done int
	)
	err := c.db.QueryRowContext(ctx, `SELECT processed, total, done FROM backfill WHERE name = ?`, threadBackfillName).Scan(&st.Done, &st.Total, &done)
	if errors.Is(err, sql.ErrNoRows) {
		st.State = "pending" // not started yet
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("cache: threads backfill status: %w", err)
	}
	st.Complete = done == 1
	if st.Complete {
		st.Done = st.Total
	}
	st.State = jobStateName(st.Complete, &c.threadsJob)
	return st, nil
}

// BackfillThreads fills the threading headers of messages cached before
// threads existed and threads every message not yet threaded. It is resumable
// (progress is committed with each batch under backfill.name = "threads"),
// rate-limited, and returns when everything is done or ctx ends. Safe to run
// beside a refresh: each batch is one short transaction.
//
// When a previous run finished, a cheap check looks for messages that were
// never threaded (an inline threading step that failed) and, if there are
// any, runs again from the start.
func (c *Cache) BackfillThreads(ctx context.Context, log *slog.Logger) error {
	c.threadsJob.running.Store(true)
	defer c.threadsJob.running.Store(false)
	last, done, err := c.backfillState(ctx, threadBackfillName)
	if err != nil {
		return fmt.Errorf("backfill state: %w", err)
	}
	fresh := last == 0 && !done
	if done {
		var one int
		err := c.db.QueryRowContext(ctx, `SELECT 1 FROM messages m WHERE NOT EXISTS
			(SELECT 1 FROM message_thread t WHERE t.account = m.account AND t.stable_id = m.stable_id) LIMIT 1`).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("backfill check: %w", err)
		}
		last, fresh = 0, true
	}
	if fresh {
		if err := c.startThreadBackfill(ctx); err != nil {
			return fmt.Errorf("backfill start: %w", err)
		}
	}
	start, processed := c.now(), 0
	var rl *rateLog
	if log != nil {
		// Rates count from where this run starts, not from 0: after a restart
		// the first line would otherwise report the whole backlog as one minute's work.
		from := 0
		if st, err := c.ThreadsBackfillStatus(ctx); err == nil {
			from = st.Done
		}
		rl = newRateLog("thread backfill", log, from)
	}
	for {
		var (
			n        int
			next     int64
			finished bool
		)
		err := c.retryBusy(ctx, log, "thread backfill", func() error {
			var err error
			n, next, finished, err = c.safeThreadBatch(ctx, last, log)
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		processed += n
		last = next
		if finished {
			if log != nil {
				log.Info("thread backfill done", "messages", processed, "took", c.now().Sub(start).Round(time.Second).String())
			}
			return nil
		}
		if rl != nil && time.Since(rl.last) >= backfillLogEvery {
			if st, err := c.ThreadsBackfillStatus(ctx); err == nil {
				rl.tick(st.Done, st.Total)
			}
		}
	}
}

type backfillRow struct {
	rowid        int64
	account, id  string
	sum, subject string
	needsHeaders bool
}

// backfillThreadBatch handles up to threadBackfillBatch messages after last.
// It returns how many it saw, the new position and whether the table ended.
//
// Only the writes hold the database lock, in transactions of at most
// writeRows rows or writeSlice of work with a rest between them: the blob
// headers are read first, then the header columns and id index are written in
// chunks, then threading is computed from reads alone (planThreads) and the
// plans are applied in chunks. Every step is idempotent, so a batch cut short
// is redone from the unthreaded rows.
func (c *Cache) backfillThreadBatch(ctx context.Context, last int64) (int, int64, bool, error) {
	if batchHook != nil {
		batchHook()
	}
	rs, err := c.db.QueryContext(ctx, `SELECT rowid, account, stable_id, blob_sha256, subject, message_id IS NULL
FROM messages WHERE rowid > ? AND `+notBulkThreaded+` ORDER BY rowid LIMIT ?`, last, threadBackfillBatch)
	if err != nil {
		return 0, last, false, fmt.Errorf("backfill scan: %w", err)
	}
	var batch []backfillRow
	for rs.Next() {
		var r backfillRow
		if err := rs.Scan(&r.rowid, &r.account, &r.id, &r.sum, &r.subject, &r.needsHeaders); err != nil {
			_ = rs.Close()
			return 0, last, false, fmt.Errorf("backfill scan: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rs.Err(); err != nil {
		_ = rs.Close()
		return 0, last, false, fmt.Errorf("backfill scan: %w", err)
	}
	_ = rs.Close()
	if len(batch) == 0 {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, last, false, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := setBackfillTx(ctx, tx, threadBackfillName, last, true, c.now(), 0); err != nil {
			return 0, last, false, err
		}
		return 0, last, true, tx.Commit()
	}

	// Blob reads happen before any write transaction, so the lock is not
	// held while the disk is slow.
	headers := make(map[int64]threadHeaders, len(batch))
	var withHeaders []backfillRow
	for _, r := range batch {
		if !r.needsHeaders {
			continue
		}
		if ctx.Err() != nil {
			return 0, last, false, ctx.Err()
		}
		headers[r.rowid] = c.readBlobThreadHeaders(r.sum)
		withHeaders = append(withHeaders, r)
	}

	// Header columns and id index, in short transactions. Each row's UPDATE is
	// guarded (message_id IS NULL) and the index insert is OR IGNORE.
	for i := 0; i < len(withHeaders); {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, last, false, fmt.Errorf("backfill begin: %w", err)
		}
		locked := time.Now()
		j := i
		for j < len(withHeaders) && (j == i || (j-i < writeRows && time.Since(locked) < writeSlice)) {
			r := withHeaders[j]
			th := headers[r.rowid]
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET message_id = ?, in_reply_to = ?, references_json = ? WHERE rowid = ? AND message_id IS NULL`,
				th.MessageID, th.InReplyTo, th.refsJSON(), r.rowid); err != nil {
				_ = tx.Rollback()
				return 0, last, false, fmt.Errorf("backfill headers: %w", err)
			}
			if err := addRefsTx(ctx, tx, r.account, r.id, r.subject, th); err != nil {
				_ = tx.Rollback()
				return 0, last, false, err
			}
			j++
		}
		hold, err := c.commitHeld(tx, locked)
		if err != nil {
			return 0, last, false, err
		}
		i = j
		if !c.yield(ctx, hold, backfillPause) {
			return 0, last, false, ctx.Err()
		}
	}

	// Which rows still need threading, then the plans: reads only.
	byAcct := map[string][]string{}
	var accts []string
	for _, r := range batch {
		if _, ok := byAcct[r.account]; !ok {
			accts = append(accts, r.account)
		}
		byAcct[r.account] = append(byAcct[r.account], r.id)
	}
	var plans []threadPlan
	for _, acct := range accts {
		var todo []string
		for _, part := range chunks(byAcct[acct], idChunk) {
			rs, err := c.db.QueryContext(ctx, `SELECT stable_id FROM messages m WHERE account = ? AND stable_id IN (`+inList(len(part))+`)
AND NOT EXISTS (SELECT 1 FROM message_thread t WHERE t.account = m.account AND t.stable_id = m.stable_id)`, strArgs(acct, part)...)
			if err != nil {
				return 0, last, false, fmt.Errorf("backfill unthreaded: %w", err)
			}
			for rs.Next() {
				var s string
				if err := rs.Scan(&s); err != nil {
					_ = rs.Close()
					return 0, last, false, fmt.Errorf("backfill unthreaded: %w", err)
				}
				todo = append(todo, s)
			}
			_ = rs.Close()
		}
		ps, err := planThreads(ctx, c.db, acct, todo, c.threadOpts(acct))
		if err != nil {
			return 0, last, false, err
		}
		plans = append(plans, ps...)
	}

	// Apply the plans in short transactions.
	for i := 0; i < len(plans); {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, last, false, fmt.Errorf("backfill begin: %w", err)
		}
		locked := time.Now()
		j := i
		for j < len(plans) && (j == i || (j-i < writeRows && time.Since(locked) < writeSlice)) {
			if err := applyPlan(ctx, tx, plans[j]); err != nil {
				_ = tx.Rollback()
				return 0, last, false, err
			}
			j++
		}
		hold, err := c.commitHeld(tx, locked)
		if err != nil {
			return 0, last, false, err
		}
		i = j
		if !c.yield(ctx, hold, backfillPause) {
			return 0, last, false, ctx.Err()
		}
	}

	next := batch[len(batch)-1].rowid
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, last, false, fmt.Errorf("backfill begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	locked := time.Now()
	if err := setBackfillTx(ctx, tx, threadBackfillName, next, false, c.now(), len(batch)); err != nil {
		return 0, last, false, err
	}
	if _, err := c.commitHeld(tx, locked); err != nil {
		return 0, last, false, err
	}
	return len(batch), next, false, nil
}

// readBlobThreadHeaders reads only the header block of a cached blob. A
// missing or unreadable blob yields no headers (the message then threads
// alone) rather than stalling the backfill.
func (c *Cache) readBlobThreadHeaders(sum string) (out threadHeaders) {
	// A panic in the header or charset code must not kill the process (a poison
	// message would crash-loop the pod): like parseMessage, recover and carry on.
	defer func() {
		if recover() != nil {
			out = threadHeaders{}
		}
	}()
	path := c.BlobPath(sum)
	if path == "" {
		return threadHeaders{}
	}
	f, err := os.Open(path)
	if err != nil {
		return threadHeaders{}
	}
	defer f.Close()
	h, err := textproto.ReadHeader(bufio.NewReaderSize(f, 32<<10))
	if err != nil && h.Len() == 0 {
		return threadHeaders{}
	}
	return headersFromTextproto(h)
}

// --- reading a thread --------------------------------------------------------

// ThreadMember is one message of a thread.
type ThreadMember struct {
	StableID string
	Date     time.Time
	From     string
	Subject  string
	Depth    int
	Outsider bool
}

// ThreadMessages lists the messages of a thread, oldest first, with the
// thread's subject. A "u:<stable id>" tid names an unthreaded message as a
// thread of one. An unknown tid is ErrNotFound.
func (c *Cache) ThreadMessages(ctx context.Context, account, tid string) ([]ThreadMember, string, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if id, ok := strings.CutPrefix(tid, "u:"); ok {
		rows, err = c.db.QueryContext(ctx, `SELECT m.stable_id, `+dateCol+`, m.from_addr, m.subject, 0, 0 FROM messages m
WHERE m.account = ? AND m.stable_id = ?`, account, id)
	} else {
		rows, err = c.db.QueryContext(ctx, `SELECT m.stable_id, `+dateCol+`, m.from_addr, m.subject, t.depth, t.outsider
FROM message_thread t INDEXED BY message_thread_by_tid JOIN messages m ON m.account = t.account AND m.stable_id = t.stable_id
WHERE t.account = ? AND t.tid = ? ORDER BY `+dateCol+`, m.stable_id`, account, tid)
	}
	if err != nil {
		return nil, "", fmt.Errorf("cache: thread messages: %w", err)
	}
	defer rows.Close()
	var out []ThreadMember
	for rows.Next() {
		var m ThreadMember
		var d int64
		var outs int
		if err := rows.Scan(&m.StableID, &d, &m.From, &m.Subject, &m.Depth, &outs); err != nil {
			return nil, "", fmt.Errorf("cache: thread messages: %w", err)
		}
		m.Outsider = outs == 1
		if d > 0 {
			m.Date = time.Unix(d, 0).UTC()
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("cache: thread messages: %w", err)
	}
	if len(out) == 0 {
		return nil, "", ErrNotFound
	}
	subject := out[0].Subject
	var root string
	if err := c.db.QueryRowContext(ctx, `SELECT COALESCE(r.subject, '') FROM threads t JOIN messages r
ON r.account = t.account AND r.stable_id = t.root_stable_id WHERE t.account = ? AND t.tid = ?`, account, tid).Scan(&root); err == nil && root != "" {
		subject = root
	}
	return out, subject, nil
}

// BodyNew returns the cleaned text (quotes, reply headers and signatures
// stripped) of each given message: from message_fts2.body_new (rowid = the
// messages rowid) when the text pipeline has indexed the message, otherwise
// computed from the blob with the same parse. A message whose blob is gone
// is absent from the result.
func (c *Cache) BodyNew(ctx context.Context, account string, ids []string) map[string]string {
	out := map[string]string{}
	for _, part := range chunks(ids, idChunk) {
		rows, err := c.db.QueryContext(ctx, `SELECT m.stable_id, f.body_new FROM messages m JOIN message_fts2 f ON f.rowid = m.rowid
WHERE m.account = ? AND m.stable_id IN (`+inList(len(part))+`)`, strArgs(account, part)...)
		if err == nil {
			for rows.Next() {
				var id, b string
				if rows.Scan(&id, &b) == nil {
					out[id] = b
				}
			}
			_ = rows.Close()
		}
	}
	for _, id := range ids {
		if _, ok := out[id]; ok {
			continue
		}
		var sum string
		if c.db.QueryRowContext(ctx, `SELECT blob_sha256 FROM messages WHERE account = ? AND stable_id = ?`, account, id).Scan(&sum) != nil {
			continue
		}
		path := c.BlobPath(sum)
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out[id] = parseMessage(raw).BodyNew // recovers panics itself
	}
	return out
}

// SetOwners tells the threader the owner's own address(es) per account: mail
// from them is never marked an outsider and may join a thread by subject.
func (c *Cache) SetOwners(byAccount map[string][]string) {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	c.owners = map[string][]string{}
	for a, as := range byAccount {
		for _, x := range as {
			c.owners[a] = append(c.owners[a], strings.ToLower(strings.TrimSpace(x)))
		}
	}
}

func (c *Cache) threadOpts(account string) ThreadOpts {
	c.ownerMu.RLock()
	defer c.ownerMu.RUnlock()
	return ThreadOpts{Owners: c.owners[account]}
}

// safeThreadBatch is backfillThreadBatch with a net under it: a panic in the
// batch is logged, and the batch's messages are written as single-message
// threads and stepped over, so a poison message cannot crash-loop the process
// or be retried forever. Everything the batch did is rolled back first (its
// transaction is never committed).
func (c *Cache) safeThreadBatch(ctx context.Context, last int64, log *slog.Logger) (n int, next int64, finished bool, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		slog.Error("cache: thread backfill batch panicked; batch stepped over", "after_rowid", last, "panic", fmt.Sprint(r))
		n, next, finished, err = c.skipThreadBatch(ctx, last)
	}()
	return c.backfillThreadBatch(ctx, last)
}

func (c *Cache) skipThreadBatch(ctx context.Context, last int64) (int, int64, bool, error) {
	rs, err := c.db.QueryContext(ctx, `SELECT rowid, account, stable_id FROM messages WHERE rowid > ? AND `+notBulkThreaded+` ORDER BY rowid LIMIT ?`, last, threadBackfillBatch)
	if err != nil {
		return 0, last, false, fmt.Errorf("skip batch: %w", err)
	}
	type row struct {
		rid     int64
		account string
		id      string
	}
	var batch []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.rid, &r.account, &r.id); err != nil {
			_ = rs.Close()
			return 0, last, false, fmt.Errorf("skip batch: %w", err)
		}
		batch = append(batch, r)
	}
	_ = rs.Close()
	if len(batch) == 0 {
		return 0, last, true, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, last, false, fmt.Errorf("skip batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range batch {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_thread (account, stable_id, tid, parent_stable_id, depth, outsider) VALUES (?, ?, ?, '', 0, 0)`,
			r.account, r.id, soloTID(r.id)); err != nil {
			return 0, last, false, fmt.Errorf("skip batch: %w", err)
		}
	}
	next := batch[len(batch)-1].rid
	if err := setBackfillTx(ctx, tx, threadBackfillName, next, false, c.now(), len(batch)); err != nil {
		return 0, last, false, err
	}
	return len(batch), next, false, tx.Commit()
}

// OutlineInfo is what a thread outline line needs besides the indexed
// subject, sender and date.
type OutlineInfo struct {
	Text   string // the start of the cleaned body, up to outlineTextBytes
	HasAtt bool
}

const outlineTextBytes = 400

// ThreadOutline gives, for each message, the start of its cleaned text and
// whether it carries an attachment, without parsing blobs: the text is the
// head of message_fts2.body_new and the attachment flag comes from the
// attachments table. A message the text pipeline has not indexed is read
// header-only (Content-Type: multipart/mixed means attachments; no text).
func (c *Cache) ThreadOutline(ctx context.Context, account string, ids []string) map[string]OutlineInfo {
	out := map[string]OutlineInfo{}
	for _, part := range chunks(ids, idChunk) {
		rows, err := c.db.QueryContext(ctx, `
SELECT m.stable_id, substr(f.body_new, 1, ?),
       EXISTS (SELECT 1 FROM attachments a WHERE a.account = m.account AND a.stable_id = m.stable_id AND a.is_inline = 0)
FROM messages m JOIN message_fts2 f ON f.rowid = m.rowid
WHERE m.account = ? AND m.stable_id IN (`+inList(len(part))+`)`, append([]any{outlineTextBytes}, strArgs(account, part)...)...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			var oi OutlineInfo
			if rows.Scan(&id, &oi.Text, &oi.HasAtt) == nil {
				out[id] = oi
			}
		}
		_ = rows.Close()
	}
	for _, id := range ids {
		if _, ok := out[id]; ok {
			continue
		}
		var sum string
		if c.db.QueryRowContext(ctx, `SELECT blob_sha256 FROM messages WHERE account = ? AND stable_id = ?`, account, id).Scan(&sum) != nil {
			continue
		}
		out[id] = OutlineInfo{HasAtt: c.blobIsMixed(sum)}
	}
	return out
}

// blobIsMixed reads only the header block of a blob (panic-safe) and reports
// a multipart/mixed content type.
func (c *Cache) blobIsMixed(sum string) (mixed bool) {
	defer func() {
		if recover() != nil {
			mixed = false
		}
	}()
	path := c.BlobPath(sum)
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h, err := textproto.ReadHeader(bufio.NewReaderSize(f, 32<<10))
	if err != nil && h.Len() == 0 {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Type"))), "multipart/mixed")
}

// recipientAddrs returns the bare lower-cased addresses of two address-list
// headers, at most maxRecipients. An unparseable list yields what could be
// read (nothing): a missing recipient only makes a reply look more foreign.
func recipientAddrs(lists ...string) []string {
	var out []string
	for _, l := range lists {
		as, err := mail.ParseAddressList(l)
		if err != nil {
			continue
		}
		for _, a := range as {
			if len(out) >= maxRecipients {
				return out
			}
			out = append(out, strings.ToLower(a.Address))
		}
	}
	return out
}
