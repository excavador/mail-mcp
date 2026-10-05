package cache

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/excavador/mail-mcp/internal/accounts"
)

var uidSeq atomic.Int64

func openCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func tctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ins writes one message straight into the index (messages, FTS, membership,
// folders), bypassing IMAP, so tests control every column. A zero date leaves
// date_unix 0, so the date falls back to internal_date (also zero then).
func ins(t *testing.T, c *Cache, account, id, from, subject, body string, date time.Time, folders ...string) {
	t.Helper()
	var d int64
	if !date.IsZero() {
		d = date.Unix()
	}
	if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, internal_date) VALUES (?,?,?,?,?,?,?)`,
		account, id, "", from, subject, d, d); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO message_fts (subject, from_addr, to_addr, cc_addr, body, account, stable_id) VALUES (?,?,?,?,?,?,?)`,
		subject, from, "", "", body, account, id); err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		if _, err := c.db.Exec(`INSERT OR IGNORE INTO folders (account, folder, uidvalidity) VALUES (?,?,1)`, account, f); err != nil {
			t.Fatal(err)
		}
		if _, err := c.db.Exec(`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES (?,?,?,?,1)`,
			account, id, f, uidSeq.Add(1)); err != nil {
			t.Fatal(err)
		}
	}
}

func search(t *testing.T, c *Cache, q SearchQuery) ([]SearchHit, bool) {
	t.Helper()
	hits, trunc, err := c.Search(tctx(t), q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return hits, trunc
}

func ids(hits []SearchHit) []string {
	out := []string{}
	for _, h := range hits {
		out = append(out, h.StableID)
	}
	return out
}

func ftsCorpus(t *testing.T) *Cache {
	c := openCache(t)
	ins(t, c, "a", "m1", "Alice <alice@example.com>", "Weekly report",
		`the foo-bar widget and a hello world; see subject:x for details`, t0, "INBOX")
	ins(t, c, "a", "m2", "Bob <bob@example.com>", "Prefixed things", `prefixed words and another message`, t0.Add(time.Hour), "INBOX")
	ins(t, c, "a", "m3", "Carol <carol@example.com>", "x marks", `nothing here`, t0.Add(2*time.Hour), "INBOX")
	return c
}

func TestFTSDefaultModeNeverErrors(t *testing.T) {
	c := ftsCorpus(t)
	for _, q := range []string{
		`"`, `""`, `"""`, `foo "bar`, `"hello`, `hello"`, `"hello"`, `*`, `hel*`, `*hello`,
		`NEAR(a b)`, `NEAR(foo bar, 2)`, `OR`, `AND`, `NOT`, `a OR b`, `a AND NOT b`, `NOT hello`,
		`-hello`, `-`, `--`, `subject:x`, `subject: x`, `body:hello`, `nosuchcol:x`, `{subject}:x`,
		`(`, `)`, `(a OR b)`, `((hello)`, `^hello`, `^`, `+`, `:`, `'`, `\`, `%`, `_`,
		`!!!`, `???`, `...`, `a:b:c`, `héllo`, `日本語`,
	} {
		t.Run(q, func(t *testing.T) {
			if _, _, err := c.Search(tctx(t), SearchQuery{Text: q}); err != nil {
				t.Fatalf("default mode errored on %q: %v", q, err)
			}
		})
	}
}

// A NUL byte in a term once ended the quoted FTS5 string early ("unterminated
// string"), so default mode errored.
func TestFTSDefaultNULByteDoesNotError(t *testing.T) {
	c := ftsCorpus(t)
	if _, _, err := c.Search(tctx(t), SearchQuery{Text: "hello\x00world"}); err != nil {
		t.Fatalf("default mode errored on NUL: %v", err)
	}
}

func TestFTSDefaultSpecialTermsAreLiteralWords(t *testing.T) {
	c := ftsCorpus(t)
	for q, want := range map[string][]string{
		`"hello"`:         {"m1"}, // quotes inside a term are escaped, not syntax
		`"hello`:          {"m1"}, // unbalanced quote
		`hello world`:     {"m1"}, // all terms must match
		`hello nosuch`:    {},     // AND, not OR
		`hello OR nosuch`: {},     // OR is a literal word here, never an operator
		`NOT hello`:       {},     // NOT is a literal word, not negation
		`hel*`:            {},     // * is not a prefix operator
		`(hello)`:         {"m1"}, // parens are punctuation
		`^hello`:          {"m1"}, // ^ is not an initial-token anchor error
		`-hello`:          {"m1"}, // leading - is not NOT
	} {
		hits, _ := search(t, c, SearchQuery{Text: q})
		if got := ids(hits); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v want %v", q, got, want)
		}
	}
}

func TestFTSDefaultColumnFilterIsLiteralNotRestriction(t *testing.T) {
	c := ftsCorpus(t)
	// m1 has the text "subject:x" only in its BODY; m3 has x in its SUBJECT.
	// A column filter would find m3 and miss m1; the literal phrase is the reverse.
	hits, _ := search(t, c, SearchQuery{Text: `subject:x`})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Fatalf("subject:x = %v, want [m1] (body text, not a subject restriction)", got)
	}
}

func TestFTSDefaultHyphenatedTermMatches(t *testing.T) {
	c := ftsCorpus(t)
	hits, _ := search(t, c, SearchQuery{Text: `foo-bar`})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Fatalf("foo-bar = %v, want [m1]", got)
	}
}

func TestFTSDefaultPunctuationOnlyIsEmptyNotError(t *testing.T) {
	c := ftsCorpus(t)
	for _, q := range []string{`!!!`, `"`, `* - ^ ( )`, `...`} {
		hits, trunc, err := c.Search(tctx(t), SearchQuery{Text: q})
		if err != nil || len(hits) != 0 || trunc || hits == nil {
			t.Errorf("%q: hits=%v trunc=%v err=%v, want empty non-nil, no error", q, hits, trunc, err)
		}
	}
}

func TestFTSSyntaxOperatorsWork(t *testing.T) {
	c := ftsCorpus(t)
	for q, want := range map[string][]string{
		`hello OR prefixed`: {"m2", "m1"}, // newest first
		`hello AND world`:   {"m1"},
		`hello NOT world`:   {},
		`pre*`:              {"m2"},
		`"hello world"`:     {"m1"},
		`NEAR(hello world)`: {"m1"},
	} {
		hits, _ := search(t, c, SearchQuery{Text: q, FTSSyntax: true})
		if got := ids(hits); !reflect.DeepEqual(got, want) {
			t.Errorf("fts %q: got %v want %v", q, got, want)
		}
	}
}

func TestFTSSyntaxColumnFilterRestricts(t *testing.T) {
	c := ftsCorpus(t)
	// x is in m3's subject; "x" appears in m1 only through the body text "subject:x".
	hits, _ := search(t, c, SearchQuery{Text: `subject:x`, FTSSyntax: true})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"m3"}) {
		t.Fatalf("fts subject:x = %v, want [m3]", got)
	}
}

func TestFTSSyntaxMalformedIsErrQuerySyntax(t *testing.T) {
	c := ftsCorpus(t)
	for _, q := range []string{`"unbalanced`, `AND`, `hello OR`, `(`, `)`, `nosuchcol:x`, `NEAR(`, `hello AND AND world`, `*`} {
		t.Run(q, func(t *testing.T) {
			_, _, err := c.Search(tctx(t), SearchQuery{Text: q, FTSSyntax: true})
			if !errors.Is(err, ErrQuerySyntax) {
				t.Fatalf("fts %q: err = %v, want ErrQuerySyntax", q, err)
			}
		})
	}
}

func TestFTSSyntaxOnlyAppliesToNonEmptyText(t *testing.T) {
	c := ftsCorpus(t)
	hits, _ := search(t, c, SearchQuery{FTSSyntax: true})
	if len(hits) != 3 {
		t.Fatalf("empty fts query with no filters = %d hits, want 3", len(hits))
	}
}

func TestSearchEmptyQueryWithFilters(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "m1", "Alice <alice@example.com>", "one", "b", t0, "INBOX")
	ins(t, c, "a", "m2", "Bob <bob@example.com>", "two", "b", t0.Add(time.Hour), "INBOX")
	hits, _ := search(t, c, SearchQuery{From: "bob"})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"m2"}) {
		t.Fatalf("empty query + from = %v", got)
	}
	hits, _ = search(t, c, SearchQuery{Text: "   "}) // whitespace-only is empty
	if len(hits) != 2 {
		t.Fatalf("blank query = %d hits, want 2", len(hits))
	}
	if hits[0].Snippet != "" {
		t.Errorf("snippet on a filter-only search: %q", hits[0].Snippet)
	}
}

func TestSearchFolderFilterAndFoldersListed(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "both", "x <x@e.com>", "s1", "hello", t0, "INBOX", "Archive", "Zed")
	ins(t, c, "a", "inbox", "x <x@e.com>", "s2", "hello", t0.Add(time.Hour), "INBOX")
	ins(t, c, "a", "arch", "x <x@e.com>", "s3", "hello", t0.Add(2*time.Hour), "Archive")

	hits, _ := search(t, c, SearchQuery{Text: "hello", Folder: "Archive"})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"arch", "both"}) {
		t.Fatalf("folder filter = %v", got)
	}
	// Folders lists every folder the message is in, not only the filtered one.
	for _, h := range hits {
		if h.StableID == "both" && !reflect.DeepEqual(h.Folders, []string{"Archive", "INBOX", "Zed"}) {
			t.Errorf("Folders = %v, want all three sorted", h.Folders)
		}
	}
	hits, _ = search(t, c, SearchQuery{Folder: "Nope"})
	if len(hits) != 0 {
		t.Errorf("unknown folder = %v", ids(hits))
	}
}

func TestSearchFromIsCaseInsensitiveAndLikeWildcardsAreLiteral(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "alice", "Alice Smith <alice@example.com>", "s", "b", t0)
	ins(t, c, "a", "pct", "Pct%Name <p@example.com>", "s", "b", t0.Add(time.Second))
	ins(t, c, "a", "us", "Under_Score <u@example.com>", "s", "b", t0.Add(2*time.Second))
	ins(t, c, "a", "nous", "Underxscore <n@example.com>", "s", "b", t0.Add(3*time.Second))
	for _, tc := range []struct {
		from string
		want []string
	}{
		{"ALICE SMITH", []string{"alice"}},
		{"aLiCe@ExAmPlE.CoM", []string{"alice"}},
		{"%", []string{"pct"}},        // would match everything if % were a wildcard
		{"r_s", []string{"us"}},       // would also match "rxs" if _ were a wildcard
		{"Pct%Name", []string{"pct"}}, //
		{`\`, []string{}},             // backslash is literal too
	} {
		hits, _ := search(t, c, SearchQuery{From: tc.from})
		if got := ids(hits); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("from %q = %v, want %v", tc.from, got, tc.want)
		}
	}
}

func TestSearchSinceInclusiveUntilExclusive(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "early", "a <a@e.com>", "s", "b", t0.Add(-time.Second))
	ins(t, c, "a", "at", "a <a@e.com>", "s", "b", t0)
	ins(t, c, "a", "late", "a <a@e.com>", "s", "b", t0.Add(time.Second))
	hits, _ := search(t, c, SearchQuery{Since: t0})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"late", "at"}) {
		t.Errorf("since is not inclusive: %v", got)
	}
	hits, _ = search(t, c, SearchQuery{Until: t0})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"early"}) {
		t.Errorf("until at the Search level is exclusive: %v", got)
	}
	hits, _ = search(t, c, SearchQuery{Since: t0, Until: t0.Add(time.Second)})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"at"}) {
		t.Errorf("since+until = %v", got)
	}
}

func TestSearchEmptyAccountMeansAllAccounts(t *testing.T) {
	c := openCache(t)
	ins(t, c, "one", "m1", "a <a@e.com>", "s", "hello", t0, "INBOX")
	ins(t, c, "two", "m2", "a <a@e.com>", "s", "hello", t0.Add(time.Second), "INBOX")
	for _, text := range []string{"", "hello"} {
		hits, _ := search(t, c, SearchQuery{Text: text})
		if len(hits) != 2 {
			t.Errorf("all accounts, text %q: %d hits, want 2", text, len(hits))
		}
		hits, _ = search(t, c, SearchQuery{Text: text, Account: "two"})
		if len(hits) != 1 || hits[0].Account != "two" {
			t.Errorf("account two, text %q: %+v", text, hits)
		}
	}
	hits, _ := search(t, c, SearchQuery{Account: "ghost"})
	if len(hits) != 0 {
		t.Errorf("account with no rows = %d hits", len(hits))
	}
}

func TestSearchLimitDefaultsAndClamps(t *testing.T) {
	c := openCache(t)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		id := fmt.Sprintf("m%04d", i)
		if _, err := tx.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, internal_date) VALUES ('a',?,'', 'x <x@e.com>','s',?,?)`,
			id, t0.Unix()+int64(i), t0.Unix()+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		limit, want int
		trunc       bool
	}{
		{0, defaultLimit, true},
		{-5, defaultLimit, true},
		{3, 3, true},
		{500, 500, true},
		{501, 500, true},
		{100000, 500, true},
	} {
		hits, trunc := search(t, c, SearchQuery{Limit: tc.limit})
		if len(hits) != tc.want || trunc != tc.trunc {
			t.Errorf("limit %d: %d hits truncated=%v, want %d / %v", tc.limit, len(hits), trunc, tc.want, tc.trunc)
		}
	}
	if defaultLimit != 50 || maxLimit != 500 {
		t.Errorf("documented limits changed: default %d max %d", defaultLimit, maxLimit)
	}
}

func TestSearchTruncatedOnlyWhenMoreThanLimit(t *testing.T) {
	c := openCache(t)
	for i := 0; i < 4; i++ {
		ins(t, c, "a", fmt.Sprintf("m%d", i), "x <x@e.com>", "s", "hello", t0.Add(time.Duration(i)*time.Second), "INBOX")
	}
	for _, text := range []string{"", "hello"} {
		if hits, trunc := search(t, c, SearchQuery{Text: text, Limit: 4}); len(hits) != 4 || trunc {
			t.Errorf("exactly limit hits (%q): n=%d truncated=%v, want 4 false", text, len(hits), trunc)
		}
		if hits, trunc := search(t, c, SearchQuery{Text: text, Limit: 5}); len(hits) != 4 || trunc {
			t.Errorf("fewer than limit (%q): n=%d truncated=%v", text, len(hits), trunc)
		}
		if hits, trunc := search(t, c, SearchQuery{Text: text, Limit: 3}); len(hits) != 3 || !trunc {
			t.Errorf("more than limit (%q): n=%d truncated=%v, want 3 true", text, len(hits), trunc)
		}
	}
}

func TestSearchOrdersByDateDescendingWithInternalDateFallback(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "old", "x <x@e.com>", "s", "b", t0)
	ins(t, c, "a", "new", "x <x@e.com>", "s", "b", t0.Add(48*time.Hour))
	ins(t, c, "a", "mid", "x <x@e.com>", "s", "b", t0.Add(24*time.Hour))
	// No Date header: date_unix 0, so the received time (internal_date) orders it.
	if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, subject, date_unix, internal_date) VALUES ('a','nodate','','s',0,?)`,
		t0.Add(36*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	hits, _ := search(t, c, SearchQuery{})
	if got := ids(hits); !reflect.DeepEqual(got, []string{"new", "nodate", "mid", "old"}) {
		t.Fatalf("order = %v", got)
	}
	if !hits[0].Date.Equal(t0.Add(48 * time.Hour)) {
		t.Errorf("hit date = %v", hits[0].Date)
	}
}

func TestSearchSnippetMarksMatchWithBrackets(t *testing.T) {
	c := ftsCorpus(t)
	hits, _ := search(t, c, SearchQuery{Text: "widget"})
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "[widget]") {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestFolderCountsFromIndexIncludesEmptyFolderAsZero(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Empty", "Full")
	e.appendMsg("Full", mkMsg("a", "one", "alpha"), t0)
	e.appendMsg("Full", mkMsg("b", "two", "beta"), t0.Add(time.Second))
	e.appendMsg("INBOX", mkMsg("c", "three", "gamma"), t0.Add(2*time.Second))
	e.refresh()
	got, err := e.cache.FolderCounts(e.ctx(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	want := []FolderCount{{"Empty", 0}, {"Full", 2}, {"INBOX", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FolderCounts = %v, want %v", got, want)
	}
	none, err := e.cache.FolderCounts(e.ctx(), "ghost")
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("unknown account: %v %v, want empty non-nil", none, err)
	}
}

// ---- ReadMessage ----

func sidBySubject(t *testing.T, c *Cache, subject string) string {
	t.Helper()
	var id string
	if err := c.db.QueryRow(`SELECT stable_id FROM messages WHERE subject = ?`, subject).Scan(&id); err != nil {
		t.Fatalf("no message with subject %q: %v", subject, err)
	}
	return id
}

func rawMsg(headers []string, body string) []byte {
	h := append([]string{
		"From: Alice <alice@example.com>",
		"To: Bob <bob@example.com>",
		"Date: Mon, 02 Jan 2006 15:04:05 +0000",
	}, headers...)
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" + body + "\r\n")
}

func TestReadMessageUnknownAndForeignAndTraversalIDsAreNotFound(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("a", "first", "alpha"), t0)
	e.refresh()
	other := e.account("other", accounts.Gmail, testPass)
	e.acct = other
	e.refresh() // "other" now knows "first" too
	e.appendMsg("INBOX", mkMsg("b", "second", "beta"), t0.Add(time.Second))
	e.acct = e.account("acct", accounts.Gmail, testPass)
	e.refresh() // only "acct" knows "second"
	second := sidBySubject(t, e.cache, "second")

	// A sentinel next to the cache: a traversal id must not read it.
	if err := os.WriteFile(filepath.Join(e.dir, "x"), []byte("Subject: leaked\r\n\r\nleaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "cache", "x"), []byte("leaked"), 0o600); err != nil {
		t.Fatal(err)
	}

	if m, err := e.cache.ReadMessage(e.ctx(), "acct", second, 1000); err != nil || m.Subject != "second" {
		t.Fatalf("own message: %v %+v", err, m)
	}
	for _, tc := range []struct{ acct, id string }{
		{"other", second},         // belongs to another account
		{"ghost", second},         // account that does not exist
		{"acct", "nope"},          // no such id
		{"acct", "../x"},          // traversal
		{"acct", "../../x"},       //
		{"acct", "/etc/passwd"},   //
		{"acct", ""},              //
		{"acct", "x' OR '1'='1"},  // SQL injection
		{"acct", second + "\x00"}, //
	} {
		m, err := e.cache.ReadMessage(e.ctx(), tc.acct, tc.id, 1000)
		if !errors.Is(err, ErrNotFound) || m != nil {
			t.Errorf("ReadMessage(%q, %q) = %v, %v; want ErrNotFound", tc.acct, tc.id, m, err)
		}
	}
}

func TestReadMessageTamperedBlobDigestNeverBecomesAPath(t *testing.T) {
	c := openCache(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(c.dir), "x"), []byte("leaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sum := range []string{"../../x", "../x", "", "ABCDEF", strings.Repeat("g", 64)} {
		if _, err := c.db.Exec(`INSERT OR REPLACE INTO messages (account, stable_id, blob_sha256) VALUES ('a','t',?)`, sum); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ReadMessage(tctx(t), "a", "t", 100); !errors.Is(err, ErrBlobMissing) {
			t.Errorf("digest %q: err = %v, want ErrBlobMissing", sum, err)
		}
	}
}

func TestReadMessageDeletedBlobIsErrBlobMissing(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("a", "first", "alpha"), t0)
	e.refresh()
	var sum string
	if err := e.cache.db.QueryRow(`SELECT blob_sha256 FROM messages`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.cache.BlobPath(sum)); err != nil {
		t.Fatal(err)
	}
	m, err := e.cache.ReadMessage(e.ctx(), "acct", onlyStableID(t, e.cache), 1000)
	if !errors.Is(err, ErrBlobMissing) || m != nil {
		t.Fatalf("got %v, %v; want ErrBlobMissing", m, err)
	}
}

func TestReadMessageHTMLOnlyYieldsStrippedText(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", rawMsg([]string{"Subject: html", "Message-Id: <h@t>", "Content-Type: text/html; charset=utf-8"},
		`<html><head><style>p{color:red}</style></head><body><p>Hello <b>wor</b>ld</p><script>evil()</script><div>second&nbsp;line &amp; more</div></body></html>`), t0)
	e.refresh()
	m, err := e.cache.ReadMessage(e.ctx(), "acct", onlyStableID(t, e.cache), 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<", ">", "evil", "color:red"} {
		if strings.Contains(m.Body, bad) {
			t.Errorf("body keeps %q: %q", bad, m.Body)
		}
	}
	if !strings.Contains(m.Body, "Hello world") || !strings.Contains(m.Body, "& more") {
		t.Errorf("stripped body = %q", m.Body)
	}
}

func TestReadMessageAttachmentMetadataWithoutContent(t *testing.T) {
	const token = "ATTACHMENT-SECRET-TOKEN-7731"
	payload := []byte("%PDF-1.4 " + token + strings.Repeat(" pad", 20))
	enc := base64.StdEncoding.EncodeToString(payload)
	body := "--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + enc + "\r\n" +
		"--B\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("pngbytes")) + "\r\n--B--"
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", rawMsg([]string{"Subject: att", "Message-Id: <att@t>", "Content-Type: multipart/mixed; boundary=B"}, body), t0)
	e.refresh()
	m, err := e.cache.ReadMessage(e.ctx(), "acct", onlyStableID(t, e.cache), 10000)
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "see attached" {
		t.Errorf("body = %q", m.Body)
	}
	want := []Attachment{
		{Filename: "report.pdf", ContentType: "application/pdf", Size: int64(len(payload))},
		{ContentType: "image/png", Size: int64(len("pngbytes"))},
	}
	if !reflect.DeepEqual(m.Attachments, want) {
		t.Errorf("attachments = %+v, want %+v", m.Attachments, want)
	}
	if strings.Contains(fmt.Sprintf("%+v", m), token) || strings.Contains(m.Body, "PDF") {
		t.Error("attachment content leaked into the message")
	}
}

func TestReadMessageBodyTruncatesOnRuneBoundary(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", rawMsg([]string{"Subject: utf", "Message-Id: <u@t>", "Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: 8bit"},
		strings.Repeat("é", 1000)), t0)
	e.refresh()
	id := onlyStableID(t, e.cache)
	for _, max := range []int{1, 2, 3, 99, 101, 1999} {
		m, err := e.cache.ReadMessage(e.ctx(), "acct", id, max)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(m.Body) || len(m.Body) > max || !m.Truncated {
			t.Errorf("max %d: valid=%v len=%d truncated=%v", max, utf8.ValidString(m.Body), len(m.Body), m.Truncated)
		}
		if len(m.Body) < max-1 {
			t.Errorf("max %d: cut too much (%d bytes)", max, len(m.Body))
		}
	}
	m, err := e.cache.ReadMessage(e.ctx(), "acct", id, 2000)
	if err != nil || m.Truncated || len(m.Body) != 2000 {
		t.Errorf("exactly fitting body: len=%d truncated=%v err=%v", len(m.Body), m.Truncated, err)
	}
}

// ---- SenderStats ----

func TestSenderStatsGroupsAddressFormsAndPicksCommonName(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "1", "Name <A@X.com>", "s", "b", t0)
	ins(t, c, "a", "2", "<a@x.com>", "s", "b", t0.Add(24*time.Hour))
	ins(t, c, "a", "3", "a@x.com", "s", "b", t0.Add(48*time.Hour))
	ins(t, c, "a", "4", "Name <a@x.com>", "s", "b", t0.Add(72*time.Hour))
	ins(t, c, "a", "5", "Other <a@x.com>", "s", "b", t0.Add(96*time.Hour))
	got, err := c.SenderStats(tctx(t), "a", "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("senders = %+v, want one group", got)
	}
	s := got[0]
	if s.Address != "a@x.com" || s.Count != 5 || s.Name != "Name" {
		t.Errorf("sender = %+v", s)
	}
	if s.First != t0.Format(time.RFC3339) || s.Last != t0.Add(96*time.Hour).Format(time.RFC3339) {
		t.Errorf("first/last = %s / %s", s.First, s.Last)
	}
	if !reflect.DeepEqual(s.SubjectShape, []string{"s"}) {
		t.Errorf("shape = %v", s.SubjectShape)
	}
}

func TestSenderStatsOrdersByCountAndLimits(t *testing.T) {
	c := openCache(t)
	add := func(addr string, n int) {
		for i := 0; i < n; i++ {
			ins(t, c, "a", fmt.Sprintf("%s-%d", addr, i), addr, "Build 4812 failed", "b", t0.Add(time.Duration(i)*time.Hour))
		}
	}
	add("c@x.com", 2)
	add("b@x.com", 3)
	add("d@x.com", 1)
	add("a@x.com", 1) // ties with d on count: address order
	got, err := c.SenderStats(tctx(t), "a", "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range got {
		order = append(order, fmt.Sprintf("%s:%d", s.Address, s.Count))
	}
	if !reflect.DeepEqual(order, []string{"b@x.com:3", "c@x.com:2", "a@x.com:1", "d@x.com:1"}) {
		t.Errorf("order = %v", order)
	}
	got, _ = c.SenderStats(tctx(t), "a", "", time.Time{}, time.Time{}, 2)
	if len(got) != 2 || got[0].Address != "b@x.com" || got[1].Address != "c@x.com" || got[0].Count != 3 {
		t.Errorf("limit 2 = %+v", got)
	}
	if got[0].SubjectShape[0] != "Build # failed" {
		t.Errorf("shape = %v", got[0].SubjectShape)
	}
}

func TestSenderStatsFilters(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "old", "x@x.com", "s", "b", t0, "INBOX")
	ins(t, c, "a", "mid", "x@x.com", "s", "b", t0.Add(24*time.Hour), "Archive")
	ins(t, c, "a", "new", "y@x.com", "s", "b", t0.Add(48*time.Hour), "INBOX", "Archive")
	ins(t, c, "other", "o", "x@x.com", "s", "b", t0.Add(24*time.Hour), "INBOX")

	count := func(folder string, since, until time.Time) map[string]int {
		got, err := c.SenderStats(tctx(t), "a", folder, since, until, 0)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int{}
		for _, s := range got {
			m[s.Address] = s.Count
		}
		return m
	}
	if got := count("", time.Time{}, time.Time{}); !reflect.DeepEqual(got, map[string]int{"x@x.com": 2, "y@x.com": 1}) {
		t.Errorf("other account leaked or counts wrong: %v", got)
	}
	if got := count("INBOX", time.Time{}, time.Time{}); !reflect.DeepEqual(got, map[string]int{"x@x.com": 1, "y@x.com": 1}) {
		t.Errorf("folder INBOX: %v", got)
	}
	if got := count("", t0.Add(24*time.Hour), time.Time{}); !reflect.DeepEqual(got, map[string]int{"x@x.com": 1, "y@x.com": 1}) {
		t.Errorf("since inclusive: %v", got)
	}
	if got := count("", time.Time{}, t0.Add(24*time.Hour)); !reflect.DeepEqual(got, map[string]int{"x@x.com": 1}) {
		t.Errorf("until exclusive: %v", got)
	}
	if got, err := c.SenderStats(tctx(t), "ghost", "", time.Time{}, time.Time{}, 0); err != nil || len(got) != 0 || got == nil {
		t.Errorf("unknown account at cache level: %v %v", got, err)
	}
}

// ---- SubjectShape ----

func TestSubjectShape(t *testing.T) {
	long := strings.Repeat("é", 130)
	for _, tc := range []struct{ in, want string }{
		{"Re: hello", "hello"},
		{"RE: re: Re:  hello", "hello"},
		{"Fwd: Re: FW: hello", "hello"},
		{"fwd:fw:RE:hello", "hello"},
		{"  FWD : hello", "hello"},
		{"Re: ", ""},
		{"Build 4812 failed", "Build # failed"},
		{"Build 4813 failed", "Build # failed"},
		{"v2.10 of 3", "v#.# of #"},
		{"Order 3f2504e0-4f89-41d3-9a47-0305e82c3301 shipped", "Order … shipped"},
		{"Order 3F2504E0-4F89-41D3-9A47-0305E82C3301 shipped", "Order … shipped"},
		{"token deadbeef1 here", "token … here"},
		{"token 0123456789abcdef here", "token … here"},
		{"defaced", "defaced"},
		{"deadbeef", "deadbeef"}, // 8 hex letters, no digit: a word
		{"abcdefabcdef", "abcdefabcdef"},
		{"  lots   of\tspace ", "lots of space"},
		{"Re: Re: Build 7 failed", "Build # failed"},
		{"not a Re: prefix", "not a Re: prefix"},
		{strings.Repeat("a", 120), strings.Repeat("a", 120)},
		{strings.Repeat("a", 121), strings.Repeat("a", 120) + "…"},
		{long, strings.Repeat("é", 120) + "…"},
	} {
		if got := SubjectShape(tc.in); got != tc.want {
			t.Errorf("SubjectShape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := SubjectShape(long); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 121 {
		t.Errorf("cap is not rune-based: %d runes", utf8.RuneCountInString(got))
	}
}

func TestSearchQueryLimitsAreErrQueryLimit(t *testing.T) {
	c := ftsCorpus(t)
	words := func(n int) string { return strings.TrimSpace(strings.Repeat("w ", n)) }
	for name, q := range map[string]SearchQuery{
		"33 words":         {Text: words(33)},
		"513 bytes":        {Text: strings.Repeat("a", 513)},
		"513 bytes fts":    {Text: strings.Repeat("a", 513), FTSSyntax: true},
		"from 257 bytes":   {From: strings.Repeat("a", 257)},
		"33 words padding": {Text: words(33) + strings.Repeat(" ", 10)},
	} {
		if _, _, err := c.Search(tctx(t), q); !errors.Is(err, ErrQueryLimit) {
			t.Errorf("%s: err = %v, want ErrQueryLimit", name, err)
		}
	}
	for name, q := range map[string]SearchQuery{
		"32 words":  {Text: words(32)},
		"512 bytes": {Text: strings.Repeat("a", 512)},
		"from 256":  {From: strings.Repeat("a", 256)},
		// the word cap applies to plain words only; an FTS expression has its own byte cap
		"fts 33 terms": {Text: strings.Repeat("w OR ", 32) + "w", FTSSyntax: true},
	} {
		if _, _, err := c.Search(tctx(t), q); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
