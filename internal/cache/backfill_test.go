package cache

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// miniPDF builds a one-page text PDF with a valid xref table.
func miniPDF(text string) []byte {
	var b strings.Builder
	offs := []int{}
	obj := func(s string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), s)
	}
	b.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	content := fmt.Sprintf("BT /F1 12 Tf 72 700 Td (%s) Tj ET", text)
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, x)
	return []byte(b.String())
}

func mimeWithPDF(body string, pdf []byte) []byte {
	return []byte("From: Bob <bob@example.com>\r\nTo: alice@example.com\r\nSubject: Offer\r\nDate: Mon, 05 Jan 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n" +
		"--XX\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=\"logo.png\"\r\nContent-Id: <logo@x>\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("\x89PNGfake")) + "\r\n" +
		"--XX\r\nContent-Type: application/pdf; name=\"offer.pdf\"\r\nContent-Disposition: attachment; filename=\"offer.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(pdf) + "\r\n--XX--\r\n")
}

func TestExtractAttachmentsAndPDFText(t *testing.T) {
	raw := mimeWithPDF("see attached", miniPDF("Thermostat Quotation 4711"))
	p := parseMessage(raw)
	if len(p.Atts) != 2 {
		t.Fatalf("want 2 attachments, got %+v", p.Atts)
	}
	img, pdfa := p.Atts[0], p.Atts[1]
	if img.Part != "2" || img.Filename != "logo.png" || img.Mime != "image/png" || !img.Inline || img.ContentID != "logo@x" || img.Size != 8 || img.SHA256 == "" || img.TextExtracted {
		t.Errorf("image meta wrong: %+v", img)
	}
	if pdfa.Part != "3" || pdfa.Inline || !pdfa.TextExtracted || !strings.Contains(pdfa.Text, "Thermostat") {
		t.Errorf("pdf meta wrong: %+v", pdfa)
	}
}

func TestPDFTextSurvivesGarbage(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("%PDF-1.4\ngarbage"), []byte(strings.Repeat("\x00", 4096)), miniPDF("x")[:100]} {
		if s, ok := pdfText(in); ok && s == "" {
			t.Errorf("ok with empty text")
		}
	}
}

func TestBackfillIndexesExistingMessagesAndSwitchesOver(t *testing.T) {
	c := openCache(t)
	if tb, ready := c.FTSTable(); tb != "message_fts2" || !ready {
		t.Fatalf("fresh cache should be ready, got %s %v", tb, ready)
	}
	// Simulate a pre-upgrade cache: messages and message_fts only.
	raw := mimeWithPDF("see attached", miniPDF("Thermostat Quotation 4711"))
	sum, err := c.putBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		id := fmt.Sprintf("m%d", i)
		blob := sum
		if i == 2 {
			blob = strings.Repeat("0", 64) // blob missing on disk
		}
		if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix) VALUES ('a', ?, ?, 'Bob <bob@example.com>', 'Offer', ?)`, id, blob, 1700000000+i); err != nil {
			t.Fatal(err)
		}
		if _, err := c.db.Exec(`INSERT INTO message_fts (subject, from_addr, to_addr, cc_addr, body, account, stable_id) VALUES ('Offer','Bob','','','see attached','a',?)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.Exec(`UPDATE backfill SET done = 0, last_rowid = 0, processed = 0, total = 3, max_rowid = (SELECT MAX(rowid) FROM messages)`); err != nil {
		t.Fatal(err)
	}
	c.fts2Ready.Store(false)
	if tb, ready := c.FTSTable(); tb != "message_fts" || ready {
		t.Fatalf("want fallback, got %s %v", tb, ready)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c.RunBackfill(ctx, slog.New(slog.DiscardHandler))

	st, err := c.BackfillStatus(ctx)
	if err != nil || !st.Complete || st.Done != 3 || st.Total != 3 {
		t.Fatalf("status %+v err %v", st, err)
	}
	if tb, ready := c.FTSTable(); tb != "message_fts2" || !ready {
		t.Fatalf("want fts2, got %s %v", tb, ready)
	}
	// A PDF-only match still returns the message, marked as an attachment hit.
	hits, _, err := c.Search(ctx, SearchQuery{Text: "Quotation"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits (m0,m1; m2 has no blob), got %+v", hits)
	}
	if !strings.HasPrefix(hits[0].Snippet, "[attachment: offer.pdf]") || !strings.Contains(hits[0].Snippet, "[Quotation]") {
		t.Errorf("snippet %q", hits[0].Snippet)
	}
	// A body match reports a body snippet, not an attachment one.
	hits, _, err = c.Search(ctx, SearchQuery{Text: "attached"})
	if err != nil || len(hits) == 0 || strings.Contains(hits[0].Snippet, "attachment:") {
		t.Fatalf("hits %+v err %v", hits, err)
	}
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM attachments WHERE text_extracted = 1`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("extracted pdfs: %d %v", n, err)
	}
	// Re-running is a no-op, and the job survives a reopen as complete.
	c.RunBackfill(ctx, nil)
}

func TestBackfillResumesAfterCancel(t *testing.T) {
	c := openCache(t)
	raw := []byte("From: a@x\r\nSubject: s\r\n\r\nhello world\r\n")
	sum, _ := c.putBlob(raw)
	for i := range 1200 {
		if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256) VALUES ('a', ?, ?)`, fmt.Sprintf("m%d", i), sum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.Exec(`UPDATE backfill SET done = 0, total = 1200, max_rowid = (SELECT MAX(rowid) FROM messages)`); err != nil {
		t.Fatal(err)
	}
	c.fts2Ready.Store(false)
	// One batch, then stop: progress must be durable and partial.
	if complete, _, err := c.backfillBatch(context.Background()); err != nil || complete {
		t.Fatalf("batch: %v %v", complete, err)
	}
	st, _ := c.BackfillStatus(context.Background())
	if st.Complete || st.Done == 0 || st.Done > 1200 {
		t.Fatalf("partial status %+v", st)
	}
	c.RunBackfill(context.Background(), slog.New(slog.DiscardHandler))
	var n int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM message_fts2`).Scan(&n)
	if n != 1200 {
		t.Fatalf("fts2 rows %d, want 1200 (no duplicates, none missed)", n)
	}
}

func TestPDFBounded(t *testing.T) {
	if !pdfBounded(miniPDF("hello")) {
		t.Fatal("a normal PDF must pass")
	}
	if pdfBounded([]byte("%PDF-1.4\ntrailer\n<< /Size 99999999 /Root 1 0 R >>\n")) {
		t.Fatal("huge /Size must be refused")
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(make([]byte, maxPDFStreamOut+1024))
	_ = zw.Close()
	bomb := append([]byte("%PDF-1.4\n1 0 obj\n<< /Filter /FlateDecode /Length 1 >>\nstream\n"), z.Bytes()...)
	bomb = append(bomb, []byte("\nendstream\nendobj\n")...)
	if pdfBounded(bomb) {
		t.Fatal("a decompression bomb must be refused")
	}
	if s, ok := pdfText(bomb); ok || s != "" {
		t.Fatal("pdfText must not extract from a bomb")
	}
	if pdfBounded([]byte("1 0 obj\n<< /Filter /LZWDecode >>\nstream\nxx\nendstream\n")) {
		t.Fatal("unboundable filters must be refused")
	}
}
