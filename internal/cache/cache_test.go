package cache

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"

	"github.com/excavador/mail-mcp/internal/accounts"
)

func messageRow(t *testing.T, c *Cache, id string) string {
	t.Helper()
	var s string
	err := c.db.QueryRow(`SELECT account||'|'||stable_id||'|'||blob_sha256||'|'||from_addr||'|'||to_addr||'|'||cc_addr||'|'||subject||'|'||date_unix||'|'||list_id||'|'||gh_reason||'|'||size||'|'||internal_date FROM messages WHERE stable_id = ?`, id).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func onlyStableID(t *testing.T, c *Cache) string {
	t.Helper()
	var id string
	if err := c.db.QueryRow(`SELECT stable_id FROM messages`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSecondRefreshOfUnchangedMailboxIsQuietAndReadOnly(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Archive")
	e.appendMsg("INBOX", mkMsg("a", "one", "alpha"), t0)
	e.appendMsg("INBOX", mkMsg("b", "two", "beta"), t0.Add(time.Second))
	e.appendMsg("Archive", mkMsg("c", "three", "gamma"), t0.Add(2*time.Second))

	e.log.reset()
	first := e.refresh()
	if first.NewBodies != 3 || first.NewIDs != 3 || first.NewUIDs != 3 {
		t.Fatalf("first refresh: %+v", first)
	}
	// Sanity: the log really captures commands, so the negative checks below mean something.
	joined := strings.Join(e.log.lines(), "\n")
	if !strings.Contains(joined, "EXAMINE") || !strings.Contains(joined, "BODY.PEEK[]") {
		t.Fatalf("command log does not capture EXAMINE / BODY.PEEK[]:\n%s", joined)
	}

	e.log.reset()
	second := e.refresh()
	if second.NewBodies != 0 || second.NewIDs != 0 || second.NewUIDs != 0 || second.Removed != 0 {
		t.Fatalf("second refresh not quiet: %+v", second)
	}
	if second.Folders != 2 || second.FoldersTotal != 2 {
		t.Errorf("Folders = %d, FoldersTotal = %d, want 2, 2", second.Folders, second.FoldersTotal)
	}
	if second.FoldersSkipped != second.FoldersTotal || second.FoldersScanned != 0 {
		t.Errorf("FoldersSkipped = %d, FoldersScanned = %d, want %d, 0", second.FoldersSkipped, second.FoldersScanned, second.FoldersTotal)
	}
	// An unchanged mailbox costs LIST and one STATUS per folder: no folder is
	// opened, no UID listing, no body.
	forbidden := map[string]bool{"STORE": true, "COPY": true, "MOVE": true, "EXPUNGE": true, "SELECT": true, "EXAMINE": true, "FETCH": true, "SEARCH": true, "APPEND": true, "DELETE": true, "CREATE": true, "RENAME": true}
	counts := map[string]int{}
	for _, v := range e.log.verbs() {
		counts[v]++
		if forbidden[v] {
			t.Errorf("command %s sent during an unchanged refresh", v)
		}
	}
	if counts["LIST"] != 1 || counts["STATUS"] != second.FoldersTotal {
		t.Errorf("verbs = %v, want 1 LIST and %d STATUS", e.log.verbs(), second.FoldersTotal)
	}
	for _, ln := range e.log.lines() {
		if strings.Contains(strings.ToUpper(ln), " STATUS ") {
			for _, item := range []string{"MESSAGES", "UIDNEXT", "UIDVALIDITY"} {
				if !strings.Contains(strings.ToUpper(ln), item) {
					t.Errorf("STATUS without %s: %q", item, ln)
				}
			}
		}
	}
	if strings.Contains(strings.ToUpper(strings.Join(e.log.lines(), "\n")), "BODY") {
		t.Errorf("second refresh fetched a body:\n%s", strings.Join(e.log.lines(), "\n"))
	}
}

func TestFirstRefreshOnlyUsesPeekFetches(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("a", "one", "alpha"), t0)
	e.log.reset()
	e.refresh()
	for _, ln := range e.log.lines() {
		u := strings.ToUpper(ln)
		if strings.Contains(u, "BODY[") {
			t.Errorf("non-PEEK fetch: %q", ln)
		}
	}
	for _, v := range e.log.verbs() {
		switch v {
		case "STORE", "COPY", "MOVE", "EXPUNGE", "SELECT":
			t.Errorf("forbidden command %s", v)
		}
	}
}

func TestMoveChangesOnlyMembership(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "A", "B")
	raw := mkMsg("mv", "moving", "needle in body")
	e.appendMsg("A", raw, t0)
	e.refresh()

	id := onlyStableID(t, e.cache)
	var sum string
	if err := e.cache.db.QueryRow(`SELECT blob_sha256 FROM messages`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	path := e.cache.BlobPath(sum)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rowBefore := messageRow(t, e.cache, id)

	// Move: same bytes, same internal date, into B; gone from A.
	e.appendMsg("B", raw, t0)
	e.expungeAll("A")
	st := e.refresh()

	if st.NewBodies != 0 || st.NewIDs != 0 {
		t.Errorf("move fetched bodies / created ids: %+v", st)
	}
	if st.NewUIDs != 1 || st.Removed != 1 {
		t.Errorf("move should be 1 new uid + 1 removed, got %+v", st)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("blob inode changed")
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("blob mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
	if got := messageRow(t, e.cache, id); got != rowBefore {
		t.Errorf("messages row changed:\n%s\n%s", rowBefore, got)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1 {
		t.Errorf("messages rows = %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM message_fts`); n != 1 {
		t.Errorf("message_fts rows = %d, want exactly 1", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'A'`); n != 0 {
		t.Errorf("membership in A = %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'B' AND stable_id = ?`, id); n != 1 {
		t.Errorf("membership in B = %d", n)
	}
}

func TestSameMessageInTwoFoldersIsOneRowTwoMemberships(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Label")
	raw := mkMsg("dup", "dup", "same bytes")
	e.appendMsg("INBOX", raw, t0)
	e.appendMsg("Label", raw, t0)
	st := e.refresh()
	if st.NewUIDs != 2 || st.NewIDs != 1 || st.NewBodies != 1 {
		t.Fatalf("stats %+v, want 2 uids / 1 id / 1 body", st)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1 {
		t.Errorf("messages = %d, want 1", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership`); n != 2 {
		t.Errorf("membership = %d, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM message_fts`); n != 1 {
		t.Errorf("fts = %d, want 1", n)
	}
}

func TestUIDValidityChangeDropsAndRescansMembership(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "X")
	m1 := mkMsg("m1", "first", "one")
	m2 := mkMsg("m2", "second", "two")
	m3 := mkMsg("m3", "third", "three")
	e.appendMsg("X", m1, t0)
	e.appendMsg("X", m2, t0.Add(time.Second))
	e.refresh()
	var oldValidity uint32
	if err := e.cache.db.QueryRow(`SELECT uidvalidity FROM folders WHERE folder='X'`).Scan(&oldValidity); err != nil {
		t.Fatal(err)
	}
	var blobs []string
	rows, err := e.cache.db.Query(`SELECT blob_sha256 FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		blobs = append(blobs, s)
	}
	_ = rows.Close()

	e.deleteFolder("X")
	e.createFolder("X")
	// UIDs are reused by different messages: uid 1 is now m3, uid 2 is m1.
	e.appendMsg("X", m3, t0.Add(2*time.Second))
	e.appendMsg("X", m1, t0)
	st := e.refresh()

	if st.Removed != 2 {
		t.Errorf("Removed = %d, want 2 (old membership dropped)", st.Removed)
	}
	if st.NewUIDs != 2 {
		t.Errorf("NewUIDs = %d, want 2 (rescanned)", st.NewUIDs)
	}
	if st.NewIDs != 1 || st.NewBodies != 1 {
		t.Errorf("only m3 is new: %+v", st)
	}
	var newValidity uint32
	if err := e.cache.db.QueryRow(`SELECT uidvalidity FROM folders WHERE folder='X'`).Scan(&newValidity); err != nil {
		t.Fatal(err)
	}
	if newValidity == oldValidity {
		t.Error("test setup: UIDVALIDITY did not change")
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder='X' AND uidvalidity = ?`, newValidity); n != 2 {
		t.Errorf("membership rows under new validity = %d, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE uidvalidity = ?`, oldValidity); n != 0 {
		t.Errorf("stale membership rows = %d", n)
	}
	var subj string
	if err := e.cache.db.QueryRow(`SELECT m.subject FROM membership s JOIN messages m USING (stable_id) WHERE s.folder='X' AND s.uid=1`).Scan(&subj); err != nil {
		t.Fatal(err)
	}
	if subj != "third" {
		t.Errorf("uid 1 maps to %q, want the new message \"third\"", subj)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 3 {
		t.Errorf("messages = %d, want 3 (old ones survive)", n)
	}
	for _, b := range blobs {
		if _, err := os.Stat(e.cache.BlobPath(b)); err != nil {
			t.Errorf("blob lost: %v", err)
		}
	}
}

func TestMessageLeavingFolderKeepsBlobAndIndex(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("keep", "keeper", "stay"), t0)
	e.appendMsg("INBOX", mkMsg("go", "goner", "leave needle"), t0.Add(time.Second))
	e.refresh()
	var sum string
	if err := e.cache.db.QueryRow(`SELECT blob_sha256 FROM messages WHERE subject='goner'`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	e.expungeUID("INBOX", 2)
	st := e.refresh()
	if st.Removed != 1 || st.NewUIDs != 0 || st.NewBodies != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership`); n != 1 {
		t.Errorf("membership = %d, want 1", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 2 {
		t.Errorf("messages = %d, want 2", n)
	}
	if _, err := os.Stat(e.cache.BlobPath(sum)); err != nil {
		t.Errorf("blob removed: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM message_fts WHERE message_fts MATCH 'needle'`); n != 1 {
		t.Errorf("departed message no longer searchable")
	}
}

func TestPutBlobExistingContentIsNotRewritten(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw := []byte("Subject: x\r\n\r\nbody\r\n")
	sum, err := c.putBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := c.BlobPath(sum)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	sum2, err := c.putBlob(raw)
	if err != nil || sum2 != sum {
		t.Fatalf("second putBlob: %q %v", sum2, err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Error("existing blob was rewritten")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(raw) {
		t.Error("blob content differs")
	}
	if after.Mode().Perm() != 0o440 {
		t.Errorf("blob mode = %v, want 0440", after.Mode().Perm())
	}
	var leftovers []string
	_ = filepath.WalkDir(filepath.Join(c.dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".tmp-") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestExtractionPlainPreferredOverHTML(t *testing.T) {
	raw := mkMsg("x", "multi", "", "MIME-Version: 1.0", `Content-Type: multipart/alternative; boundary="B"`)
	raw = []byte(strings.Replace(string(raw), "\r\n\r\n\r\n", "\r\n\r\n", 1) +
		"--B\r\nContent-Type: text/plain\r\n\r\nplainword here\r\n" +
		"--B\r\nContent-Type: text/html\r\n\r\n<p>htmlonlyword</p>\r\n--B--\r\n")
	p := parseMessage(raw)
	if !strings.Contains(p.Body, "plainword") || strings.Contains(p.Body, "htmlonlyword") {
		t.Errorf("body = %q, want plain part only", p.Body)
	}
}

func TestExtractionHTMLOnlyIsStripped(t *testing.T) {
	html := `<html><head><title>TTL</title><style>.x{color:red}</style></head><body>` +
		`<script>var secretjs = 1;</script><div>Hello <b>bold</b> <b>wor</b>ld &amp; coamp; co</div><style>p{x:y}</style><p>second</p></body></html>`
	raw := mkMsg("h", "html", html, "Content-Type: text/html; charset=utf-8")
	p := parseMessage(raw)
	for _, bad := range []string{"<", ">", "secretjs", "color:red", "TTL", "p{x:y}"} {
		if strings.Contains(p.Body, bad) {
			t.Errorf("body %q still contains %q", p.Body, bad)
		}
	}
	for _, want := range []string{"Hello", "bold", "& co", "second"} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("body %q lacks %q", p.Body, want)
		}
	}
}

func TestExtractionSkipsAttachments(t *testing.T) {
	raw := mkMsg("att", "att", "", "MIME-Version: 1.0", `Content-Type: multipart/mixed; boundary="M"`)
	raw = []byte(strings.Replace(string(raw), "\r\n\r\n\r\n", "\r\n\r\n", 1) +
		"--M\r\nContent-Type: text/plain\r\n\r\nvisiblebody\r\n" +
		"--M\r\nContent-Type: text/plain; name=\"n.txt\"\r\nContent-Disposition: attachment; filename=\"n.txt\"\r\n\r\nattachedsecret\r\n" +
		"--M\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n\r\nUERGQ09OVEVOVA==\r\n--M--\r\n")
	p := parseMessage(raw)
	if !strings.Contains(p.Body, "visiblebody") || strings.Contains(p.Body, "attachedsecret") || strings.Contains(p.Body, "PDFCONTENT") {
		t.Errorf("body = %q", p.Body)
	}
}

func TestExtractionTruncatesOnValidUTF8Boundary(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} { // 2, 3, 4 bytes
		w := len(r)
		for prefix := 0; prefix < w; prefix++ {
			body := strings.Repeat("a", prefix) + strings.Repeat(r, maxIndexedText/w+10)
			raw := mkMsg("big", "big", body, "Content-Type: text/plain; charset=utf-8")
			p := parseMessage(raw)
			if !utf8.ValidString(p.Body) {
				t.Fatalf("%q prefix %d: truncated body is not valid UTF-8", r, prefix)
			}
			if strings.ContainsRune(p.Body, utf8.RuneError) {
				t.Fatalf("%q prefix %d: replacement char in body", r, prefix)
			}
			if len(p.Body) > maxIndexedText || len(p.Body) <= maxIndexedText-w {
				t.Fatalf("%q prefix %d: len = %d, want in (%d, %d]", r, prefix, len(p.Body), maxIndexedText-w, maxIndexedText)
			}
		}
	}
}

func TestMalformedMessageStillCachedAsBlob(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	// Valid header block (so a stable id exists), broken MIME: multipart with no boundary, bogus charset and encoding.
	raw := []byte("From: x@example.com\r\nMessage-Id: <bad@test>\r\nSubject: =?bogus-charset?Q?hi?=\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed\r\nContent-Transfer-Encoding: weird\r\n\r\n--no boundary here\r\nbody\r\n")
	e.appendMsg("INBOX", raw, t0)
	st := e.refresh()
	if st.NewBodies != 1 {
		t.Fatalf("stats %+v", st)
	}
	var sum string
	if err := e.cache.db.QueryRow(`SELECT blob_sha256 FROM messages`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(e.cache.BlobPath(sum))
	if err != nil || string(got) != string(raw) {
		t.Errorf("blob not stored verbatim: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership`); n != 1 {
		t.Errorf("membership = %d", n)
	}
}

func TestHeaderlessGarbageMessageStillCached(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	raw := []byte("\x00\x01\x02 not a header at all\r\nstill not\r\n")
	e.appendMsg("INBOX", raw, t0)
	c, err := imapxDial(e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	st, err := e.cache.Refresh(e.ctx(), e.acct, c)
	if err != nil {
		t.Fatalf("garbage message made Refresh fail: %v (stats %+v)", err, st)
	}
	if st.NewBodies != 1 {
		t.Errorf("stats %+v", st)
	}
}

func TestIndexedFieldsAndFTS(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	raw := mkMsg("f", "Quarterly zebra report", "the quick brown fox jumps",
		"Cc: Carol <carol@example.com>",
		"List-Id: <list.example.com>",
		"X-GitHub-Reason: review_requested")
	e.appendMsg("INBOX", raw, t0)
	e.appendMsg("INBOX", mkMsg("g", "unrelated", "nothing here"), t0)
	e.refresh()

	var from, to, cc, subj, listID, reason string
	var dateUnix, size, internal int64
	err := e.cache.db.QueryRow(`SELECT from_addr, to_addr, cc_addr, subject, date_unix, list_id, gh_reason, size, internal_date FROM messages WHERE subject LIKE 'Quarterly%'`).
		Scan(&from, &to, &cc, &subj, &dateUnix, &listID, &reason, &size, &internal)
	if err != nil {
		t.Fatal(err)
	}
	if listID != "<list.example.com>" {
		t.Errorf("list_id = %q", listID)
	}
	if reason != "review_requested" {
		t.Errorf("gh_reason = %q", reason)
	}
	if size != int64(len(raw)) {
		t.Errorf("size = %d, want %d", size, len(raw))
	}
	if internal != t0.Unix() {
		t.Errorf("internal_date = %d, want %d", internal, t0.Unix())
	}
	if want := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC).Unix(); dateUnix != want {
		t.Errorf("date_unix = %d, want %d", dateUnix, want)
	}
	if !strings.Contains(from, "alice@example.com") || !strings.Contains(to, "bob@example.com") || !strings.Contains(cc, "carol@example.com") {
		t.Errorf("addresses: %q %q %q", from, to, cc)
	}
	for q, want := range map[string]int{
		`zebra`:               1, // subject
		`brown`:               1, // body
		`carol`:               1, // cc column
		`"quick brown"`:       1,
		`body:fox`:            1,
		`absentword`:          0,
		`subject:fox`:         0,
		`nothing OR zebra`:    2,
		`alice`:               2,
		`from_addr:alice`:     2,
		`unrelated AND zebra`: 0,
		`brow*`:               1,
	} {
		if n := e.count(`SELECT COUNT(*) FROM message_fts WHERE message_fts MATCH ?`, q); n != want {
			t.Errorf("MATCH %q = %d rows, want %d", q, n, want)
		}
	}
}

func TestStableID(t *testing.T) {
	hdr := func(lines ...string) []byte { return []byte(strings.Join(lines, "\r\n") + "\r\n\r\n") }
	d1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	mid := hdr("Message-Id: <m@x>", "Subject: s")
	pm := hdr("X-Pm-Internal-Id: ABC123", "Message-Id: <m@x>")

	id := func(p accounts.Provider, h []byte, size int64, d time.Time) string {
		t.Helper()
		s, err := stableID(p, h, size, d)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	if got := id(accounts.Proton, pm, 10, d1); got != "pm:ABC123" {
		t.Errorf("proton with header = %q", got)
	}
	if got := id(accounts.Proton, pm, 999, d1.Add(time.Hour)); got != "pm:ABC123" {
		t.Errorf("proton id must not depend on size/date, got %q", got)
	}
	pmNoHdr := id(accounts.Proton, mid, 10, d1)
	if !strings.HasPrefix(pmNoHdr, "mid:") {
		t.Errorf("proton fallback = %q, want mid: scheme", pmNoHdr)
	}
	gm := id(accounts.Gmail, mid, 10, d1)
	if !strings.HasPrefix(gm, "mid:") || gm != pmNoHdr {
		t.Errorf("gmail = %q, proton fallback = %q; schemes must agree", gm, pmNoHdr)
	}
	if got := id(accounts.Gmail, pm, 10, d1); !strings.HasPrefix(got, "mid:") {
		t.Errorf("gmail must ignore X-Pm-Internal-Id, got %q", got)
	}
	if got := id(accounts.Gmail, mid, 10, d1); got != gm {
		t.Error("not deterministic")
	}
	if id(accounts.Gmail, mid, 11, d1) == gm {
		t.Error("different size must give a different id")
	}
	if id(accounts.Gmail, mid, 10, d1.Add(time.Second)) == gm {
		t.Error("different internal date must give a different id")
	}
	if id(accounts.Gmail, mid, 10, d1.Add(500*time.Millisecond)) != gm {
		t.Error("sub-second date difference should not matter")
	}
	if id(accounts.Gmail, hdr("Message-Id: <other@x>"), 10, d1) == gm {
		t.Error("different Message-ID must give a different id")
	}
	// No Message-ID: the header block is mixed in.
	n1 := hdr("Subject: a", "From: x@y", "Date: Mon, 02 Jan 2006 15:04:05 +0000")
	n2 := hdr("Subject: b", "From: x@y", "Date: Mon, 02 Jan 2006 15:04:05 +0000")
	a, b := id(accounts.Gmail, n1, 10, d1), id(accounts.Gmail, n2, 10, d1)
	if a == b {
		t.Error("no Message-ID, different headers, same size/date must differ")
	}
	if a != id(accounts.Gmail, n1, 10, d1) || !strings.HasPrefix(a, "mid:") {
		t.Errorf("no Message-ID id unstable or wrong scheme: %q", a)
	}
	// Duplicate or malformed X-Pm-Internal-Id falls back to the Message-ID scheme.
	for name, h := range map[string][]byte{
		"duplicate": hdr("X-Pm-Internal-Id: ABC", "X-Pm-Internal-Id: DEF", "Message-Id: <m@x>", "Subject: s"),
		"space":     hdr("X-Pm-Internal-Id: AB C", "Message-Id: <m@x>", "Subject: s"),
		"colon":     hdr("X-Pm-Internal-Id: pm:evil", "Message-Id: <m@x>", "Subject: s"),
		"slash":     hdr("X-Pm-Internal-Id: ../x", "Message-Id: <m@x>", "Subject: s"),
		"too long":  hdr("X-Pm-Internal-Id: "+strings.Repeat("A", 129), "Message-Id: <m@x>", "Subject: s"),
		"empty":     hdr("X-Pm-Internal-Id:", "Message-Id: <m@x>", "Subject: s"),
	} {
		if got := id(accounts.Proton, h, 10, d1); got != gm {
			t.Errorf("%s: got %q, want fallback %q", name, got, gm)
		}
	}
	if got := id(accounts.Proton, hdr("X-Pm-Internal-Id: "+strings.Repeat("a-_=Z9", 21)+"ab"), 1, d1); got != "pm:"+strings.Repeat("a-_=Z9", 21)+"ab" {
		t.Errorf("128-char valid id rejected: %q", got)
	}
	// Empty header block still yields an id and no error.
	if got := id(accounts.Gmail, hdr(), 0, time.Time{}); got == "" {
		t.Error("empty id")
	}
}

func TestRefreshErrorIsReturnedRecordedAndVisibleInStatus(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("a", "one", "x"), t0)
	e.refresh()
	ss, err := e.cache.Status(e.ctx())
	if err != nil || len(ss) != 1 || !ss[0].LastOK || ss[0].LastRefresh == nil {
		t.Fatalf("status after good refresh: %+v %v", ss, err)
	}
	if ss[0].Messages != 1 || ss[0].Memberships != 1 || ss[0].Folders != 1 {
		t.Errorf("counts: %+v", ss[0])
	}

	// Server hangs up: the client is closed before Refresh.
	c, err := imapxDial(e)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := e.cache.Refresh(e.ctx(), e.acct, c); err == nil {
		t.Fatal("Refresh on a dead connection returned nil")
	}
	if n := e.count(`SELECT COUNT(*) FROM refreshes WHERE account = ? AND ok = 0`, e.acct.Name); n != 1 {
		t.Errorf("refreshes row with ok=0: %d", n)
	}
	ss, err = e.cache.Status(e.ctx())
	if err != nil || len(ss) != 1 {
		t.Fatalf("status: %+v %v", ss, err)
	}
	if ss[0].LastOK {
		t.Error("Status still reports last refresh OK")
	}
	if ss[0].Messages != 1 {
		t.Error("a failed refresh must not discard what is cached")
	}
}

func TestRefreshOnceLoginFailureIsRecorded(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	bad := e.account("badpw", accounts.Gmail, "wrong-password")
	if _, err := e.cache.RefreshOnce(e.ctx(), bad); err == nil {
		t.Fatal("RefreshOnce with a wrong password returned nil")
	}
	n := e.count(`SELECT COUNT(*) FROM refreshes WHERE account = 'badpw' AND ok = 0`)
	if n != 1 {
		t.Errorf("refreshes rows with ok=0 for never-connected account: %d, want 1", n)
	}
	ss, err := e.cache.Status(e.ctx())
	if err != nil || len(ss) != 1 || ss[0].Account != "badpw" || ss[0].LastOK || ss[0].LastRefresh == nil {
		t.Errorf("status: %+v %v", ss, err)
	}
}

func TestRunWithZeroIntervalReturnsImmediately(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	done := make(chan struct{})
	go func() {
		e.cache.Run(context.Background(), slog.New(slog.DiscardHandler), e.acct, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run with interval 0 did not return")
	}
	if n := e.count(`SELECT COUNT(*) FROM refreshes`); n != 0 {
		t.Errorf("Run with interval 0 refreshed (%d rows)", n)
	}
}

type countHandler struct{ failed, ok *atomic.Int32 }

func (h countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countHandler) Handle(_ context.Context, r slog.Record) error {
	switch r.Message {
	case "cache refresh failed":
		h.failed.Add(1)
	case "cache refreshed":
		h.ok.Add(1)
	}
	return nil
}
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countHandler) WithGroup(string) slog.Handler      { return h }

func TestRunKeepsTickingAfterFailure(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	bad := e.account("badpw", accounts.Gmail, "wrong-password")
	h := countHandler{failed: new(atomic.Int32), ok: new(atomic.Int32)}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.cache.Run(ctx, slog.New(h), bad, 40*time.Millisecond)
		close(done)
	}()
	deadline := time.After(8 * time.Second)
	for h.failed.Load() < 3 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("Run stopped ticking after a failure: %d failures logged", h.failed.Load())
		case <-done:
			t.Fatalf("Run returned early after %d failures", h.failed.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if h.ok.Load() != 0 {
		t.Error("bad credentials should never log success")
	}
}

func TestRunRefreshesOnStartAndOnTick(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	e.appendMsg("INBOX", mkMsg("a", "one", "x"), t0)
	h := countHandler{failed: new(atomic.Int32), ok: new(atomic.Int32)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.cache.Run(ctx, slog.New(h), e.acct, 40*time.Millisecond)
		close(done)
	}()
	deadline := time.After(8 * time.Second)
	for h.ok.Load() < 3 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("only %d refreshes logged", h.ok.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1 {
		t.Errorf("messages = %d", n)
	}
}

var _ = imap.UID(0)

func TestExtractionInlineTagsKeepWordsBlockTagsSeparate(t *testing.T) {
	raw := mkMsg("i", "inl", "<p>wor<b>bo</b>ld</p><p>next</p><div>alpha</div>beta<br>gamma", "Content-Type: text/html; charset=utf-8")
	p := parseMessage(raw)
	for _, want := range []string{"worbold", "next", "alpha", "beta", "gamma"} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("body %q lacks %q", p.Body, want)
		}
	}
	for _, bad := range []string{"nextalpha", "alphabeta", "betagamma", "worbold next"} {
		if strings.Contains(p.Body, bad) {
			t.Errorf("body %q wrongly joins %q", p.Body, bad)
		}
	}
	if q := parseMessage(mkMsg("j", "j", "<b>wor</b>ld", "Content-Type: text/html")); !strings.Contains(q.Body, "world") {
		t.Errorf("body = %q, want \"world\"", q.Body)
	}
}

func TestBlobPathRefusesNonDigests(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	good := strings.Repeat("ab", 32)
	if c.BlobPath(good) == "" {
		t.Error("valid digest refused")
	}
	for _, bad := range []string{"../x", "", "a", strings.ToUpper(good), good + "0", good[:63], strings.Repeat("g", 64), "../" + good[:61], strings.Repeat("ab", 31) + "A" + "b"} {
		if got := c.BlobPath(bad); got != "" {
			t.Errorf("BlobPath(%q) = %q, want empty", bad, got)
		}
	}
}

func TestHeaderFieldsCappedOnUTF8Boundary(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} {
		w := len(r)
		for prefix := 0; prefix < w; prefix++ {
			long := strings.Repeat("a", prefix) + strings.Repeat(r, maxHeaderField/w+10)
			raw := mkMsg("h", "S", "body", "List-Id: "+long, "X-GitHub-Reason: "+long, "Cc: "+long,
				"Content-Type: text/plain; charset=utf-8")
			raw = []byte(strings.Replace(string(raw), "Subject: S", "Subject: "+long, 1))
			p := parseMessage(raw)
			for name, v := range map[string]string{"subject": p.Subject, "list_id": p.ListID, "gh_reason": p.GitHubReason, "cc": p.Cc} {
				if !utf8.ValidString(v) || strings.ContainsRune(v, utf8.RuneError) {
					t.Fatalf("%s %q prefix %d: invalid UTF-8", name, r, prefix)
				}
				if len(v) > maxHeaderField || len(v) <= maxHeaderField-w {
					t.Fatalf("%s %q prefix %d: len %d, want in (%d, %d]", name, r, prefix, len(v), maxHeaderField-w, maxHeaderField)
				}
			}
		}
	}
}
