package cache

// The read side of the cache: everything the read tools answer from. None of
// it writes to the index, and none of it reaches the mailbox.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

var (
	// ErrNotFound means no message with that stable id exists for the account.
	ErrNotFound = errors.New("message not found in cache")
	// ErrBlobMissing means the index knows the message but its blob is gone
	// from disk, so the cache needs repairing (a refresh does not re-fetch it).
	ErrBlobMissing = errors.New("message body is missing from the cache on disk")
	// ErrQuerySyntax is a rejected FTS5 expression (fts_syntax mode only).
	ErrQuerySyntax = errors.New("invalid full-text query")
)

const (
	defaultLimit = 50
	maxLimit     = 500
)

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	}
	return n
}

// dateCol is a message's date: the Date header, else the time the server
// received it, for the odd message whose header is missing or unparseable.
const dateCol = `(CASE WHEN m.date_unix > 0 THEN m.date_unix ELSE m.internal_date END)`

// FolderCount is how many messages the cache holds in one folder.
type FolderCount struct {
	Name   string `json:"name"`
	Cached int    `json:"cached"`
}

// FolderCounts lists the folders the cache knows for an account with the
// number of cached messages in each, from the index alone.
func (c *Cache) FolderCounts(ctx context.Context, account string) ([]FolderCount, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT f.folder, COUNT(s.uid)
FROM folders f
LEFT JOIN membership s ON s.account = f.account AND s.folder = f.folder
WHERE f.account = ?
GROUP BY f.folder
ORDER BY f.folder`, account)
	if err != nil {
		return nil, fmt.Errorf("cache: folder counts: %w", err)
	}
	defer rows.Close()
	out := []FolderCount{}
	for rows.Next() {
		var f FolderCount
		if err := rows.Scan(&f.Name, &f.Cached); err != nil {
			return nil, fmt.Errorf("cache: folder counts: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SearchQuery is one search. Zero fields mean "no constraint".
type SearchQuery struct {
	Account   string // empty: every account
	Text      string
	FTSSyntax bool // Text is an FTS5 expression, passed through verbatim
	Folder    string
	From      string
	Since     time.Time // inclusive
	Until     time.Time // exclusive
	Limit     int
}

// SearchHit is one message a search found.
type SearchHit struct {
	Account  string    `json:"account"`
	StableID string    `json:"stable_id"`
	Date     time.Time `json:"date"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
	Folders  []string  `json:"folders"`
	Snippet  string    `json:"snippet,omitempty"`
}

// ftsPhrases turns free text into an FTS5 expression that can only mean "all
// of these words": each whitespace-separated term becomes a quoted string
// (inside which FTS5 treats "*", "-", "NEAR", "col:" and so on as plain
// punctuation) and the terms are ANDed. A term with no letter or digit would
// tokenise to nothing and is dropped. ok is false when nothing is left.
func ftsPhrases(text string) (expr string, ok bool) {
	var terms []string
	for _, t := range strings.Fields(text) {
		// A NUL ends the SQL string early inside the driver; it is never
		// part of a word.
		t = strings.ReplaceAll(t, "\x00", "")
		if !strings.ContainsFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " AND "), len(terms) > 0
}

// likeEscaper makes s literal inside a LIKE pattern with ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Bounds on what a search may ask for, so one call cannot be made expensive.
const (
	maxQueryBytes   = 512
	maxQueryTerms   = 32
	maxFromBytes    = 256
	searchTimeout   = 5 * time.Second
	maxAddrsListed  = 50
	listedAddrsNote = "+%d more"
)

// MaxQueryBytes is the longest search query accepted, by the cache search and
// by the server-side (X-GM-RAW) search alike.
const MaxQueryBytes = maxQueryBytes

// ErrQueryLimit is a search refused for its size. Its message is safe to show.
var ErrQueryLimit = errors.New("query too large")

// Search answers from the index only. It returns at most q.Limit hits (newest
// first) and reports whether more matched than were returned. It gives up
// after searchTimeout.
func (c *Cache) Search(ctx context.Context, q SearchQuery) ([]SearchHit, bool, error) {
	limit := clampLimit(q.Limit)
	text := strings.TrimSpace(q.Text)
	if len(text) > maxQueryBytes {
		return nil, false, fmt.Errorf("%w: query is longer than %d bytes", ErrQueryLimit, maxQueryBytes)
	}
	if len(q.From) > maxFromBytes {
		return nil, false, fmt.Errorf("%w: from filter is longer than %d bytes", ErrQueryLimit, maxFromBytes)
	}
	var expr string
	if text != "" {
		expr = text
		if !q.FTSSyntax {
			if len(strings.Fields(text)) > maxQueryTerms {
				return nil, false, fmt.Errorf("%w: query has more than %d words", ErrQueryLimit, maxQueryTerms)
			}
			var ok bool
			if expr, ok = ftsPhrases(text); !ok {
				return []SearchHit{}, false, nil // nothing searchable in the query: nothing matches
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	// Filters, shared by both shapes of the query; m is messages.
	var (
		filt []string
		fa   []any
	)
	if q.Account != "" {
		filt = append(filt, `m.account = ?`)
		fa = append(fa, q.Account)
	}
	if q.Folder != "" {
		filt = append(filt, `EXISTS (SELECT 1 FROM membership s WHERE s.account = m.account AND s.stable_id = m.stable_id AND s.folder = ?)`)
		fa = append(fa, q.Folder)
	}
	if q.From != "" {
		// Known limitation: SQLite's lower() folds ASCII only, so a From
		// with non-ASCII letters matches case-sensitively.
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

	var (
		query string
		args  []any
	)
	if expr == "" {
		// No full-text part: nothing to snippet, so one bounded pass.
		query = `SELECT m.account, m.stable_id, ` + dateCol + `, m.from_addr, m.subject, '' FROM messages m WHERE 1=1` + and +
			` ORDER BY ` + dateCol + ` DESC, m.account, m.stable_id LIMIT ?`
		args = append(append(args, fa...), limit+1)
	} else {
		// The FTS table is the base of the inner query so that MATCH drives
		// the scan; starting from messages would walk every message and test
		// MATCH per row. The inner query picks the surviving rows (by rowid,
		// with one extra to detect truncation) and only then does the outer
		// query pay for snippet() and the wide columns, looking each row up
		// by rowid. One extra row tells "exactly limit" from "more than limit".
		query = `WITH hit AS (
	SELECT message_fts.rowid AS rid, m.account AS account, m.stable_id AS stable_id, ` + dateCol + ` AS d
	FROM message_fts JOIN messages m ON m.account = message_fts.account AND m.stable_id = message_fts.stable_id
	WHERE message_fts MATCH ?` + and + `
	ORDER BY d DESC, m.account, m.stable_id LIMIT ?
)
SELECT hit.account, hit.stable_id, hit.d, m.from_addr, m.subject, snippet(message_fts, 4, '[', ']', '…', 20)
FROM hit
JOIN message_fts ON message_fts.rowid = hit.rid
JOIN messages m ON m.account = hit.account AND m.stable_id = hit.stable_id
WHERE message_fts MATCH ?
ORDER BY hit.d DESC, hit.account, hit.stable_id`
		args = append(append([]any{expr}, fa...), limit+1, expr)
	}

	queryErr := func(err error) error {
		if q.FTSSyntax && text != "" {
			return fmt.Errorf("%w: %v", ErrQuerySyntax, err)
		}
		return fmt.Errorf("cache: search: %w", err)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, queryErr(err)
	}
	defer rows.Close()

	hits := []SearchHit{}
	for rows.Next() {
		var (
			h    SearchHit
			unix int64
		)
		if err := rows.Scan(&h.Account, &h.StableID, &unix, &h.From, &h.Subject, &h.Snippet); err != nil {
			return nil, false, queryErr(err)
		}
		if unix > 0 {
			h.Date = time.Unix(unix, 0).UTC()
		}
		hits = append(hits, h)
	}
	// FTS5 reports a bad expression while stepping, not when preparing.
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("cache: search: %w", ctx.Err())
		}
		return nil, false, queryErr(err)
	}
	_ = rows.Close()

	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	for i := range hits {
		fs, err := c.messageFolders(ctx, hits[i].Account, hits[i].StableID)
		if err != nil {
			return nil, false, err
		}
		hits[i].Folders = fs
	}
	return hits, truncated, nil
}

// HitsByUID resolves UIDs of one folder (as a server-side search returned
// them) to cached messages through membership. UIDs the cache does not hold
// are counted in uncached, not looked up. Hits are newest first, at most
// limit; truncated says more were found than returned.
func (c *Cache) HitsByUID(ctx context.Context, account, folder string, uids []imap.UID, limit int) (hits []SearchHit, truncated bool, uncached int, err error) {
	limit = clampLimit(limit)
	byID := map[string]SearchHit{}
	// resolved counts membership rows found, one per UID, so uncached is per
	// UID too (several UIDs never share a stable id in one folder, but the
	// count would be right if they did).
	resolved := 0
	const chunk = 500
	for start := 0; start < len(uids); start += chunk {
		end := min(start+chunk, len(uids))
		args := []any{account, folder}
		marks := make([]string, 0, end-start)
		for _, u := range uids[start:end] {
			marks = append(marks, "?")
			args = append(args, uint32(u))
		}
		rows, qerr := c.db.QueryContext(ctx, `
SELECT m.account, m.stable_id, `+dateCol+`, m.from_addr, m.subject
FROM membership s JOIN messages m ON m.account = s.account AND m.stable_id = s.stable_id
WHERE s.account = ? AND s.folder = ? AND s.uid IN (`+strings.Join(marks, ",")+`)`, args...)
		if qerr != nil {
			return nil, false, 0, fmt.Errorf("cache: hits by uid: %w", qerr)
		}
		for rows.Next() {
			var (
				h    SearchHit
				unix int64
			)
			if serr := rows.Scan(&h.Account, &h.StableID, &unix, &h.From, &h.Subject); serr != nil {
				_ = rows.Close()
				return nil, false, 0, fmt.Errorf("cache: hits by uid: %w", serr)
			}
			if unix > 0 {
				h.Date = time.Unix(unix, 0).UTC()
			}
			byID[h.StableID] = h
			resolved++
		}
		if rerr := rows.Err(); rerr != nil {
			_ = rows.Close()
			return nil, false, 0, fmt.Errorf("cache: hits by uid: %w", rerr)
		}
		_ = rows.Close()
	}
	uncached = len(uids) - resolved

	hits = make([]SearchHit, 0, len(byID))
	for _, h := range byID {
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].Date.Equal(hits[j].Date) {
			return hits[i].Date.After(hits[j].Date)
		}
		return hits[i].StableID < hits[j].StableID
	})
	if truncated = len(hits) > limit; truncated {
		hits = hits[:limit]
	}
	for i := range hits {
		fs, ferr := c.messageFolders(ctx, hits[i].Account, hits[i].StableID)
		if ferr != nil {
			return nil, false, 0, ferr
		}
		hits[i].Folders = fs
	}
	return hits, truncated, uncached, nil
}

// addrsLimited renders an address header like addrs, but lists at most
// maxAddrsListed addresses and says how many it left out.
func addrsLimited(h gomail.Header, key string) string {
	l, err := h.AddressList(key)
	if err != nil || len(l) == 0 {
		return strings.TrimSpace(h.Get(key))
	}
	more := 0
	if len(l) > maxAddrsListed {
		more = len(l) - maxAddrsListed
		l = l[:maxAddrsListed]
	}
	parts := make([]string, 0, len(l)+1)
	for _, a := range l {
		parts = append(parts, a.String())
	}
	if more > 0 {
		parts = append(parts, fmt.Sprintf(listedAddrsNote, more))
	}
	return strings.Join(parts, ", ")
}

func (c *Cache) messageFolders(ctx context.Context, account, stableID string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT DISTINCT folder FROM membership WHERE account = ? AND stable_id = ? ORDER BY folder`, account, stableID)
	if err != nil {
		return nil, fmt.Errorf("cache: folders of message: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("cache: folders of message: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Attachment describes a message part that is not body text. Never content.
type Attachment struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// Message is one cached message, parsed for reading. Every string in it was
// written by the sender (or whoever forged the sender).
type Message struct {
	Account     string       `json:"account"`
	StableID    string       `json:"stable_id"`
	From        string       `json:"from"`
	To          string       `json:"to"`
	Cc          string       `json:"cc,omitempty"`
	Date        string       `json:"date,omitempty"`
	Subject     string       `json:"subject"`
	MessageID   string       `json:"message_id,omitempty"`
	ListID      string       `json:"list_id,omitempty"`
	GitHub      string       `json:"x_github_reason,omitempty"`
	Folders     []string     `json:"folders"`
	Body        string       `json:"-"`
	Truncated   bool         `json:"-"`
	Attachments []Attachment `json:"attachments"`
}

// maxAttachments bounds the attachment listing for a hostile MIME tree.
const maxAttachments = 200

// ReadMessage loads one message from the cache. The blob is located from the
// index row for (account, stableID) and from nothing else, so a stable id
// that belongs to another account, or to no message, is ErrNotFound and no
// caller-supplied string ever becomes a path. The text body is cut to
// maxBody bytes on a UTF-8 boundary.
func (c *Cache) ReadMessage(ctx context.Context, account, stableID string, maxBody int) (*Message, error) {
	var sum string
	err := c.db.QueryRowContext(ctx,
		`SELECT blob_sha256 FROM messages WHERE account = ? AND stable_id = ?`, account, stableID).Scan(&sum)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("cache: read message: %w", err)
	}
	path := c.BlobPath(sum)
	if path == "" {
		return nil, ErrBlobMissing
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrBlobMissing
		}
		return nil, fmt.Errorf("cache: read blob: %w", err)
	}
	folders, err := c.messageFolders(ctx, account, stableID)
	if err != nil {
		return nil, err
	}

	m := &Message{Account: account, StableID: stableID, Folders: folders, Attachments: []Attachment{}}
	e, perr := message.Read(bytes.NewReader(raw))
	if e == nil || (perr != nil && !message.IsUnknownCharset(perr) && !message.IsUnknownEncoding(perr)) {
		return m, nil // unparseable: still a message, with no headers or body
	}
	h := gomail.Header{Header: e.Header}
	m.From, m.To, m.Cc = addrsLimited(h, "From"), addrsLimited(h, "To"), addrsLimited(h, "Cc")
	m.Subject, _ = h.Subject()
	if m.Subject == "" {
		m.Subject = e.Header.Get("Subject")
	}
	if d, err := h.Date(); err == nil {
		m.Date = d.UTC().Format(time.RFC3339)
	} else {
		m.Date = strings.TrimSpace(e.Header.Get("Date"))
	}
	m.MessageID = strings.TrimSpace(e.Header.Get("Message-Id"))
	m.ListID = strings.TrimSpace(e.Header.Get("List-Id"))
	m.GitHub = strings.TrimSpace(e.Header.Get("X-Github-Reason"))

	var plain, htm strings.Builder
	var atts []Attachment
	// collectText and collectAttachments each consume the part bodies, so
	// each gets its own parse of the same bytes.
	collectText(e, &plain, &htm, 0)
	if e2, err := message.Read(bytes.NewReader(raw)); e2 != nil && (err == nil || message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)) {
		collectAttachments(e2, &atts, 0)
	}
	m.Attachments = append(m.Attachments, atts...)

	body := strings.TrimSpace(plain.String())
	if body == "" {
		body = stripHTML(htm.String())
	}
	m.Truncated = plain.Len() >= maxIndexedText || htm.Len() >= maxIndexedText
	if maxBody > 0 && len(body) > maxBody {
		body = strings.ToValidUTF8(body[:maxBody], "")
		m.Truncated = true
	}
	m.Body = body
	return m, nil
}

// collectAttachments lists the non-body leaves of the MIME tree: anything
// marked as an attachment or carrying a filename, plus any part that is not
// text. Sizes are of the decoded content.
func collectAttachments(e *message.Entity, out *[]Attachment, depth int) {
	if depth > 16 || len(*out) >= maxAttachments {
		return
	}
	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if err != nil && (p == nil || errors.Is(err, io.EOF) || !message.IsUnknownCharset(err)) {
				return
			}
			collectAttachments(p, out, depth+1)
		}
	}
	mt, mp, err := e.Header.ContentType()
	if err != nil {
		mt = "text/plain"
	}
	disp, dp, _ := e.Header.ContentDisposition()
	name := dp["filename"]
	if name == "" {
		name = mp["name"]
	}
	isText := mt == "text/plain" || mt == "text/html"
	if disp != "attachment" && name == "" && isText {
		return
	}
	n, _ := io.Copy(io.Discard, e.Body)
	*out = append(*out, Attachment{Filename: strings.ToValidUTF8(name, ""), ContentType: mt, Size: n})
}

// Sender is the activity of one sender address in an account.
type Sender struct {
	Address      string   `json:"address"`
	Name         string   `json:"name,omitempty"`
	Count        int      `json:"count"`
	First        string   `json:"first"`
	Last         string   `json:"last"`
	SubjectShape []string `json:"subject_shape"`
}

// senderAddrSQL pulls the bare, lowercased address out of from_addr, which is
// "Name <addr>", "<addr>" or a bare "addr". A display name that itself holds
// a "<" would fool it; such a sender is grouped under a wrong-looking key,
// which is a cosmetic fault and never an unsafe one.
const senderAddrSQL = `lower(CASE WHEN instr(m.from_addr, '<') > 0
	THEN substr(m.from_addr, instr(m.from_addr, '<') + 1, instr(m.from_addr, '>') - instr(m.from_addr, '<') - 1)
	ELSE trim(m.from_addr) END)`

// SenderStats ranks the senders in one account by message count. One query
// groups by (sender, raw From, subject) for the busiest limit senders; the
// rest -- display name, date range, subject shapes -- is folded in Go.
func (c *Cache) SenderStats(ctx context.Context, account, folder string, since, until time.Time, limit int) ([]Sender, error) {
	limit = clampLimit(limit)
	where := `m.account = ?`
	args := []any{account}
	if folder != "" {
		where += ` AND EXISTS (SELECT 1 FROM membership s WHERE s.account = m.account AND s.stable_id = m.stable_id AND s.folder = ?)`
		args = append(args, folder)
	}
	if !since.IsZero() {
		where += ` AND ` + dateCol + ` >= ?`
		args = append(args, since.Unix())
	}
	if !until.IsZero() {
		where += ` AND ` + dateCol + ` < ?`
		args = append(args, until.Unix())
	}
	args = append(args, limit)

	rows, err := c.db.QueryContext(ctx, `
WITH base AS (
	SELECT `+senderAddrSQL+` AS addr, m.from_addr AS raw, m.subject AS subject, `+dateCol+` AS d
	FROM messages m WHERE `+where+`
), top AS (
	SELECT addr FROM base GROUP BY addr ORDER BY COUNT(*) DESC, addr LIMIT ?
)
SELECT b.addr, b.raw, b.subject, COUNT(*), MIN(b.d), MAX(b.d)
FROM base b JOIN top t ON t.addr = b.addr
GROUP BY b.addr, b.raw, b.subject`, args...)
	if err != nil {
		return nil, fmt.Errorf("cache: sender stats: %w", err)
	}
	defer rows.Close()

	type acc struct {
		count       int
		first, last int64
		names       map[string]int
		shapes      map[string]int
	}
	by := map[string]*acc{}
	for rows.Next() {
		var (
			addr, raw, subject string
			n                  int
			lo, hi             int64
		)
		if err := rows.Scan(&addr, &raw, &subject, &n, &lo, &hi); err != nil {
			return nil, fmt.Errorf("cache: sender stats: %w", err)
		}
		a := by[addr]
		if a == nil {
			a = &acc{first: lo, last: hi, names: map[string]int{}, shapes: map[string]int{}}
			by[addr] = a
		}
		a.count += n
		a.first, a.last = min(a.first, lo), max(a.last, hi)
		if p, err := mail.ParseAddress(raw); err == nil && p.Name != "" {
			a.names[p.Name] += n
		}
		a.shapes[SubjectShape(subject)] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache: sender stats: %w", err)
	}

	out := make([]Sender, 0, len(by))
	for addr, a := range by {
		s := Sender{Address: addr, Count: a.count, SubjectShape: topKeys(a.shapes, 3)}
		if n := topKeys(a.names, 1); len(n) > 0 {
			s.Name = n[0]
		}
		if a.first > 0 {
			s.First = time.Unix(a.first, 0).UTC().Format(time.RFC3339)
		}
		if a.last > 0 {
			s.Last = time.Unix(a.last, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Address < out[j].Address
	})
	return out, nil
}

// topKeys returns the n keys with the highest counts, ties broken by key.
func topKeys(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

var (
	replyPrefix = regexp.MustCompile(`(?i)^\s*((re|fwd?)\s*:\s*)+`)
	uuidLike    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	hexLike     = regexp.MustCompile(`(?i)\b[0-9a-f]{8,}\b`)
	digitRun    = regexp.MustCompile(`[0-9]+`)
	spaceRun    = regexp.MustCompile(`\s+`)
)

// SubjectShape reduces a subject to what a template would leave constant:
// Re:/Fwd: stripped, uuids and long hex tokens become "…", digit runs "#".
// "Build 4812 failed" and "Build 4813 failed" share one shape.
func SubjectShape(s string) string {
	s = replyPrefix.ReplaceAllString(s, "")
	s = uuidLike.ReplaceAllString(s, "…")
	s = hexLike.ReplaceAllStringFunc(s, func(t string) string {
		// A long run of letters a-f ("defaced") is a word, not an id.
		if digitRun.MatchString(t) {
			return "…"
		}
		return t
	})
	s = digitRun.ReplaceAllString(s, "#")
	s = strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}
