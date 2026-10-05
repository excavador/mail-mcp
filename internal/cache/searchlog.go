package cache

// The search log answers one question: when the agent searches, does it then
// read what it found, or does it search again with other words? A search whose
// results were never fetched, followed soon by a similar search, is the signal
// that keyword search is missing something (and the trigger for adding vector
// search). The log is a table; the "was it fetched" part needs a short memory
// of what recent searches returned, which is a small in-memory ring.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// searchRingSize is how many recent searches are remembered.
	searchRingSize = 50
	// fetchWindow is how long after a search a fetch still counts as
	// "fetched what the search found".
	fetchWindow = 10 * time.Minute
	// reformulateWindow is how soon after a zero-fetch search a similar one
	// counts as a reformulation of it.
	reformulateWindow = 2 * time.Minute
	// statsMinSample is the fewest searches the verdict is given for.
	statsMinSample = 20
	// vectorTrigger is the share of searches that were abandoned and
	// reformulated above which vector search is warranted.
	vectorTrigger = 0.10
)

// SearchRecord is one search call, as the server reports it.
type SearchRecord struct {
	Account string
	Query   string // already sanitised and capped by the caller
	// Key identifies the query including its filters (not its page), so a
	// repeat or a next page of the same search is not mistaken for a new one.
	Key   string
	Mode  string // "thread", "message", "server"
	Page  bool   // a continuation (cursor) of an earlier search
	Hits  int
	Total int
	// Refs are what the search returned: stable ids and thread ids.
	Refs []ResultRef
}

// ResultRef names one result.
type ResultRef struct {
	Account  string
	StableID string
	TID      string
}

type searchEntry struct {
	id      int64
	at      time.Time
	key     string
	account string
	terms   map[string]bool
	stable  map[string]bool // account \x00 stable id
	tids    map[string]bool // account \x00 tid
	fetched bool
}

type searchTracker struct {
	mu   sync.Mutex
	ring []*searchEntry // oldest first
}

// QueryKey is a stable key for a query and its filters.
func QueryKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h[:8])
}

// queryTerms are the lower-cased words of a query, without operators.
func queryTerms(q string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch w {
		case "and", "or", "not", "near":
			continue
		}
		if len([]rune(w)) >= 2 {
			out[w] = true
		}
	}
	return out
}

// similar reports whether two term sets overlap by at least half of the
// smaller one.
func similar(a, b map[string]bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	n := 0
	for t := range a {
		if b[t] {
			n++
		}
	}
	return n > 0 && float64(n) >= 0.5*float64(min(len(a), len(b)))
}

// LogSearch records a search call. It returns the log row id (0 when the call
// is a page of an earlier search, which extends that search's results and is
// logged with a ":page" mode so statistics skip it).
func (c *Cache) LogSearch(ctx context.Context, r SearchRecord) (int64, error) {
	now := c.now()
	terms := queryTerms(r.Query)
	t := &c.searches
	t.mu.Lock()
	defer t.mu.Unlock()

	var reformOf sql.NullInt64
	var pageOf *searchEntry
	for i := len(t.ring) - 1; i >= 0; i-- {
		e := t.ring[i]
		if r.Page && e.key == r.Key && e.account == r.Account {
			pageOf = e
			break
		}
	}
	mode := r.Mode
	if r.Page {
		mode += ":page"
	} else {
		repeat := false
		for _, e := range t.ring {
			if e.key == r.Key && now.Sub(e.at) <= reformulateWindow {
				repeat = true // the same search again is a repeat, not new words
			}
		}
		for i := len(t.ring) - 1; i >= 0 && !repeat; i-- {
			e := t.ring[i]
			if now.Sub(e.at) > reformulateWindow {
				break
			}
			if !e.fetched && e.key != r.Key && e.id != 0 && similar(e.terms, terms) {
				reformOf = sql.NullInt64{Int64: e.id, Valid: true}
				break
			}
		}
	}
	res, err := c.db.ExecContext(ctx, `INSERT INTO search_log (at, account, query_hash, query, mode, hits, total, reformulated_of)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, now.Unix(), r.Account, r.Key, r.Query, mode, r.Hits, r.Total, reformOf)
	if err != nil {
		return 0, fmt.Errorf("cache: log search: %w", err)
	}
	id, _ := res.LastInsertId()

	add := func(e *searchEntry) {
		for _, ref := range r.Refs {
			if ref.StableID != "" {
				e.stable[ref.Account+"\x00"+ref.StableID] = true
			}
			if ref.TID != "" {
				e.tids[ref.Account+"\x00"+ref.TID] = true
			}
		}
	}
	if pageOf != nil {
		add(pageOf)
		return id, nil
	}
	e := &searchEntry{id: id, at: now, key: r.Key, account: r.Account, terms: terms,
		stable: map[string]bool{}, tids: map[string]bool{}}
	add(e)
	t.ring = append(t.ring, e)
	if len(t.ring) > searchRingSize {
		t.ring = t.ring[len(t.ring)-searchRingSize:]
	}
	return id, nil
}

// NoteFetch marks as fetched every search of the last ten minutes that
// returned this thread or any of these messages. tid may be empty when the
// caller has only a message: it is then looked up.
func (c *Cache) NoteFetch(ctx context.Context, account, tid string, stableIDs ...string) {
	t := &c.searches
	t.mu.Lock()
	n := len(t.ring)
	t.mu.Unlock()
	if n == 0 {
		return
	}
	if tid == "" && len(stableIDs) == 1 {
		_ = c.db.QueryRowContext(ctx, `SELECT tid FROM message_thread WHERE account = ? AND stable_id = ?`, account, stableIDs[0]).Scan(&tid)
	}
	now := c.now()
	var ids []int64
	t.mu.Lock()
	for _, e := range t.ring {
		if e.fetched || now.Sub(e.at) > fetchWindow {
			continue
		}
		hit := tid != "" && e.tids[account+"\x00"+tid]
		for _, id := range stableIDs {
			if hit {
				break
			}
			hit = e.stable[account+"\x00"+id] || e.tids[account+"\x00u:"+id]
		}
		if hit {
			e.fetched = true
			ids = append(ids, e.id)
		}
	}
	t.mu.Unlock()
	for _, id := range ids {
		_, _ = c.db.ExecContext(ctx, `UPDATE search_log SET fetched = 1 WHERE id = ?`, id)
	}
}

// SearchStats summarises the search log.
type SearchStats struct {
	Days        int     `json:"days"`
	Searches    int     `json:"searches"`
	ZeroFetch   int     `json:"zero_fetch"`
	Reformulate int     `json:"reformulated"`
	AbandonedRe int     `json:"zero_fetch_then_reformulated"`
	ZeroFetchP  float64 `json:"zero_fetch_share"`
	ReformP     float64 `json:"reformulated_share"`
	AbandonedP  float64 `json:"zero_fetch_reformulated_share"`
	Verdict     string  `json:"verdict"`
	// VectorSearch is true when the share of searches that were abandoned and
	// then reformulated is above the 10% trigger (and the sample is large
	// enough to say).
	VectorSearch bool `json:"vector_search_warranted"`
}

// SearchStatsSince reads the log for the last days days. Continuations of a
// search (mode ending ":page") are not searches and are left out.
func (c *Cache) SearchStatsSince(ctx context.Context, days int) (SearchStats, error) {
	if days <= 0 {
		days = 14
	}
	since := c.now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	st := SearchStats{Days: days}
	err := c.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(fetched = 0), 0),
       COALESCE(SUM(reformulated_of IS NOT NULL), 0),
       COALESCE(SUM(fetched = 0 AND id IN (SELECT reformulated_of FROM search_log WHERE reformulated_of IS NOT NULL AND at >= ?)), 0)
FROM search_log WHERE at >= ? AND mode NOT LIKE '%:page'`, since, since).Scan(&st.Searches, &st.ZeroFetch, &st.Reformulate, &st.AbandonedRe)
	if err != nil {
		return st, fmt.Errorf("cache: search stats: %w", err)
	}
	if st.Searches > 0 {
		n := float64(st.Searches)
		st.ZeroFetchP, st.ReformP, st.AbandonedP = float64(st.ZeroFetch)/n, float64(st.Reformulate)/n, float64(st.AbandonedRe)/n
	}
	switch {
	case st.Searches < statsMinSample:
		st.Verdict = fmt.Sprintf("insufficient data: %d searches in %d days, need %d", st.Searches, days, statsMinSample)
	case st.AbandonedP > vectorTrigger:
		st.VectorSearch = true
		st.Verdict = fmt.Sprintf("add vector search: %.0f%% of searches were abandoned (nothing fetched) and then reformulated, above the 10%% trigger", st.AbandonedP*100)
	default:
		st.Verdict = fmt.Sprintf("keyword search is enough for now: %.0f%% abandoned then reformulated, at or below the 10%% trigger", st.AbandonedP*100)
	}
	return st, nil
}
