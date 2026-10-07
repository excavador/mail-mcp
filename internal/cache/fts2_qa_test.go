package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// ---- fixtures ----

func b64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for len(s) > 76 {
		out.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	return out.String() + s
}

// msgHdr builds a CRLF multipart/mixed message from complete MIME parts.
func msgHdr(from, subject string, date time.Time, parts ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: x@example.com\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=QQ\r\n\r\n",
		from, subject, date.Format(time.RFC1123Z))
	for _, p := range parts {
		b.WriteString("--QQ\r\n" + p + "\r\n")
	}
	b.WriteString("--QQ--\r\n")
	return []byte(b.String())
}

func partText(body string) string {
	return "Content-Type: text/plain; charset=utf-8\r\n\r\n" + body
}

func partPDF(name string, pdf []byte) string {
	return "Content-Type: application/pdf; name=\"" + name + "\"\r\nContent-Disposition: attachment; filename=\"" + name + "\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64(pdf)
}

func partBin(name string, data []byte) string {
	return "Content-Type: application/octet-stream; name=\"" + name + "\"\r\nContent-Disposition: attachment; filename=\"" + name + "\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64(data)
}

// indexRaw stores raw as a blob and indexes it the way refresh does (one
// transaction: message, message_fts2, attachments).
func indexRaw(t *testing.T, c *Cache, account, id string, raw []byte, folders ...string) {
	t.Helper()
	sum, err := c.putBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := parseMessage(raw)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	info := headerInfo{stableID: id, size: int64(len(raw)), internal: p.Date}
	if err := insertMessageTx(context.Background(), tx, account, info, sum, p); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	addFolders(t, c, account, id, folders...)
}

func addFolders(t *testing.T, c *Cache, account, id string, folders ...string) {
	t.Helper()
	for _, f := range folders {
		if _, err := c.db.Exec(`INSERT OR IGNORE INTO folders (account, folder, uidvalidity) VALUES (?,?,1)`, account, f); err != nil {
			t.Fatal(err)
		}
		if _, err := c.db.Exec(`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES (?,?,?,?,1)`, account, id, f, uidSeq.Add(1)); err != nil {
			t.Fatal(err)
		}
	}
}

// legacyInsert writes a message the way a pre-fts2 version did: a messages row
// with its blob on disk and no message_fts2 row (the backfill indexes it).
func legacyInsert(t *testing.T, c *Cache, account, id string, raw []byte) {
	t.Helper()
	sum, err := c.putBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := parseMessage(raw)
	if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, to_addr, cc_addr, subject, date_unix, internal_date) VALUES (?,?,?,?,?,?,?,?,?)`,
		account, id, sum, p.From, p.To, p.Cc, p.Subject, p.Date.Unix(), p.Date.Unix()); err != nil {
		t.Fatal(err)
	}
}

// reopenAsPreUpgrade closes c and reopens its directory after removing every
// trace of the fts2 work, so Open sees what an upgraded pre-fts2 database is.
func reopenAsPreUpgrade(t *testing.T, c *Cache) *Cache {
	t.Helper()
	dir := c.dir
	for _, q := range []string{`DELETE FROM message_fts2`, `DELETE FROM attachment_fts`, `DELETE FROM attachments`, `DELETE FROM backfill`} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return reopen(t, c, dir)
}

func reopen(t *testing.T, c *Cache, dir string) *Cache {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func qaCount(t *testing.T, c *Cache, q string, args ...any) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// assertAligned checks the fts2 invariants: exactly one message_fts2 row per
// message with rowid = messages.rowid (and the same account/stable_id), and
// attachment_fts rows aligned to attachments rowids.
func assertAligned(t *testing.T, c *Cache, wantMessages int) {
	t.Helper()
	if n := qaCount(t, c, `SELECT COUNT(*) FROM messages`); n != wantMessages {
		t.Fatalf("messages = %d, want %d", n, wantMessages)
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM message_fts2`); n != wantMessages {
		t.Fatalf("message_fts2 rows = %d, want %d (duplicates or misses)", n, wantMessages)
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM messages m LEFT JOIN message_fts2 f ON f.rowid = m.rowid WHERE f.rowid IS NULL`); n != 0 {
		t.Fatalf("%d messages have no message_fts2 row at their rowid", n)
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM messages m JOIN message_fts2 f ON f.rowid = m.rowid WHERE f.account <> m.account OR f.stable_id <> m.stable_id`); n != 0 {
		t.Fatalf("%d message_fts2 rows are misaligned with messages.rowid", n)
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM (SELECT account, stable_id FROM message_fts2 GROUP BY account, stable_id HAVING COUNT(*) > 1)`); n != 0 {
		t.Fatalf("%d messages indexed more than once in message_fts2", n)
	}
	// One attachment_fts row per attachment that has a file name; no PDF text.
	extracted := qaCount(t, c, `SELECT COUNT(*) FROM attachments WHERE filename <> ''`)
	if n := qaCount(t, c, `SELECT COUNT(*) FROM attachment_fts`); n != extracted {
		t.Fatalf("attachment_fts rows = %d, want %d (one per named attachment)", n, extracted)
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM attachment_fts f JOIN attachments a ON a.rowid = f.rowid WHERE a.account = f.account AND a.stable_id = f.stable_id AND a.part = f.part AND a.filename <> ''`); n != extracted {
		t.Fatalf("only %d of %d attachment_fts rows align with attachments.rowid", n, extracted)
	}
}

func hitIDs(hits []SearchHit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Account+"/"+h.StableID)
	}
	sort.Strings(out)
	return out
}

func eqs(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

var d0 = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

// ---- 1, 2: readiness and the fallback ----

func TestQAFreshDBIsReadyImmediately(t *testing.T) {
	c := openCache(t)
	if !isReady(c) {
		t.Fatal("fresh DB must be ready")
	}
	st, err := c.BackfillStatus(context.Background())
	if err != nil || !st.Complete || st.Done != 0 || st.Total != 0 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestQAPreexistingDBNotReadyUntilBackfillFinishes(t *testing.T) {
	c := openCache(t)
	for i := range 3 {
		legacyInsert(t, c, "a", fmt.Sprintf("m%d", i), msgHdr("Bob <bob@example.com>", "Hello", d0, partText("body "+fmt.Sprint(i))))
	}
	c = reopenAsPreUpgrade(t, c)
	if isReady(c) {
		t.Fatal("pre-existing DB must not be ready")
	}
	st, _ := c.BackfillStatus(context.Background())
	if st.Complete || st.Total != 3 || st.Done != 0 {
		t.Fatalf("status before: %+v", st)
	}
	// A reopen without finishing does not make it ready.
	c = reopen(t, c, c.dir)
	if isReady(c) {
		t.Fatal("reopen alone must not mark ready")
	}
	ctx := tctx(t)
	c.RunBackfill(ctx, quiet())
	if !isReady(c) {
		t.Fatal("after backfill: not ready")
	}
	assertAligned(t, c, 3)
	// Completion is durable: a reopen is ready at once.
	c = reopen(t, c, c.dir)
	if !isReady(c) {
		t.Fatal("completed backfill must survive reopen as ready")
	}
}

func TestQASearchReadsOnlyFTS2WhileBackfillRuns(t *testing.T) {
	c := openCache(t)
	legacyInsert(t, c, "a", "L1", msgHdr("Bob <bob@example.com>", "Quarterly", d0,
		partText("zanzibar spice order"), partPDF("offer.pdf", miniPDF("Thermostat Quotation 4711"))))
	legacyInsert(t, c, "a", "L2", msgHdr("Carol <carol@example.com>", "Lunch", d0.Add(time.Hour), partText("see you at the cafe")))
	c = reopenAsPreUpgrade(t, c)
	ctx := tctx(t)

	if isReady(c) {
		t.Fatal("backfill must be pending")
	}
	// No message_fts fallback: until the backfill reaches a message, search
	// does not see it (and does not fail).
	if hits, _, err := c.Search(ctx, SearchQuery{Text: "zanzibar"}); err != nil || len(hits) != 0 {
		t.Fatalf("pending backfill: %+v %v", hits, err)
	}

	c.RunBackfill(ctx, quiet())

	if !isReady(c) {
		t.Fatal("want ready after backfill")
	}
	if hits, _, err := c.Search(ctx, SearchQuery{Text: "zanzibar"}); err != nil || len(hits) != 1 || !strings.Contains(hits[0].Snippet, "[zanzibar]") {
		t.Fatalf("fts2 body search: %+v %v", hits, err)
	}
	hits, _, err := c.Search(ctx, SearchQuery{Text: "pdf"})
	if err != nil || len(hits) != 1 || hits[0].StableID != "L1" || hits[0].Snippet != "attachment: offer.[pdf]" {
		t.Fatalf("fts2 attachment search: %+v %v", hits, err)
	}
	if hits, _, err := c.Search(ctx, SearchQuery{Text: "body:zanzibar", FTSSyntax: true}); err != nil || len(hits) != 1 {
		t.Fatalf("fts2 FTS syntax body: %+v %v", hits, err)
	}
	if hits, _, _ := c.Search(ctx, SearchQuery{Text: "cafe", From: "bob"}); len(hits) != 0 {
		t.Fatalf("from filter excluded: %+v", hits)
	}
}

// ---- message_fts is retired ----

func tableExists(t *testing.T, c *Cache, name string) bool {
	t.Helper()
	return qaCount(t, c, `SELECT COUNT(*) FROM sqlite_master WHERE name = '`+name+`'`) > 0
}

func TestOpenDropsLegacyMessageFTSIdempotently(t *testing.T) {
	c := openCache(t)
	if tableExists(t, c, "message_fts") {
		t.Fatal("fresh DB must not create message_fts")
	}
	// An old file: message_fts present and populated, fts2 complete.
	if _, err := c.db.Exec(`CREATE VIRTUAL TABLE message_fts USING fts5 (subject, from_addr, to_addr, cc_addr, body, account UNINDEXED, stable_id UNINDEXED)`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO message_fts (subject, from_addr, to_addr, cc_addr, body, account, stable_id) VALUES ('s','f','t','c','b','a','x')`); err != nil {
		t.Fatal(err)
	}
	indexRaw(t, c, "a", "k1", msgHdr("Bob <bob@example.com>", "Keep", d0, partText("kept words")))
	dir := c.dir
	for i := range 2 { // second pass: nothing left to drop, no error
		c = reopen(t, c, dir)
		if tableExists(t, c, "message_fts") || qaCount(t, c, `SELECT COUNT(*) FROM sqlite_master WHERE substr(name, 1, 12) = 'message_fts_'`) != 0 {
			t.Fatalf("open %d: message_fts or its shadow tables remain", i)
		}
	}
	if !isReady(c) {
		t.Fatal("dropping message_fts must not touch the fts2 state")
	}
	if hits, _, err := c.Search(context.Background(), SearchQuery{Text: "kept"}); err != nil || len(hits) != 1 {
		t.Fatalf("search after drop: %+v %v", hits, err)
	}
}

func TestInsertPathDoesNotWriteMessageFTS(t *testing.T) {
	c := openCache(t)
	// A rolled-back-to-old-version file would have the table; prove the insert
	// path leaves even an existing one untouched.
	if _, err := c.db.Exec(`CREATE VIRTUAL TABLE message_fts USING fts5 (subject, from_addr, to_addr, cc_addr, body, account UNINDEXED, stable_id UNINDEXED)`); err != nil {
		t.Fatal(err)
	}
	indexRaw(t, c, "a", "w1", msgHdr("Bob <bob@example.com>", "Hello", d0, partText("fresh words")))
	if n := qaCount(t, c, `SELECT COUNT(*) FROM message_fts`); n != 0 {
		t.Fatalf("insert wrote %d message_fts rows", n)
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM message_fts2`); n != 1 {
		t.Fatalf("message_fts2 rows = %d, want 1", n)
	}
}

// ---- 3: backfill invariants ----

func seedLegacy(t *testing.T, c *Cache, n int) {
	t.Helper()
	// 13 distinct blobs shared by n messages; every third carries a PDF.
	raws := make([][]byte, 13)
	for i := range raws {
		if i%3 == 0 {
			raws[i] = msgHdr("Bob <bob@example.com>", fmt.Sprintf("S%d", i), d0, partText(fmt.Sprintf("body%d", i)), partPDF("f.pdf", miniPDF(fmt.Sprintf("pdftext%d", i))), partBin("a.bin", []byte("xx")))
		} else {
			raws[i] = msgHdr("Bob <bob@example.com>", fmt.Sprintf("S%d", i), d0, partText(fmt.Sprintf("body%d", i)))
		}
	}
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	sums := make([]string, len(raws))
	for i, r := range raws {
		if sums[i], err = c.putBlob(r); err != nil {
			t.Fatal(err)
		}
	}
	for i := range n {
		if _, err := tx.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, internal_date) VALUES ('a', ?, ?, 'Bob', 'S', ?, ?)`,
			fmt.Sprintf("m%05d", i), sums[i%len(sums)], d0.Unix()+int64(i), d0.Unix()+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestQABackfillKillAndRestartMidRunHasNoDuplicatesOrMisses(t *testing.T) {
	const n = 1300 // three batches of at most 500
	c := openCache(t)
	seedLegacy(t, c, n)
	c = reopenAsPreUpgrade(t, c)
	dir := c.dir

	deadline := time.Now().Add(60 * time.Second)
	// Kill 1: cancel as soon as the first batch is durable (mid-run).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.RunBackfill(ctx, quiet()) }()
	for {
		if st, err := c.BackfillStatus(context.Background()); err == nil && st.Done > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backfill made no progress")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RunBackfill did not return after cancel")
	}
	st, _ := c.BackfillStatus(context.Background())
	if st.Complete || st.Done == 0 || st.Done >= n {
		t.Fatalf("expected a partial run, got %+v", st)
	}
	if isReady(c) {
		t.Fatal("must not be ready after a killed run")
	}
	if got := qaCount(t, c, `SELECT COUNT(*) FROM message_fts2`); got != st.Done {
		t.Fatalf("durable fts2 rows %d != reported done %d (a cancelled batch leaked rows)", got, st.Done)
	}

	// Restart (new process): resumes, and is killed again at random points.
	c = reopen(t, c, dir)
	if isReady(c) {
		t.Fatal("reopen mid-run must not be ready")
	}
	for i := 1; i <= 6 && !isReady(c); i++ {
		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Duration(i)*7*time.Millisecond)
		c.RunBackfill(ctx2, quiet())
		cancel2()
		c = reopen(t, c, dir)
	}
	// Final run to completion.
	ctx3, cancel3 := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel3()
	c.RunBackfill(ctx3, quiet())
	if !isReady(c) {
		t.Fatal("backfill did not complete")
	}
	assertAligned(t, c, n)
	st, _ = c.BackfillStatus(context.Background())
	if !st.Complete || st.Done != n || st.Total != n {
		t.Fatalf("final status %+v", st)
	}
	// 1/3 of 13 blobs hold a PDF + a bin: 5 blobs (i%3==0) among 0..12.
	if got := qaCount(t, c, `SELECT COUNT(*) FROM attachments`); got == 0 {
		t.Fatal("no attachments recorded")
	}
}

func isReady(c *Cache) bool { return c.fts2Ready.Load() }

func TestQABackfillIgnoresMessagesInsertedAfterSnapshot(t *testing.T) {
	// Deterministic interleaving: one batch, then "refresh" writes new messages
	// (with their own fts2 rows), then the backfill resumes.
	const n = 1200
	c := openCache(t)
	seedLegacy(t, c, n)
	c = reopenAsPreUpgrade(t, c)
	if complete, _, err := c.backfillBatch(context.Background()); err != nil || complete {
		t.Fatalf("first batch: %v %v", complete, err)
	}
	for i := range 5 {
		indexRaw(t, c, "a", fmt.Sprintf("new%d", i), msgHdr("Eve <eve@example.com>", "new", d0, partText("fresh"), partPDF("n.pdf", miniPDF("newpdf"))))
	}
	ctx := tctx(t)
	c.RunBackfill(ctx, quiet())
	if !isReady(c) {
		t.Fatal("not ready")
	}
	assertAligned(t, c, n+5)
	st, _ := c.BackfillStatus(ctx)
	if st.Done != st.Total || st.Total != n {
		t.Fatalf("status %+v (total is the snapshot, new mail is not counted)", st)
	}
}

func TestQABackfillWithConcurrentRefresh(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	pdfMsg := func(i int) []byte {
		return append([]byte(fmt.Sprintf("Message-Id: <q%d@test>\r\n", i)), mimeWithPDF(fmt.Sprintf("body q%d", i), miniPDF(fmt.Sprintf("pdf q%d", i)))...)
	}
	for i := range 120 {
		e.appendMsg("INBOX", pdfMsg(i), t0.Add(time.Duration(i)*time.Second))
	}
	e.refresh()
	c := e.cache
	// Turn the first 120 into a "pre-upgrade" state.
	for _, q := range []string{`DELETE FROM message_fts2`, `DELETE FROM attachment_fts`, `DELETE FROM attachments`,
		`UPDATE backfill SET done = 0, last_rowid = 0, processed = 0, total = (SELECT COUNT(*) FROM messages), max_rowid = (SELECT MAX(rowid) FROM messages)`} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	c.fts2Ready.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.RunBackfill(ctx, quiet()) }()
	for wave := range 4 {
		for j := range 10 {
			e.appendMsg("INBOX", pdfMsg(1000+wave*10+j), t0.Add(time.Hour+time.Duration(wave*10+j)*time.Second))
		}
		e.refresh()
	}
	select {
	case <-done:
	case <-time.After(55 * time.Second):
		t.Fatal("backfill did not finish")
	}
	if !isReady(c) {
		t.Fatal("not ready")
	}
	assertAligned(t, c, 160)
}

func TestQABackfillMissingBlobGivesHeadersOnlyRowAndIsCounted(t *testing.T) {
	c := openCache(t)
	good := msgHdr("Bob <bob@example.com>", "Fine", d0, partText("ok body"))
	sum, _ := c.putBlob(good)
	rows := []struct{ id, blob, subj string }{
		{"m0", sum, "Fine"},
		{"m1", strings.Repeat("0", 64), "Okapi"}, // well-formed digest, no file
		{"m2", "", "Narwhal"},                    // no digest at all
	}
	for i, r := range rows {
		if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, internal_date) VALUES ('a',?,?,'Bob',?,?,?)`,
			r.id, r.blob, r.subj, d0.Unix()+int64(i), d0.Unix()+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	c = reopenAsPreUpgrade(t, c)
	var logs bytes.Buffer
	c.RunBackfill(tctx(t), slog.New(slog.NewTextHandler(&logs, nil)))
	if !isReady(c) {
		t.Fatalf("a missing blob must not stall the backfill:\n%s", logs.String())
	}
	assertAligned(t, c, 3)
	if !strings.Contains(logs.String(), "missing_blobs=2") {
		t.Fatalf("missing_blobs=2 not logged:\n%s", logs.String())
	}
	for _, w := range []string{"Okapi", "Narwhal"} {
		hits, _, err := c.Search(tctx(t), SearchQuery{Text: w})
		if err != nil || len(hits) != 1 {
			t.Fatalf("headers-only row %s must be searchable by subject: %+v %v", w, hits, err)
		}
	}
	if n := qaCount(t, c, `SELECT COUNT(*) FROM attachments WHERE stable_id IN ('m1','m2')`); n != 0 {
		t.Fatalf("headers-only rows have %d attachments", n)
	}
}

// ---- 4: refresh transaction ----

func TestQAFailedTransactionLeavesNoFts2OrAttachmentRows(t *testing.T) {
	c := openCache(t)
	raw := msgHdr("Bob <bob@example.com>", "Tx", d0, partText("tx body"), partPDF("t.pdf", miniPDF("txpdf")))
	sum, _ := c.putBlob(raw)
	p := parseMessage(raw)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertMessageTx(context.Background(), tx, "a", headerInfo{stableID: "x", size: 1, internal: d0}, sum, p); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"messages", "message_fts2", "attachments", "attachment_fts"} {
		if n := qaCount(t, c, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s has %d rows after rollback", tbl, n)
		}
	}
}

func TestQARefreshWritesMessageFts2AndAttachmentsInOneTransaction(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	// Break the last write of the transaction (attachment text): the message
	// and everything before it in the same transaction must roll back.
	if _, err := e.cache.db.Exec(`DROP TABLE attachment_fts`); err != nil {
		t.Fatal(err)
	}
	e.appendMsg("INBOX", append([]byte("Message-Id: <atomic@test>\r\n"), mimeWithPDF("atomic body", miniPDF("atomic pdf"))...), t0)
	c, err := imapxDial(e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, rerr := e.cache.Refresh(e.ctx(), e.acct, c)
	t.Logf("Refresh err (expected): %v", rerr)
	for _, tbl := range []string{"messages", "message_fts2", "attachments"} {
		if n := e.count(`SELECT COUNT(*) FROM ` + tbl); n != 0 {
			t.Errorf("%s has %d rows after a failed transaction (partial write)", tbl, n)
		}
	}
}

// There is no seam to make a PDF parse slow (pdfText is a plain function), so
// the "parse before BeginTx" property is pinned by source order: the write lock
// must not be taken until the slow work is done.
func TestQAParsingPrecedesBeginTx(t *testing.T) {
	for _, tc := range []struct{ file, fn, lock string }{
		{"refresh.go", "func (c *Cache) fetchBodies(", "if err := flush()"}, // flush's closure owns the BeginTx
		{"backfill.go", "func (c *Cache) backfillBatch(", "BeginTx("},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		i := strings.Index(s, tc.fn)
		if i < 0 {
			t.Fatalf("%s not found in %s", tc.fn, tc.file)
		}
		body := s[i:]
		if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
			body = body[:j+1]
		}
		p, b := strings.Index(body, "parseMessage("), strings.Index(body, tc.lock)
		if p < 0 || b < 0 || p > b {
			t.Errorf("%s: parseMessage at %d must come before BeginTx at %d", tc.fn, p, b)
		}
	}
}

// ---- 5: CleanBody guards ----

func TestQACleanBodyFalsePositiveGuards(t *testing.T) {
	long := strings.Repeat("A paragraph of real content that must remain searchable. ", 6)
	t.Run("wrote: mid-sentence is kept", func(t *testing.T) {
		in := "As Alice wrote: the plan is fine, and I agree with it.\nSecond line."
		if got := CleanBody(in); got != in {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("inline reply keeps new text between quotes", func(t *testing.T) {
		in := "> Can we ship on Friday?\nYes, Friday works.\n> And the docs?\nDocs land Thursday."
		if got, want := CleanBody(in), "Yes, Friday works.\nDocs land Thursday."; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("inline reply under a header stays findable through body_full", func(t *testing.T) {
		// CleanBody cuts everything after a reply header (documented): the
		// inline answers are then only in body_full, which is indexed.
		c := openCache(t)
		raw := msgHdr("Bob <bob@example.com>", "Re: ship", d0, partText("Thanks.\n\nOn Mon, Jan 5, 2026 at 10:00 AM Al <al@example.com> wrote:\n> Can we ship?\nanswerinline yes\n> docs?\nanswertwo thursday"))
		indexRaw(t, c, "a", "i1", raw)
		hits, _, err := c.Search(tctx(t), SearchQuery{Text: "answerinline"})
		if err != nil || len(hits) != 1 {
			t.Fatalf("inline answer lost from search: %+v %v", hits, err)
		}
	})
	t.Run("bottom-posted answer under header is kept (fallback to original)", func(t *testing.T) {
		in := "On Mon, Jan 5, 2026 at 10:00 AM Al <al@example.com> wrote:\n> Can we ship?\n\nYes ship it."
		if got := CleanBody(in); !strings.Contains(got, "Yes ship it.") {
			t.Fatalf("bottom-posted answer lost: %q", got)
		}
	})
	t.Run("sig delimiter inside a code block is cut (documented behaviour)", func(t *testing.T) {
		in := "Look:\n```\nline one\n-- \nline two\n```\nafter"
		got := CleanBody(in)
		// Actual behaviour: "-- " ends the body wherever it appears. The tail
		// is not lost from search, because body_full keeps the whole text.
		if got != "Look:\n```\nline one" {
			t.Fatalf("behaviour changed: %q", got)
		}
	})
	t.Run("diff headers are not mistaken for a signature", func(t *testing.T) {
		in := "--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new\nreview done"
		if got := CleanBody(in); got != in {
			t.Fatalf("diff mangled: %q", got)
		}
	})
	t.Run("forwarded-only message keeps its content", func(t *testing.T) {
		in := "---------- Forwarded message ---------\nFrom: Carol <carol@example.com>\nDate: Mon, Jan 5, 2026\nSubject: Invoice\nTo: me@example.com\n\nInvoice 42 is due on Friday.\nPay to NL00BANK."
		got := CleanBody(in)
		if !strings.Contains(got, "Invoice 42 is due on Friday.") || !strings.Contains(got, "NL00BANK") {
			t.Fatalf("forward content lost: %q", got)
		}
	})
	t.Run("no quotes is unchanged", func(t *testing.T) {
		if got := CleanBody(long); got != strings.TrimSpace(long) {
			t.Fatalf("changed: %q", got)
		}
	})
	t.Run("CRLF input", func(t *testing.T) {
		got := CleanBody("Sounds good.\r\n\r\n> old\r\n> older\r\nThanks\r\n-- \r\nAl")
		if got != "Sounds good.\n\nThanks" {
			t.Fatalf("got %q", got)
		}
		if got := CleanBody("Hi\r\nthere"); strings.Contains(got, "\r") {
			t.Fatalf("CR leaked into plain text: %q", got)
		}
	})
	t.Run("one-liner over a 5000-line quote returns the one-liner", func(t *testing.T) {
		in := "ok thanks\n" + strings.Repeat("> quoted line of an earlier message\n", 5000)
		if got := CleanBody(in); got != "ok thanks" {
			t.Fatalf("body_new for a one-liner over a huge quote is %d bytes, want the one-liner; got prefix %q", len(got), got[:min(len(got), 40)])
		}
	})
}

// ---- 6: attachments ----

func TestQANestedRelatedInAlternativeWithCid(t *testing.T) {
	raw := []byte("From: a@x\r\nSubject: s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=AA\r\n\r\n" +
		"--AA\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--AA\r\nContent-Type: multipart/related; boundary=RR\r\n\r\n" +
		"--RR\r\nContent-Type: text/html\r\n\r\n<p>hi <img src=\"cid:img1@x\"></p>\r\n" +
		"--RR\r\nContent-Type: image/png\r\nContent-Id: <img1@x>\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64([]byte("\x89PNGdata")) + "\r\n" +
		"--RR--\r\n--AA--\r\n")
	atts := extractAttachments(raw)
	if len(atts) != 1 {
		t.Fatalf("want exactly the image (html and plain are bodies), got %+v", atts)
	}
	a := atts[0]
	if a.Part != "2.2" || a.ContentID != "img1@x" || !a.Inline || a.Mime != "image/png" || a.Size != 8 || a.SHA256 == "" {
		t.Fatalf("meta %+v", a)
	}
}

func TestQAAttachmentFilenameEncodings(t *testing.T) {
	want := "Café menü.zip"
	mk := func(disp, ct string) []byte {
		return []byte("From: a@x\r\nSubject: s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: " + ct + "\r\n" + disp + "\r\n\r\nzz\r\n--B--\r\n")
	}
	enc := base64.StdEncoding.EncodeToString([]byte(want))
	for name, raw := range map[string][]byte{
		"RFC2047 B":         mk(`Content-Disposition: attachment; filename="=?UTF-8?B?`+enc+`?="`, "application/zip"),
		"RFC2047 Q":         mk(`Content-Disposition: attachment; filename="=?UTF-8?Q?Caf=C3=A9_men=C3=BC.zip?="`, "application/zip"),
		"RFC2047 in name=":  mk(``, `application/zip; name="=?UTF-8?B?`+enc+`?="`),
		"RFC2231":           mk(`Content-Disposition: attachment; filename*=UTF-8''Caf%C3%A9%20men%C3%BC.zip`, "application/zip"),
		"RFC2231 continued": mk(`Content-Disposition: attachment; filename*0*=UTF-8''Caf%C3%A9; filename*1*=%20men%C3%BC.zip`, "application/zip"),
		"RFC2231 latin1":    mk(`Content-Disposition: attachment; filename*=ISO-8859-1''Caf%E9%20men%FC.zip`, "application/zip"),
	} {
		atts := extractAttachments(raw)
		if len(atts) != 1 || atts[0].Filename != want {
			t.Errorf("%s: got %+v", name, atts)
		}
	}
}

func TestQAMoreThan200PartsIsCapped(t *testing.T) {
	var parts []string
	for i := range 250 {
		parts = append(parts, partBin(fmt.Sprintf("f%d.bin", i), []byte{byte(i)}))
	}
	atts := extractAttachments(msgHdr("a@x", "many", d0, parts...))
	if len(atts) != maxAttachments {
		t.Fatalf("got %d attachments, want cap %d", len(atts), maxAttachments)
	}
	if atts[0].Part != "1" || atts[len(atts)-1].Part != "200" {
		t.Fatalf("parts %q..%q", atts[0].Part, atts[len(atts)-1].Part)
	}
	// And through the index: bounded rows, no crash.
	c := openCache(t)
	indexRaw(t, c, "a", "many", msgHdr("a@x", "many", d0, parts...))
	if n := qaCount(t, c, `SELECT COUNT(*) FROM attachments`); n != maxAttachments {
		t.Fatalf("rows %d", n)
	}
}

// PDF text is not extracted (parsing untrusted PDFs in-process was unsafe), but
// every PDF is recorded as metadata whatever its content, with text_extracted 0.
func TestQAPDFCases(t *testing.T) {
	valid := miniPDF("Hello Widget")
	const tenMiB = 10 << 20
	cases := []struct {
		name    string
		pdf     []byte
		minSize int64
	}{
		{"valid text", valid, 1},
		{"image only", miniPDF(""), 1},
		{"over 10 MiB", append([]byte("%PDF-1.4\n"), make([]byte, tenMiB+1)...), tenMiB + 1},
		{"truncated", valid[:len(valid)/2], 1},
		{"garbage", append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("\xff\x00endobj stream "), 500)...), 1},
		{"page tree loop", []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [2 0 R] /Count 1 >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			atts := extractAttachments(msgHdr("a@x", "pdf", d0, partText("b"), partPDF("x.pdf", tc.pdf)))
			if time.Since(start) > 4*time.Second {
				t.Errorf("took %v", time.Since(start))
			}
			if len(atts) != 1 {
				t.Fatalf("attachment must be recorded: %+v", atts)
			}
			a := atts[0]
			if a.Filename != "x.pdf" || a.Mime != "application/pdf" || a.Size < tc.minSize || a.SHA256 == "" {
				t.Errorf("meta %+v", a)
			}
		})
	}
	t.Run("recorded in the index with text_extracted 0 and only the name searchable", func(t *testing.T) {
		c := openCache(t)
		indexRaw(t, c, "a", "b1", msgHdr("a@x", "pdf", d0, partText("b"), partPDF("x.pdf", valid)))
		if n := qaCount(t, c, `SELECT COUNT(*) FROM attachments WHERE filename = 'x.pdf' AND text_extracted = 0`); n != 1 {
			t.Fatalf("attachment rows %d", n)
		}
		if n := qaCount(t, c, `SELECT COUNT(*) FROM attachment_fts WHERE text <> ''`); n != 0 {
			t.Fatalf("attachment_fts rows with text: %d", n)
		}
		if n := qaCount(t, c, `SELECT COUNT(*) FROM attachment_fts WHERE filename = 'x.pdf'`); n != 1 {
			t.Fatalf("attachment_fts name rows %d", n)
		}
	})
}

// ---- 7: search on fts2 ----

func TestQAFilenameOnlyMatchRanksBelowBodyAndAppearsOnce(t *testing.T) {
	c := openCache(t)
	indexRaw(t, c, "a", "p1", msgHdr("Bob <bob@example.com>", "Offer", d0.Add(3*time.Hour), partText("see attached"), partPDF("thermostat-quote.pdf", miniPDF("Thermostat Quotation 4711"))))
	indexRaw(t, c, "a", "p2", msgHdr("Bob <bob@example.com>", "Both", d0.Add(time.Hour), partText("The Thermostat arrived"), partPDF("thermostat-manual.pdf", miniPDF("x"))))
	ctx := tctx(t)

	hits, _, err := c.Search(ctx, SearchQuery{Text: "Quotation"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("PDF text must not be searchable: %+v %v", hits, err)
	}
	hits, _, err = c.Search(ctx, SearchQuery{Text: "quote"})
	if err != nil || len(hits) != 1 || hits[0].StableID != "p1" || hits[0].Snippet != "attachment: thermostat-[quote].pdf" {
		t.Fatalf("filename-only: %+v %v", hits, err)
	}
	// p1 is newer, but only matches by file name: it ranks below the body hit p2,
	// and p2 (body and file name both match) appears once with a body snippet.
	hits, _, err = c.Search(ctx, SearchQuery{Text: "Thermostat"})
	if err != nil || len(hits) != 2 || hits[0].StableID != "p2" || hits[1].StableID != "p1" {
		t.Fatalf("ranking: %+v %v", hits, err)
	}
	if strings.Contains(hits[0].Snippet, "attachment:") || !strings.HasPrefix(hits[1].Snippet, "attachment:") {
		t.Errorf("snippets %q / %q", hits[0].Snippet, hits[1].Snippet)
	}
	// Two matching file names in one message: still one hit.
	indexRaw(t, c, "a", "p3", msgHdr("Bob <bob@example.com>", "Two", d0.Add(2*time.Hour), partText("x"), partPDF("gazebo-plans.pdf", miniPDF("a")), partPDF("gazebo-costs.pdf", miniPDF("b"))))
	if hits, _, _ := c.Search(ctx, SearchQuery{Text: "Gazebo"}); len(hits) != 1 {
		t.Fatalf("message with two matching names appears %d times", len(hits))
	}
	// The limit is applied after excluding messages that also match by body.
	for i := range 5 {
		indexRaw(t, c, "a", fmt.Sprintf("q%d", i), msgHdr("Bob <bob@example.com>", "Q", d0.Add(time.Duration(10+i)*time.Hour), partText("zeppelin body"), partPDF("zeppelin.pdf", miniPDF("a"))))
	}
	indexRaw(t, c, "a", "qold", msgHdr("Bob <bob@example.com>", "Q", d0, partText("x"), partPDF("zeppelin-old.pdf", miniPDF("a"))))
	hits, _, err = c.Search(ctx, SearchQuery{Text: "zeppelin", Limit: 6})
	if err != nil || len(hits) != 6 || hits[5].StableID != "qold" {
		t.Fatalf("overlap displaced a row: %v %v", hitIDs(hits), err)
	}
}

func TestQAAttachmentBranchHonoursAccountAndOtherFilters(t *testing.T) {
	c := openCache(t)
	pdf := func(n string) string { return partPDF("zebrafish-"+n, miniPDF("census")) }
	mk := func(from, subj string, d time.Time) []byte {
		return msgHdr(from, subj, d, partText("nothing relevant"), pdf("z.pdf"))
	}
	d1, d2, d3 := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	indexRaw(t, c, "A", "m1", mk("Alice <alice@a.example>", "one", d1), "INBOX")
	indexRaw(t, c, "A", "m2", mk("Carol <carol@c.example>", "two", d2), "Archive")
	indexRaw(t, c, "B", "m3", mk("Alice <alice@a.example>", "three", d3), "INBOX")
	// A message in B with the SAME stable id as A's m1 but no PDF match.
	indexRaw(t, c, "B", "m1", msgHdr("Bob <bob@b.example>", "plain", d1, partText("nothing")), "INBOX")
	// An A-only match for the strict isolation check.
	indexRaw(t, c, "A", "only", msgHdr("Zed <zed@a.example>", "only", d1, partText("x"), partPDF("quokka.pdf", miniPDF("habitat"))), "INBOX")
	ctx := tctx(t)

	for _, tc := range []struct {
		name string
		q    SearchQuery
		want []string
	}{
		{"no filter", SearchQuery{Text: "Zebrafish"}, []string{"A/m1", "A/m2", "B/m3"}},
		{"account A", SearchQuery{Text: "Zebrafish", Account: "A"}, []string{"A/m1", "A/m2"}},
		{"account B", SearchQuery{Text: "Zebrafish", Account: "B"}, []string{"B/m3"}},
		{"A-only match, restricted to B", SearchQuery{Text: "Quokka", Account: "B"}, nil},
		{"A-only match, restricted to A", SearchQuery{Text: "Quokka", Account: "A"}, []string{"A/only"}},
		{"A-only match, unrestricted", SearchQuery{Text: "Quokka"}, []string{"A/only"}},
		{"folder Archive", SearchQuery{Text: "Zebrafish", Folder: "Archive"}, []string{"A/m2"}},
		{"folder INBOX", SearchQuery{Text: "Zebrafish", Folder: "INBOX"}, []string{"A/m1", "B/m3"}},
		{"account B folder Archive", SearchQuery{Text: "Zebrafish", Account: "B", Folder: "Archive"}, nil},
		{"from carol", SearchQuery{Text: "Zebrafish", From: "carol"}, []string{"A/m2"}},
		{"from alice", SearchQuery{Text: "Zebrafish", From: "alice"}, []string{"A/m1", "B/m3"}},
		{"account B from carol", SearchQuery{Text: "Zebrafish", Account: "B", From: "carol"}, nil},
		{"since feb", SearchQuery{Text: "Zebrafish", Since: d2}, []string{"A/m2", "B/m3"}},
		{"until feb", SearchQuery{Text: "Zebrafish", Until: d2}, []string{"A/m1"}},
		{"since+until", SearchQuery{Text: "Zebrafish", Since: d2, Until: d3}, []string{"A/m2"}},
		{"account B since+until excludes", SearchQuery{Text: "Zebrafish", Account: "B", Since: d1, Until: d3}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits, _, err := c.Search(ctx, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := hitIDs(hits); !eqs(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for _, h := range hits {
				if !strings.HasPrefix(h.Snippet, "attachment:") {
					t.Errorf("%s: snippet %q", h.StableID, h.Snippet)
				}
			}
		})
	}
}

func TestQAFts2FoldsDiacritics(t *testing.T) {
	c := openCache(t)
	indexRaw(t, c, "a", "d1", msgHdr("Bob <bob@example.com>", "Menu", d0, partText("Meet at the Café in Zürich"), partPDF("résumé.pdf", miniPDF("curriculum vitae"))))
	indexRaw(t, c, "a", "d2", msgHdr("Bob <bob@example.com>", "Plain", d0.Add(time.Hour), partText("the cafe is closed")))
	ctx := tctx(t)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"cafe", []string{"a/d1", "a/d2"}},
		{"café", []string{"a/d1", "a/d2"}},
		{"CAFÉ", []string{"a/d1", "a/d2"}},
		{"zurich", []string{"a/d1"}},
		{"resume", []string{"a/d1"}}, // the PDF's filename, via the attachment branch
	} {
		hits, _, err := c.Search(ctx, SearchQuery{Text: tc.q})
		if err != nil || !eqs(hitIDs(hits), tc.want) {
			t.Errorf("%q: got %v %v, want %v", tc.q, hitIDs(hits), err, tc.want)
		}
	}
}

// ---- 8: status ----

func TestQABackfillStatusDoneEqualsTotalWhenComplete(t *testing.T) {
	c := openCache(t)
	seedLegacy(t, c, 7)
	c = reopenAsPreUpgrade(t, c)
	ctx := tctx(t)
	st, _ := c.BackfillStatus(ctx)
	if st.Complete || st.Total != 7 || st.Done != 0 {
		t.Fatalf("before: %+v", st)
	}
	c.RunBackfill(ctx, quiet())
	st, err := c.BackfillStatus(ctx)
	if err != nil || !st.Complete || st.Done != st.Total || st.Total != 7 {
		t.Fatalf("after: %+v %v", st, err)
	}
	_ = filepath.Join // keep import used if tests above change
}
