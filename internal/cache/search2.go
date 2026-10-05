package cache

// Search v2: results grouped by thread, with facets and paging. It shares the
// full-text table and the filters with Search but is a separate function so
// that Search keeps its exact behaviour (and its callers: organise, tests).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Facet names accepted by SearchV2.
var FacetNames = []string{"sender", "domain", "month", "list_id"}

const (
	maxFacetValues = 10
	// maxPageLimit is the largest page SearchV2 returns.
	maxPageLimit = 100
)

// SearchOptions is a SearchV2 call. SearchQuery.Limit is the page size
// (default 20, at most 100).
type SearchOptions struct {
	SearchQuery
	GroupBy string   // "thread" (default) or "message"
	Facets  []string // subset of FacetNames
	Offset  int
}

// FacetValue is one value of a facet and how many matching messages have it.
type FacetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// ThreadHit is one thread a search matched.
type ThreadHit struct {
	TID          string
	Account      string
	Subject      string
	LastAt       time.Time
	NMsgs        int      // messages in the thread (not only matching ones)
	Matched      int      // messages of the thread that matched
	Participants []string // every participant the thread row keeps
	Snippet      string
	TopStableID  string // the newest matching message
	Folders      []string
}

// SearchResult is what SearchV2 returns. Exactly one of Threads and Messages
// is set, by GroupBy.
type SearchResult struct {
	Total     int // threads or messages matched, whichever was asked
	Facets    map[string][]FacetValue
	FacetNote string
	Threads   []ThreadHit
	Messages  []SearchHit
}

// matchPlan is the matching set of a search as a list of CTEs ending in
// "match" with columns rid, account, stable_id, d, from_addr, list_id,
// subject, att. att is 1 for a message found only through an attachment
// file name (fts2 only), which ranks below every body hit; for those rows rid
// is the attachment_fts rowid, otherwise the full-text rowid of the message.
type matchPlan struct {
	ctes   string
	args   []any
	empty  bool   // nothing searchable in the query: nothing matches
	expr   string // the full-text expression, "" when the query has no text
	fts2   bool
	table  string
	hasAtt bool // the attachment branch is part of the set
}

func (c *Cache) matchSQL(q SearchQuery) (matchPlan, error) {
	var p matchPlan
	text := strings.TrimSpace(q.Text)
	if len(text) > maxQueryBytes {
		return p, fmt.Errorf("%w: query is longer than %d bytes", ErrQueryLimit, maxQueryBytes)
	}
	if len(q.From) > maxFromBytes {
		return p, fmt.Errorf("%w: from filter is longer than %d bytes", ErrQueryLimit, maxFromBytes)
	}
	if text != "" {
		p.expr = text
		if !q.FTSSyntax {
			if len(strings.Fields(text)) > maxQueryTerms {
				return p, fmt.Errorf("%w: query has more than %d words", ErrQueryLimit, maxQueryTerms)
			}
			var ok bool
			if p.expr, ok = ftsPhrases(text); !ok {
				p.empty = true
				return p, nil
			}
		}
	}
	var filt []string
	var fa []any
	if q.Account != "" {
		filt = append(filt, `m.account = ?`)
		fa = append(fa, q.Account)
	}
	if q.Folder != "" {
		filt = append(filt, `EXISTS (SELECT 1 FROM membership s WHERE s.account = m.account AND s.stable_id = m.stable_id AND s.folder = ?)`)
		fa = append(fa, q.Folder)
	}
	if q.From != "" {
		filt = append(filt, `lower(m.from_addr) LIKE ? ESCAPE '\'`)
		fa = append(fa, "%"+likeEscaper.Replace(strings.ToLower(q.From))+"%")
	}
	if !q.Since.IsZero() {
		filt = append(filt, dateCol+` >= ?`)
		fa = append(fa, q.Since.Unix())
	}
	if !q.Until.IsZero() {
		filt = append(filt, dateCol+` < ?`)
		fa = append(fa, q.Until.Unix())
	}
	and := ""
	for _, f := range filt {
		and += ` AND ` + f
	}
	cols := `m.account AS account, m.stable_id AS stable_id, ` + dateCol + ` AS d, m.from_addr AS from_addr, m.list_id AS list_id, m.subject AS subject`
	if p.expr == "" {
		p.ctes = `match AS (SELECT m.rowid AS rid, ` + cols + `, 0 AS att FROM messages m WHERE 1=1` + and + `)`
		p.args = fa
		return p, nil
	}
	table, fts2 := c.FTSTable()
	p.table, p.fts2 = table, fts2
	if !fts2 {
		p.ctes = `match AS (SELECT ` + table + `.rowid AS rid, ` + cols + `, 0 AS att FROM ` + table + ` JOIN messages m ON m.account = ` + table + `.account AND m.stable_id = ` + table + `.stable_id
WHERE ` + table + ` MATCH ?` + and + `)`
		p.args = append([]any{p.expr}, fa...)
		return p, nil
	}
	if q.FTSSyntax {
		// The user's own column filters would break on attachment_fts, so the
		// attachment branch is left out; "body:" maps to both body columns.
		p.expr = rewriteBodyFilter(p.expr)
		p.ctes = `match AS (SELECT message_fts2.rowid AS rid, ` + cols + `, 0 AS att FROM message_fts2 JOIN messages m ON m.rowid = message_fts2.rowid
WHERE message_fts2 MATCH ?` + and + `)`
		p.args = append([]any{p.expr}, fa...)
		return p, nil
	}
	p.hasAtt = true
	p.ctes = `mh AS MATERIALIZED (SELECT message_fts2.rowid AS rid, ` + cols + `, 0 AS att FROM message_fts2 JOIN messages m ON m.rowid = message_fts2.rowid
WHERE message_fts2 MATCH ?` + and + `),
ah AS (SELECT MIN(attachment_fts.rowid) AS rid, m.account AS account, m.stable_id AS stable_id, ` + dateCol + ` AS d, m.from_addr AS from_addr, m.list_id AS list_id, m.subject AS subject, 1 AS att
FROM attachment_fts JOIN messages m ON m.account = attachment_fts.account AND m.stable_id = attachment_fts.stable_id
WHERE attachment_fts MATCH ?` + and + `
AND NOT EXISTS (SELECT 1 FROM mh WHERE mh.account = m.account AND mh.stable_id = m.stable_id)
GROUP BY m.account, m.stable_id),
match AS (SELECT * FROM mh UNION ALL SELECT * FROM ah)`
	p.args = append(append(append([]any{p.expr}, fa...), p.expr), fa...)
	return p, nil
}

// snippets returns snippet text for the page's rows: key is rid*2+att. Message
// rows take it from the body (fts2: body_new, else body_full when only that
// matched); attachment-only rows say "attachment: " and the file name.
func (c *Cache) snippets(ctx context.Context, p matchPlan, rows []snipRef) map[int64]string {
	out := map[int64]string{}
	if p.expr == "" || len(rows) == 0 {
		return out
	}
	run := func(q string, rids []int64) {
		if len(rids) == 0 {
			return
		}
		args := []any{p.expr}
		marks := make([]string, len(rids))
		for i, r := range rids {
			marks[i] = "?"
			args = append(args, r)
		}
		rs, err := c.db.QueryContext(ctx, strings.ReplaceAll(q, "?IDS", strings.Join(marks, ",")), args...)
		if err != nil {
			return // a snippet is decoration: never fail the search for it
		}
		defer rs.Close()
		for rs.Next() {
			var rid int64
			var sn string
			if rs.Scan(&rid, &sn) == nil {
				out[rid*2+int64(attFlag(q))] = sn
			}
		}
	}
	var msg, att []int64
	for _, r := range rows {
		if r.att {
			att = append(att, r.rid)
		} else {
			msg = append(msg, r.rid)
		}
	}
	if p.fts2 {
		run(`SELECT rowid, replace(replace(CASE WHEN instr(a, char(1)) > 0 OR instr(b, char(1)) = 0 THEN a ELSE b END, char(1), '['), char(2), ']') -- msg
FROM (SELECT rowid, snippet(message_fts2, 4, char(1), char(2), '…', 20) AS a, snippet(message_fts2, 5, char(1), char(2), '…', 20) AS b
      FROM message_fts2 WHERE message_fts2 MATCH ? AND rowid IN (?IDS))`, msg)
		run(`SELECT rowid, 'attachment: ' || snippet(attachment_fts, 3, '[', ']', '…', 20) -- att
FROM attachment_fts WHERE attachment_fts MATCH ? AND rowid IN (?IDS)`, att)
	} else {
		run(`SELECT rowid, snippet(`+p.table+`, 4, '[', ']', '…', 20) -- msg
FROM `+p.table+` WHERE `+p.table+` MATCH ? AND rowid IN (?IDS)`, msg)
	}
	return out
}

type snipRef struct {
	rid int64
	att bool
}

// attFlag reads the "-- att" marker of a snippet query.
func attFlag(q string) int {
	if strings.Contains(q, "-- att") {
		return 1
	}
	return 0
}

// SearchV2 runs a grouped search. It takes at most searchTimeout in all;
// facets that do not fit in what is left are dropped with a note (the hits are
// never dropped for them).
func (c *Cache) SearchV2(ctx context.Context, o SearchOptions) (SearchResult, error) {
	res := SearchResult{}
	limit := o.Limit
	switch {
	case limit <= 0:
		limit = 20
	case limit > maxPageLimit:
		limit = maxPageLimit
	}
	o.Offset = max(o.Offset, 0)
	plan, err := c.matchSQL(o.SearchQuery)
	if err != nil {
		return res, err
	}
	if plan.empty {
		return res, nil
	}
	margs := plan.args
	hasText := strings.TrimSpace(o.Text) != ""
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	queryErr := func(err error) error {
		if ctx.Err() != nil {
			return fmt.Errorf("cache: search: %w", ctx.Err())
		}
		if o.FTSSyntax && hasText {
			return fmt.Errorf("%w: %v", ErrQuerySyntax, err)
		}
		return fmt.Errorf("cache: search: %w", err)
	}

	type page struct {
		rid      int64
		account  string
		stableID string
		tid      string
		date     int64
		matched  int
		from     string
		subject  string
		att      bool
	}
	var pg []page
	if o.GroupBy == "message" {
		rows, err := c.db.QueryContext(ctx, `WITH `+plan.ctes+`
SELECT rid, account, stable_id, d, from_addr, subject, att, COUNT(*) OVER () FROM match
ORDER BY att, d DESC, account, stable_id LIMIT ? OFFSET ?`, append(append([]any{}, margs...), limit, o.Offset)...)
		if err != nil {
			return res, queryErr(err)
		}
		for rows.Next() {
			var p page
			if err := rows.Scan(&p.rid, &p.account, &p.stableID, &p.date, &p.from, &p.subject, &p.att, &res.Total); err != nil {
				_ = rows.Close()
				return res, queryErr(err)
			}
			pg = append(pg, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return res, queryErr(err)
		}
		_ = rows.Close()
	} else {
		// One row per thread: the best matching message stands for it, a body
		// hit before an attachment-only hit, then the newest (SQLite takes the
		// bare columns from the row of the single MAX()). Threads rank the same way.
		rows, err := c.db.QueryContext(ctx, `WITH `+plan.ctes+`
SELECT tid, account, d, stable_id, rid, att, hitn, COUNT(*) OVER () FROM (
	SELECT COALESCE(mt.tid, 'u:' || x.stable_id) AS tid, x.account AS account, MAX((1 - x.att) * 1099511627776 + x.d) AS rk,
	       x.d AS d, x.stable_id AS stable_id, x.rid AS rid, x.att AS att, COUNT(*) AS hitn
	FROM match x LEFT JOIN message_thread mt ON mt.account = x.account AND mt.stable_id = x.stable_id
	GROUP BY x.account, COALESCE(mt.tid, 'u:' || x.stable_id)
) ORDER BY rk DESC, account, tid LIMIT ? OFFSET ?`, append(append([]any{}, margs...), limit, o.Offset)...)
		if err != nil {
			return res, queryErr(err)
		}
		for rows.Next() {
			var p page
			if err := rows.Scan(&p.tid, &p.account, &p.date, &p.stableID, &p.rid, &p.att, &p.matched, &res.Total); err != nil {
				_ = rows.Close()
				return res, queryErr(err)
			}
			pg = append(pg, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return res, queryErr(err)
		}
		_ = rows.Close()
	}

	if len(pg) == 0 && o.Offset > 0 {
		// A page past the end has no row to carry COUNT(*) OVER (): count apart.
		q := `WITH ` + plan.ctes + ` SELECT COUNT(*) FROM match`
		if o.GroupBy != "message" {
			q = `WITH ` + plan.ctes + ` SELECT COUNT(*) FROM (SELECT 1 FROM match x LEFT JOIN message_thread mt ON mt.account = x.account AND mt.stable_id = x.stable_id
GROUP BY x.account, COALESCE(mt.tid, 'u:' || x.stable_id))`
		}
		if err := c.db.QueryRowContext(ctx, q, margs...).Scan(&res.Total); err != nil {
			return res, queryErr(err)
		}
	}
	srefs := make([]snipRef, len(pg))
	for i, p := range pg {
		srefs[i] = snipRef{p.rid, p.att}
	}
	snips := c.snippets(ctx, plan, srefs)
	snip := func(p page) string {
		k := p.rid * 2
		if p.att {
			k++
		}
		return snips[k]
	}

	if o.GroupBy == "message" {
		for _, p := range pg {
			h := SearchHit{Account: p.account, StableID: p.stableID, From: p.from, Subject: p.subject, Snippet: snip(p)}
			if p.date > 0 {
				h.Date = time.Unix(p.date, 0).UTC()
			}
			res.Messages = append(res.Messages, h)
		}
		if err := c.fillFolders(ctx, res.Messages); err != nil {
			return res, err
		}
	} else {
		refs := make([]threadRef, 0, len(pg))
		for _, p := range pg {
			refs = append(refs, threadRef{account: p.account, tid: p.tid, top: p.stableID, matched: p.matched, date: p.date, snippet: snip(p)})
		}
		hits, err := c.threadHits(ctx, refs)
		if err != nil {
			return res, err
		}
		res.Threads = hits
	}

	if len(o.Facets) > 0 && o.Offset == 0 {
		f, err := c.facets(ctx, plan.ctes, margs, o.Facets)
		switch {
		case err == nil:
			res.Facets = f
		case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
			res.FacetNote = "facets omitted: they did not fit in the 5 s search budget; narrow the query and ask again"
		default:
			return res, queryErr(err)
		}
	}
	return res, nil
}

type threadRef struct {
	account, tid, top string
	matched           int
	date              int64
	snippet           string
}

// threadHits fills ThreadHit for the given thread references, in their order.
// A tid with no threads row (not threaded yet, "u:" ids) is described by its
// top message alone.
func (c *Cache) threadHits(ctx context.Context, refs []threadRef) ([]ThreadHit, error) {
	type info struct {
		subject, people string
		last            int64
		n               int
	}
	infos := map[string]info{}
	byAcct := map[string][]string{}
	topsByAcct := map[string][]string{}
	for _, r := range refs {
		byAcct[r.account] = append(byAcct[r.account], r.tid)
		topsByAcct[r.account] = append(topsByAcct[r.account], r.top)
	}
	for acct, tids := range byAcct {
		for _, part := range chunks(tids, idChunk) {
			rows, err := c.db.QueryContext(ctx, `
SELECT t.tid, COALESCE(r.subject, ''), t.participants_json, t.last_at, t.n_msgs
FROM threads t LEFT JOIN messages r ON r.account = t.account AND r.stable_id = t.root_stable_id
WHERE t.account = ? AND t.tid IN (`+inList(len(part))+`)`, strArgs(acct, part)...)
			if err != nil {
				return nil, fmt.Errorf("cache: thread info: %w", err)
			}
			for rows.Next() {
				var tid string
				var in info
				if err := rows.Scan(&tid, &in.subject, &in.people, &in.last, &in.n); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("cache: thread info: %w", err)
				}
				infos[acct+"\x00"+tid] = in
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("cache: thread info: %w", err)
			}
			_ = rows.Close()
		}
	}
	tops := map[string]SearchHit{}
	for acct, ids := range topsByAcct {
		sums, err := c.Summaries(ctx, acct, ids)
		if err != nil {
			return nil, err
		}
		for _, s := range sums {
			tops[acct+"\x00"+s.StableID] = s
		}
	}
	all := make([]SearchHit, 0, len(refs))
	for _, r := range refs {
		all = append(all, SearchHit{Account: r.account, StableID: r.top})
	}
	if err := c.fillFolders(ctx, all); err != nil {
		return nil, err
	}
	out := make([]ThreadHit, 0, len(refs))
	for i, r := range refs {
		h := ThreadHit{TID: r.tid, Account: r.account, TopStableID: r.top, Matched: r.matched, Snippet: r.snippet, Folders: all[i].Folders}
		top := tops[r.account+"\x00"+r.top]
		h.Subject = top.Subject
		h.LastAt = time.Unix(r.date, 0).UTC()
		h.NMsgs = 1
		if in, ok := infos[r.account+"\x00"+r.tid]; ok {
			if in.subject != "" {
				h.Subject = in.subject
			}
			if in.last > 0 {
				h.LastAt = time.Unix(in.last, 0).UTC()
			}
			h.NMsgs = max(in.n, 1)
			_ = json.Unmarshal([]byte(in.people), &h.Participants)
		}
		if len(h.Participants) == 0 && top.From != "" {
			h.Participants = []string{personName(top.From)}
		}
		out = append(out, h)
	}
	return out, nil
}

// GroupHitsByThread turns message hits (a server search's) into thread hits,
// newest thread first. Message order within a thread is not kept: the newest
// hit stands for it.
func (c *Cache) GroupHitsByThread(ctx context.Context, hits []SearchHit) ([]ThreadHit, error) {
	tids := map[string]string{}
	byAcct := map[string][]string{}
	for _, h := range hits {
		byAcct[h.Account] = append(byAcct[h.Account], h.StableID)
	}
	for acct, ids := range byAcct {
		for _, part := range chunks(ids, idChunk) {
			rows, err := c.db.QueryContext(ctx, `SELECT stable_id, tid FROM message_thread WHERE account = ? AND stable_id IN (`+inList(len(part))+`)`, strArgs(acct, part)...)
			if err != nil {
				return nil, fmt.Errorf("cache: thread of hits: %w", err)
			}
			for rows.Next() {
				var s, t string
				if err := rows.Scan(&s, &t); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("cache: thread of hits: %w", err)
				}
				tids[acct+"\x00"+s] = t
			}
			_ = rows.Close()
		}
	}
	idx := map[string]int{}
	var refs []threadRef
	for _, h := range hits { // newest first
		tid, ok := tids[h.Account+"\x00"+h.StableID]
		if !ok {
			tid = "u:" + h.StableID
		}
		k := h.Account + "\x00" + tid
		if i, seen := idx[k]; seen {
			refs[i].matched++
			continue
		}
		idx[k] = len(refs)
		refs = append(refs, threadRef{account: h.Account, tid: tid, top: h.StableID, matched: 1, date: h.Date.Unix()})
	}
	return c.threadHits(ctx, refs)
}

var facetSQL = map[string]string{
	"sender":  `SELECT 'sender' AS k, addr AS v, COUNT(*) AS n FROM f WHERE addr <> '' GROUP BY addr ORDER BY n DESC, v LIMIT %d`,
	"domain":  `SELECT 'domain', substr(addr, instr(addr, '@') + 1) AS v, COUNT(*) AS n FROM f WHERE instr(addr, '@') > 0 GROUP BY v ORDER BY n DESC, v LIMIT %d`,
	"month":   `SELECT 'month', strftime('%%Y-%%m', d, 'unixepoch') AS v, COUNT(*) AS n FROM f WHERE d > 0 GROUP BY v ORDER BY v DESC LIMIT %d`,
	"list_id": `SELECT 'list_id', list_id AS v, COUNT(*) AS n FROM f WHERE list_id <> '' GROUP BY v ORDER BY n DESC, v LIMIT %d`,
}

// facets counts the matching messages by each requested facet, top values only,
// in one pass over the matching set.
func (c *Cache) facets(ctx context.Context, ctes string, margs []any, names []string) (map[string][]FacetValue, error) {
	var parts []string
	seen := map[string]bool{}
	for _, n := range names {
		tmpl, ok := facetSQL[n]
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		parts = append(parts, `SELECT * FROM (`+fmt.Sprintf(tmpl, maxFacetValues)+`)`)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	addr := strings.ReplaceAll(senderAddrSQL, "m.from_addr", "from_addr")
	rows, err := c.db.QueryContext(ctx, `WITH `+ctes+`, f AS MATERIALIZED (SELECT `+addr+` AS addr, d, list_id FROM match) `+
		strings.Join(parts, ` UNION ALL `), margs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]FacetValue{}
	for rows.Next() {
		var k string
		var fv FacetValue
		if err := rows.Scan(&k, &fv.Value, &fv.Count); err != nil {
			return nil, err
		}
		out[k] = append(out[k], fv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, vs := range out {
		sort.SliceStable(vs, func(i, j int) bool {
			if k == "month" {
				return vs[i].Value > vs[j].Value
			}
			if vs[i].Count != vs[j].Count {
				return vs[i].Count > vs[j].Count
			}
			return vs[i].Value < vs[j].Value
		})
	}
	return out, nil
}
