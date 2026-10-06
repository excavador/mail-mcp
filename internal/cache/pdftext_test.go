package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeX is a PDFExtractor that parses nothing: it reads the staged file the
// way the sidecar would (by hash, from the pdf directory) and answers from a
// table keyed by a marker in the file.
type fakeX struct {
	c *Cache

	mu     sync.Mutex
	calls  []string
	status map[string]string // marker -> status (default ok)
	failN  map[string]int    // marker -> transport errors still to return
	down   bool              // ErrPDFUnavailable for everything
}

var markerRE = regexp.MustCompile(`MARK-[A-Za-z0-9]+`)

func (f *fakeX) Extract(_ context.Context, sha string) (PDFResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sha)
	if f.down {
		return PDFResult{}, ErrPDFUnavailable
	}
	b, err := os.ReadFile(f.c.pdfFile(sha))
	if err != nil {
		return PDFResult{}, fmt.Errorf("fake: file not staged: %w", err)
	}
	if !strings.HasPrefix(string(b), "%PDF-") {
		return PDFResult{Status: "not_pdf"}, nil
	}
	m := markerRE.FindString(string(b))
	if f.failN[m] > 0 {
		f.failN[m]--
		return PDFResult{}, errors.New("fake transport error")
	}
	if s := f.status[m]; s != "" && s != "ok" {
		return PDFResult{Status: s}, nil
	}
	return PDFResult{Status: "ok", Text: "Invoice   for " + m + "\n\n\n\n  Wärmepumpe 4711   total \x01 EUR 99"}, nil
}

func (f *fakeX) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// addPDFMessage caches a message carrying one PDF (by content) and indexes it
// the way refresh does.
func addPDFMessage(t *testing.T, c *Cache, id string, pdf []byte) {
	t.Helper()
	raw := mimeWithPDF("see attached "+id, pdf)
	sum, err := c.putBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix) VALUES ('a', ?, ?, 'Bob <bob@example.com>', 'Offer', 1700000000)`, id, sum)
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := res.LastInsertId()
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := indexText2Tx(context.Background(), tx, rid, "a", id, parseMessage(raw)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func pdfDoc(marker string) []byte { return []byte("%PDF-1.4\n% " + marker + "\n") }

func searchIDs(t *testing.T, c *Cache, q string) (ids []string, snippet string) {
	t.Helper()
	hits, _, err := c.Search(context.Background(), SearchQuery{Text: q})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		ids = append(ids, h.StableID)
		snippet = h.Snippet
	}
	return ids, snippet
}

func TestPDFTextJobIndexesCachesByHashAndSearchFindsIt(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c}
	log := slog.New(slog.DiscardHandler)

	addPDFMessage(t, c, "m1", pdfDoc("MARK-Alpha"))
	addPDFMessage(t, c, "m2", pdfDoc("MARK-Alpha")) // the same file forwarded
	addPDFMessage(t, c, "m3", pdfDoc("MARK-Beta"))

	// Without an extractor nothing happens and nothing is searchable.
	c.RunPDFText(ctx, log)
	if ids, _ := searchIDs(t, c, "4711"); len(ids) != 0 {
		t.Fatalf("hits before extraction: %v", ids)
	}
	if st, _ := c.PDFTextStatus(ctx); st.Enabled || st.State != "disabled" || st.Total != 3 || st.Done != 0 {
		t.Fatalf("status without extractor: %+v", st)
	}

	c.SetPDFExtractor(x)
	if err := c.RunPDFTextOnce(ctx, log); err != nil {
		t.Fatal(err)
	}
	// Extracted once per distinct file, cached by hash for the forward.
	if x.n() != 2 {
		t.Fatalf("extractor called %d times, want 2 (one per distinct hash)", x.n())
	}
	ids, snip := searchIDs(t, c, "4711")
	if len(ids) != 3 {
		t.Fatalf("search 4711 found %v, want all three messages", ids)
	}
	if !strings.HasPrefix(snip, "attachment: ") || !strings.Contains(snip, "[4711]") {
		t.Errorf("snippet %q does not show the PDF text", snip)
	}
	if ids, _ := searchIDs(t, c, "wärmepumpe"); len(ids) != 3 { // diacritics fold
		t.Errorf("accented term: %v", ids)
	}
	if ids, _ := searchIDs(t, c, "Beta"); len(ids) != 1 || ids[0] != "m3" {
		t.Errorf("per-file term: %v", ids)
	}
	// Control characters never reach the index or the stored text.
	var stored string
	if err := c.db.QueryRow(`SELECT text FROM pdf_text WHERE status = 'ok' LIMIT 1`).Scan(&stored); err != nil || strings.Contains(stored, "\x01") || strings.Contains(stored, "    ") {
		t.Errorf("stored text not tidied: %q %v", stored, err)
	}
	// The staged files are gone.
	if es, _ := os.ReadDir(c.pdfDir()); len(es) > 0 {
		for _, e := range es {
			if sub, _ := os.ReadDir(c.pdfDir() + "/" + e.Name()); len(sub) > 0 {
				t.Errorf("staged file left behind in %s", e.Name())
			}
		}
	}
	st, _ := c.PDFTextStatus(ctx)
	if !st.Complete || st.Done != 3 || st.Outcomes["ok"] != 2 {
		t.Fatalf("status: %+v", st)
	}

	// Idempotent: another pass changes nothing and calls nothing.
	if err := c.RunPDFTextOnce(ctx, log); err != nil || x.n() != 2 {
		t.Fatalf("second pass: err=%v calls=%d", err, x.n())
	}
	// A new message with an already-known file is satisfied from the cache.
	addPDFMessage(t, c, "m4", pdfDoc("MARK-Alpha"))
	if err := c.RunPDFTextOnce(ctx, log); err != nil || x.n() != 2 {
		t.Fatalf("known file re-extracted: err=%v calls=%d", err, x.n())
	}
	if ids, _ := searchIDs(t, c, "4711"); len(ids) != 4 {
		t.Errorf("new message not searchable: %v", ids)
	}
}

func TestPDFTextJobSkipsNonPDFs(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c}
	c.SetPDFExtractor(x)
	// mimeWithPDF carries a PNG too; add a text attachment and a PDF with no
	// content type but a .pdf name.
	raw := []byte("From: a@x\r\nSubject: s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: text/plain; name=\"n.txt\"\r\nContent-Disposition: attachment; filename=\"n.txt\"\r\n\r\nMARK-Text\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"SCAN.PDF\"\r\n\r\n%PDF-1.4 MARK-Scan\r\n" +
		"--B\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"i.png\"\r\n\r\n\x89PNG\r\n--B--\r\n")
	sum, _ := c.putBlob(raw)
	res, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256) VALUES ('a','m',?)`, sum)
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := res.LastInsertId()
	tx, _ := c.db.Begin()
	if err := indexText2Tx(ctx, tx, rid, "a", "m", parseMessage(raw)); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if x.n() != 1 {
		t.Fatalf("extractor called %d times; only SCAN.PDF is a PDF", x.n())
	}
	if ids, _ := searchIDs(t, c, "Scan"); len(ids) != 1 {
		t.Errorf("PDF by file name not indexed: %v", ids)
	}
	var open int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM attachments WHERE text_extracted = 0`).Scan(&open)
	if open != 2 { // the text file and the PNG are never touched
		t.Errorf("non-PDF rows changed: %d still 0", open)
	}
}

func TestPDFTextJobRestartSafeAndRecordsOutcomes(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c, status: map[string]string{"MARK-Slow": "timeout", "MARK-Fake": "not_pdf"}, failN: map[string]int{}}
	c.SetPDFExtractor(x)
	addPDFMessage(t, c, "m1", pdfDoc("MARK-One"))
	addPDFMessage(t, c, "m2", pdfDoc("MARK-Slow"))
	addPDFMessage(t, c, "m3", pdfDoc("MARK-Two"))

	// The sidecar goes away after the first file: the pass stops with an
	// error, the first file is done, the rest are pending.
	calls := 0
	x.down = false
	c.SetPDFExtractor(extractorFunc(func(ctx context.Context, sha string) (PDFResult, error) {
		calls++
		if calls > 1 {
			return PDFResult{}, ErrPDFUnavailable
		}
		return x.Extract(ctx, sha)
	}))
	if err := c.RunPDFTextOnce(ctx, nil); !errors.Is(err, ErrPDFUnavailable) {
		t.Fatalf("want ErrPDFUnavailable, got %v", err)
	}
	st, _ := c.PDFTextStatus(ctx)
	if st.Done != 1 || st.Complete {
		t.Fatalf("after interruption: %+v", st)
	}
	// "Restart": the work resumes from the database; the finished file is not
	// extracted again, and a timeout is recorded, never retried.
	before := x.n()
	c.SetPDFExtractor(x)
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := x.n() - before; got != 2 {
		t.Fatalf("resume extracted %d files, want the 2 left", got)
	}
	st, _ = c.PDFTextStatus(ctx)
	if !st.Complete || st.Done != 3 || st.Outcomes["timeout"] != 1 || st.Outcomes["ok"] != 2 {
		t.Fatalf("final status: %+v", st)
	}
	before = x.n()
	if err := c.RunPDFTextOnce(ctx, nil); err != nil || x.n() != before {
		t.Fatalf("hostile file retried: err=%v", err)
	}
	if ids, _ := searchIDs(t, c, "Slow"); len(ids) != 0 {
		t.Errorf("timed-out file is searchable: %v", ids)
	}
}

type extractorFunc func(context.Context, string) (PDFResult, error)

func (f extractorFunc) Extract(ctx context.Context, sha string) (PDFResult, error) {
	return f(ctx, sha)
}

func TestPDFTextRepeatedTransportFailureIsRecordedAsFailedAndPassContinues(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c, failN: map[string]int{"MARK-Poison": 1000}}
	c.SetPDFExtractor(x)
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Poison")) // first in line
	addPDFMessage(t, c, "m2", pdfDoc("MARK-Fine"))
	// Each pass stops at the poison file with its transport error, but the
	// count survives the pass; the third pass records it and goes on.
	for i := 1; i < pdfMaxAttempts; i++ {
		if err := c.RunPDFTextOnce(ctx, nil); err == nil {
			t.Fatalf("pass %d: want the transport error", i)
		}
		if st, _ := c.PDFTextStatus(ctx); st.Done != 0 {
			t.Fatalf("pass %d: done=%d", i, st.Done)
		}
	}
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	s, _ := c.PDFTextStatus(ctx)
	if s.Outcomes["failed"] != 1 || s.Outcomes["ok"] != 1 || !s.Complete {
		t.Fatalf("poison file not recorded as failed with the next one processed: %+v", s)
	}
	if ids, _ := searchIDs(t, c, "Fine"); len(ids) != 1 {
		t.Errorf("file behind the poison one not indexed: %v", ids)
	}
	if _, ok := c.pdfAttempts["x"]; ok || len(c.pdfAttempts) != 0 {
		t.Errorf("attempt counters not cleared: %v", c.pdfAttempts)
	}
}

func TestPDFTextPendingRowFromARestartIsOneStrikeNotAVerdict(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c}
	c.SetPDFExtractor(x)
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Restarted"))
	var sha string
	_ = c.db.QueryRow(`SELECT sha256 FROM attachments WHERE mime = 'application/pdf'`).Scan(&sha)
	// (c) A drain or restart stopped the process between "pending" and the
	// final write. The good file must still be extracted and indexed.
	if err := c.markPDFPending(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.PDFTextStatus(ctx); st.Outcomes["ok"] != 1 || st.Outcomes["failed"] != 0 || !st.Complete {
		t.Fatalf("a good file was lost to a stale pending row: %+v", st)
	}
	if ids, _ := searchIDs(t, c, "Restarted"); len(ids) != 1 {
		t.Fatalf("not indexed: %v", ids)
	}

	// Only the third strike makes a file failed, with no further extraction.
	addPDFMessage(t, c, "m2", pdfDoc("MARK-Crashy"))
	var sha2 string
	_ = c.db.QueryRow(`SELECT sha256 FROM attachments WHERE stable_id = 'm2' AND mime = 'application/pdf'`).Scan(&sha2)
	if err := c.markPDFPending(ctx, sha2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE pdf_text SET strikes = ? WHERE sha256 = ?`, pdfMaxAttempts-1, sha2); err != nil {
		t.Fatal(err)
	}
	before := x.n()
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if x.n() != before {
		t.Fatal("a file with three strikes was extracted again")
	}
	if st, _ := c.PDFTextStatus(ctx); st.Outcomes["failed"] != 1 || st.Outcomes["pending"] != 0 || !st.Complete {
		t.Fatalf("%+v", st)
	}
	// An error return leaves no pending row behind.
	addPDFMessage(t, c, "m3", pdfDoc("MARK-Err"))
	x.failN = map[string]int{"MARK-Err": 1}
	if err := c.RunPDFTextOnce(ctx, nil); err == nil {
		t.Fatal("want error")
	}
	var n int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM pdf_text WHERE status = 'pending'`).Scan(&n)
	if n != 0 {
		t.Fatalf("pending rows left after a transport error: %d", n)
	}
}

func TestPDFTextBusyOnTheFinalWriteDoesNotDiscardTheExtraction(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c}
	c.SetPDFExtractor(x)
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Busy"))
	tries := 0
	pdfStoreHook = func() error {
		tries++
		if tries == 1 {
			return errors.New("database is locked (SQLITE_BUSY)")
		}
		return nil
	}
	defer func() { pdfStoreHook = nil }()
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if tries < 2 {
		t.Fatal("the write was not retried")
	}
	if x.n() != 1 {
		t.Fatalf("extractor called %d times: BUSY must retry the write, not the extraction", x.n())
	}
	if st, _ := c.PDFTextStatus(ctx); st.Outcomes["ok"] != 1 || st.Outcomes["failed"] != 0 {
		t.Fatalf("%+v", st)
	}
	if ids, _ := searchIDs(t, c, "Busy"); len(ids) != 1 {
		t.Fatalf("not indexed: %v", ids)
	}
}

func TestPDFTextStageWriteErrorIsNotAFileOutcome(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c}
	c.SetPDFExtractor(x)
	// A stage root that is a file: creating directories under it fails, as a
	// full or broken emptyDir would.
	root := t.TempDir() + "/file"
	_ = os.WriteFile(root, []byte("x"), 0o600)
	c.SetPDFStageDir(root)
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Stage"))
	for i := 0; i < pdfMaxAttempts+2; i++ {
		if err := c.RunPDFTextOnce(ctx, nil); !errors.Is(err, ErrPDFUnavailable) {
			t.Fatalf("pass %d: want a retryable error, got %v", i, err)
		}
	}
	if st, _ := c.PDFTextStatus(ctx); st.Outcomes["failed"] != 0 || st.Done != 0 {
		t.Fatalf("a stage write error became the file's verdict: %+v", st)
	}
}

func TestPDFTextStagesInADedicatedDirectory(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	stage := t.TempDir()
	c.SetPDFStageDir(stage)
	_ = os.MkdirAll(stage+"/pdf/ab", 0o700)
	_ = os.WriteFile(stage+"/pdf/ab/leftover", []byte("x"), 0o600)
	var seen string
	c.SetPDFExtractor(extractorFunc(func(_ context.Context, sha string) (PDFResult, error) {
		seen = c.pdfFile(sha)
		if _, err := os.Stat(seen); err != nil {
			return PDFResult{}, err
		}
		return PDFResult{Status: "ok", Text: "Staged"}, nil
	}))
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Stage"))
	_ = os.RemoveAll(c.pdfDir()) // what RunPDFText does at start; the root (a mount point) stays
	if _, err := os.Stat(stage + "/pdf/ab/leftover"); err == nil {
		t.Fatal("leftover survived the startup cleanup")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("the staging directory itself was removed")
	}
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, stage+"/pdf/") {
		t.Fatalf("staged at %q, want under %s", seen, stage)
	}
	if ids, _ := searchIDs(t, c, "Staged"); len(ids) != 1 {
		t.Fatalf("not indexed: %v", ids)
	}
}

func TestPDFTextLimitsAndMissingParts(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	x := &fakeX{c: c}
	c.SetPDFExtractor(x)
	addPDFMessage(t, c, "big", pdfDoc("MARK-Big"))
	addPDFMessage(t, c, "gone", pdfDoc("MARK-Gone"))
	// The index claims a size over the limit: refused without staging.
	if _, err := c.db.Exec(`UPDATE attachments SET size = ? WHERE stable_id = 'big' AND mime = 'application/pdf'`, maxPDFInput+1); err != nil {
		t.Fatal(err)
	}
	// The blob of the other is gone.
	var blob string
	_ = c.db.QueryRow(`SELECT blob_sha256 FROM messages WHERE stable_id = 'gone'`).Scan(&blob)
	_ = os.Remove(c.BlobPath(blob))
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if x.n() != 0 {
		t.Fatalf("extractor called %d times", x.n())
	}
	st, _ := c.PDFTextStatus(ctx)
	if !st.Complete || st.Outcomes["too_large"] != 1 || st.Outcomes["failed"] != 1 {
		t.Fatalf("status: %+v", st)
	}
}

func TestPDFTextStoredTextIsCappedOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", maxPDFStored) // 2 bytes each
	s, cut := tidyPDFText(long)
	if !cut || len(s) > maxPDFStored || len(s) < maxPDFStored-3 || strings.ContainsRune(s, '�') {
		t.Fatalf("len=%d cut=%v", len(s), cut)
	}
}

func TestAttachmentTextsCaps(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	c.SetPDFExtractor(&fakeX{c: c})
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Cap"))
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	ts, err := c.AttachmentTexts(ctx, "a", "m1", 1<<20, 1<<20)
	if err != nil || len(ts) != 1 || ts[0].Filename != "offer.pdf" || ts[0].Truncated || !strings.Contains(ts[0].Text, "MARK-Cap") {
		t.Fatalf("%+v %v", ts, err)
	}
	ts, _ = c.AttachmentTexts(ctx, "a", "m1", 10, 1<<20)
	if len(ts) != 1 || len(ts[0].Text) > 10 || !ts[0].Truncated {
		t.Fatalf("per-attachment cap: %+v", ts)
	}
	if ts, _ := c.AttachmentTexts(ctx, "a", "m1", 100, 0); len(ts) != 0 {
		t.Fatalf("total cap 0: %+v", ts)
	}
	if ts, _ := c.AttachmentTexts(ctx, "a", "nope", 100, 100); len(ts) != 0 {
		t.Fatalf("unknown message: %+v", ts)
	}
}

func TestPDFTextRepeatedHelperUnavailableForOneFileBecomesFailed(t *testing.T) {
	c := openCache(t)
	ctx := tctx(t)
	good := &fakeX{c: c}
	c.SetPDFExtractor(extractorFunc(func(ctx context.Context, sha string) (PDFResult, error) {
		b, _ := os.ReadFile(c.pdfFile(sha))
		if strings.Contains(string(b), "MARK-Oom") {
			return PDFResult{}, ErrPDFHelperUnavailable
		}
		return good.Extract(ctx, sha)
	}))
	addPDFMessage(t, c, "m1", pdfDoc("MARK-Oom")) // first in line
	addPDFMessage(t, c, "m2", pdfDoc("MARK-Fine"))
	for i := 1; i < pdfMaxAttempts; i++ {
		if err := c.RunPDFTextOnce(ctx, nil); !errors.Is(err, ErrPDFUnavailable) {
			t.Fatalf("pass %d: %v", i, err)
		}
		if st, _ := c.PDFTextStatus(ctx); st.Outcomes["failed"] != 0 {
			t.Fatalf("pass %d recorded a verdict early: %+v", i, st)
		}
	}
	if err := c.RunPDFTextOnce(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.PDFTextStatus(ctx); st.Outcomes["failed"] != 1 || st.Outcomes["ok"] != 1 || !st.Complete {
		t.Fatalf("%+v", st)
	}
}
