package cache

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/ledongthuc/pdf"
)

// Limits on what is read from an attachment for indexing.
const (
	maxPDFBytes     = 10 << 20        // larger PDFs are recorded but not parsed
	maxPDFText      = 256 << 10       // extracted text kept per PDF
	pdfBudget       = 5 * time.Second // wall time for one PDF
	maxPDFInflight  = 4               // abandoned-but-running parses tolerated
	maxAttachmentsN = maxAttachments  // per message, same bound as the listing
	maxMIMEDepth    = 16
)

// AttachmentMeta is what the index keeps about one attachment or inline part.
// Part is the MIME part path ("2", "2.1"): the position of the part among the
// children at each multipart level, 1-based, so it identifies the part inside
// the blob without needing the filename.
type AttachmentMeta struct {
	Part      string
	Filename  string
	Mime      string
	Size      int64 // decoded size
	SHA256    string
	Inline    bool
	ContentID string // without angle brackets; what "cid:" refers to

	// Text is the extracted text of a text-based PDF, empty otherwise.
	Text          string
	TextExtracted bool
}

// pdfInflight counts PDF parses still running, including ones whose caller
// gave up after the budget. The library takes no context, so a hostile PDF can
// keep spinning after we stopped waiting; this bounds how many may pile up.
var pdfInflight atomic.Int32

// pdfText extracts text from a PDF held in data. It never panics and never
// blocks longer than pdfBudget. The second result is false when no text was
// obtained (parse failure, timeout, image-only PDF).
func pdfText(data []byte) (string, bool) {
	if len(data) == 0 || len(data) > maxPDFBytes {
		return "", false
	}
	if pdfInflight.Load() >= maxPDFInflight {
		return "", false
	}
	pdfInflight.Add(1)
	type result struct {
		text string
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		defer pdfInflight.Add(-1)
		defer func() {
			if r := recover(); r != nil {
				ch <- result{}
			}
		}()
		if !pdfBounded(data) {
			ch <- result{}
			return
		}
		text, ok := readPDF(data)
		ch <- result{text, ok}
	}()
	t := time.NewTimer(pdfBudget)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.text, r.ok
	case <-t.C:
		return "", false
	}
}

// Bounds enforced by pdfBounded before the library sees a PDF.
const (
	maxPDFStreams   = 20000    // stream objects
	maxPDFStreamOut = 16 << 20 // decoded bytes of one stream
	maxPDFTotalOut  = 48 << 20 // decoded bytes over all streams
	maxPDFXrefSize  = 200000   // /Size of the cross-reference table
)

var (
	pdfSizeRE      = regexp.MustCompile(`/Size\s+(\d+)`)
	pdfOtherFilter = regexp.MustCompile(`/(LZWDecode|RunLengthDecode|ASCII85Decode|ASCIIHexDecode|LZW|RL|A85|AHx)\b`)
)

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// pdfBounded reports whether data is safe to hand to the PDF library.
// ledongthuc/pdf has no hook to limit what it decodes or allocates (it
// inflates streams with an unbounded read and sizes the xref table from the
// file's own /Size), so the limits are enforced here, up front:
//   - /Size values above maxPDFXrefSize are refused (bounds the xref table);
//   - the file is scanned for every "stream" keyword, at most maxPDFStreams;
//   - each FlateDecode stream is inflated into a counter and refused beyond
//     maxPDFStreamOut, and all of them beyond maxPDFTotalOut (a zip bomb);
//   - streams using filters that are not inflated here (LZW, RunLength,
//     ASCII85, ASCIIHex) are refused, since their output cannot be bounded
//     the same way; they are rare in mailed PDFs.
//
// This is a pre-scan, not a guarantee against every parser quirk: the library
// still builds page text and object tables in memory, but only from inputs
// bounded above, and the whole parse stays under the 5 s budget.
func pdfBounded(data []byte) bool {
	for _, m := range pdfSizeRE.FindAllSubmatch(data, -1) {
		if n, err := strconv.ParseInt(string(m[1]), 10, 64); err != nil || n > maxPDFXrefSize {
			return false
		}
	}
	var total int64
	streams := 0
	for pos := 0; ; {
		i := bytes.Index(data[pos:], []byte("stream"))
		if i < 0 {
			return true
		}
		i += pos
		pos = i + len("stream")
		if i >= 3 && string(data[i-3:i]) == "end" {
			continue
		}
		if streams++; streams > maxPDFStreams {
			return false
		}
		dict := data[max(0, i-1024):i]
		if o := bytes.LastIndex(dict, []byte("obj")); o >= 0 {
			dict = dict[o:]
		}
		start := pos
		if start < len(data) && data[start] == '\r' {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		}
		end := bytes.Index(data[start:], []byte("endstream"))
		if end < 0 {
			end = len(data) - start
		}
		if pdfOtherFilter.Match(dict) {
			return false
		}
		if bytes.Contains(dict, []byte("/FlateDecode")) || bytes.Contains(dict, []byte("/Fl ")) {
			zr, err := zlib.NewReader(bytes.NewReader(data[start : start+end]))
			if err != nil {
				continue // the library will fail on it too
			}
			var cw countWriter
			_, _ = io.Copy(&cw, io.LimitReader(zr, maxPDFStreamOut+1))
			_ = zr.Close()
			if cw.n > maxPDFStreamOut {
				return false
			}
			if total += cw.n; total > maxPDFTotalOut {
				return false
			}
		}
		pos = start + end
	}
}

// readPDF does the extraction; callers recover from panics. It reads page by
// page and stops at maxPDFText.
func readPDF(data []byte) (string, bool) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false
	}
	var b strings.Builder
	n := r.NumPage()
	for i := 1; i <= n && b.Len() < maxPDFText; i++ {
		s, err := r.Page(i).GetPlainText(nil)
		if err != nil {
			continue // one bad page does not lose the others
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	s := collapseSpace(strings.ToValidUTF8(b.String(), ""))
	if len(s) > maxPDFText {
		s = strings.ToValidUTF8(s[:maxPDFText], "")
	}
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

func collapseSpace(s string) string {
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(blank.ReplaceAllString(s, "\n"))
}

// capWriter keeps the first limit bytes written and counts the rest.
type capWriter struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if !w.over {
		if w.buf.Len()+len(p) > w.limit {
			w.over = true
			w.buf = bytes.Buffer{} // too big to parse: release what was held
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

// isPDF reports whether a part should be parsed as a PDF.
func isPDF(mt, name string) bool {
	if mt == "application/pdf" || mt == "application/x-pdf" {
		return true
	}
	return (mt == "application/octet-stream" || mt == "") && strings.EqualFold(filepath.Ext(name), ".pdf")
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.Reader}

func decodeName(s string) string {
	if strings.Contains(s, "=?") {
		if d, err := wordDecoder.DecodeHeader(s); err == nil {
			s = d
		}
	}
	return capField(strings.ToValidUTF8(s, ""))
}

// extractAttachments walks the MIME tree of raw and reports every attachment
// and inline part: anything marked attachment, carrying a filename or a
// Content-ID, or not text. The plain and HTML body parts themselves are not
// attachments. Each part is streamed through a hash; only PDFs are buffered
// (up to maxPDFBytes) for text extraction. A bounded number of parts is
// recorded, so a hostile tree cannot flood the table.
func extractAttachments(raw []byte) []AttachmentMeta {
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		return nil
	}
	var out []AttachmentMeta
	walkAttachments(e, "", 0, &out)
	return out
}

func walkAttachments(e *message.Entity, path string, depth int, out *[]AttachmentMeta) {
	if depth > maxMIMEDepth || len(*out) >= maxAttachmentsN {
		return
	}
	if mr := e.MultipartReader(); mr != nil {
		for i := 1; ; i++ {
			p, err := mr.NextPart()
			if err != nil && (p == nil || errors.Is(err, io.EOF) || !message.IsUnknownCharset(err)) {
				return
			}
			child := strconv.Itoa(i)
			if path != "" {
				child = path + "." + child
			}
			walkAttachments(p, child, depth+1, out)
		}
	}
	if path == "" {
		path = "1"
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
	cid := strings.Trim(strings.TrimSpace(e.Header.Get("Content-Id")), "<>")
	isText := mt == "text/plain" || mt == "text/html"
	if disp != "attachment" && name == "" && cid == "" && isText {
		return // a body part
	}
	m := AttachmentMeta{
		Part:      path,
		Filename:  decodeName(name),
		Mime:      capField(mt),
		Inline:    disp == "inline" || (disp != "attachment" && cid != ""),
		ContentID: capField(cid),
	}
	h := sha256.New()
	var dst io.Writer = h
	var cw *capWriter
	if isPDF(mt, name) {
		cw = &capWriter{limit: maxPDFBytes}
		dst = io.MultiWriter(h, cw)
	}
	n, _ := io.Copy(dst, e.Body)
	m.Size = n
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	if cw != nil && !cw.over {
		m.Text, m.TextExtracted = pdfText(cw.buf.Bytes())
	}
	*out = append(*out, m)
}

// insertAttachmentsTx records atts for the message (account, stableID) inside
// tx. attachments carries one row per part; attachment_fts one row per part
// with extracted text, its rowid equal to the attachments rowid so a hit
// resolves to its part without a scan.
func insertAttachmentsTx(ctx context.Context, tx *sql.Tx, account, stableID string, atts []AttachmentMeta) error {
	for _, a := range atts {
		inline := 0
		if a.Inline {
			inline = 1
		}
		extracted := 0
		if a.TextExtracted {
			extracted = 1
		}
		res, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO attachments (account, stable_id, part, filename, mime, size, sha256, is_inline, content_id, text_extracted)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account, stableID, a.Part, a.Filename, a.Mime, a.Size, a.SHA256, inline, a.ContentID, extracted)
		if err != nil {
			return fmt.Errorf("index attachment: %w", err)
		}
		if !a.TextExtracted {
			continue
		}
		rid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("index attachment: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO attachment_fts (rowid, account, stable_id, part, filename, text) VALUES (?, ?, ?, ?, ?, ?)`,
			rid, account, stableID, a.Part, a.Filename, a.Text); err != nil {
			return fmt.Errorf("index attachment text: %w", err)
		}
	}
	return nil
}
