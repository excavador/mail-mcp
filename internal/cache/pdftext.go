package cache

// The pdf_text job: text of PDF attachments, extracted by a sidecar.
//
// mail-mcp contains no PDF parser. In-process parsing was removed after a
// security review (decompression bombs, page-tree loops, text amplification),
// and the follow-up is this: the job takes the decoded attachment out of the
// message blob (a MIME walk, no PDF code), writes it under <cache>/pdf/ where
// the sidecar sees the store read-only, and asks the sidecar for text by the
// file's SHA-256. The sidecar runs pdftotext under hard limits and may be
// killed at will; whatever it says is bounded, cached by hash, and indexed
// into attachment_fts, so a search finds words inside an invoice.
//
// The job is the last of the scheduler's jobs, never touches IMAP, writes in
// short transactions like the others, and is idempotent: an attachment is
// done when text_extracted = 1, and every outcome (including a hostile file
// that timed out) is recorded by hash so it is never retried.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
)

// PDFResult is what an extractor reports for one file.
type PDFResult struct {
	Status      string // ok, timeout, too_large, failed, not_pdf
	Text        string
	Truncated   bool
	PagesCapped bool
}

// PDFExtractor extracts text from the file whose SHA-256 is sha. The file is
// at <cache dir>/pdf/<hh>/<sha>; the extractor never receives a path.
type PDFExtractor interface {
	Extract(ctx context.Context, sha string) (PDFResult, error)
}

// ErrPDFUnavailable means the extractor cannot take work now (its circuit
// breaker is open, or it is not running). The job stops and tries again later.
var ErrPDFUnavailable = errors.New("pdf extractor unavailable")

// SetPDFExtractor turns the pdf_text job on (nil turns it off).
func (c *Cache) SetPDFExtractor(x PDFExtractor) {
	c.pdfMu.Lock()
	c.pdfX = x
	c.pdfMu.Unlock()
}

func (c *Cache) pdfExtractor() PDFExtractor {
	c.pdfMu.RLock()
	defer c.pdfMu.RUnlock()
	return c.pdfX
}

const (
	// maxPDFInput is the largest attachment sent for extraction; the sidecar
	// enforces the same bound, this one saves writing the file.
	maxPDFInput = 25 << 20
	// maxPDFBlob is the largest message blob read to find an attachment
	// (25 MiB in base64 is about 34 MiB).
	maxPDFBlob = 48 << 20
	// maxPDFStored caps the text kept per file, in pdf_text and in the index:
	// the same bound as indexed body text.
	maxPDFStored = maxIndexedText

	pdfBatch       = 20
	pdfRetryAfter  = 2 * time.Minute  // after the extractor was unavailable
	pdfRescanEvery = 10 * time.Minute // new mail may carry new PDFs
	pdfMaxAttempts = 3                // transport failures on one file before it is recorded as failed
)

// pdfCond selects the attachments that are PDFs: by type, or by file name.
const pdfCond = `(lower(mime) = 'application/pdf' OR lower(filename) LIKE '%.pdf')`

var (
	runSpaces = regexp.MustCompile(`[ \t]{3,}`)
	runLines  = regexp.MustCompile(`\n{3,}`)
	lineTail  = regexp.MustCompile(`[ \t]+\n`)
)

// tidyPDFText shrinks the padding of -layout output, strips control
// characters (so a stray \x01 cannot be mistaken for a snippet marker) and
// caps the result on a rune boundary. The second result reports a cut.
func tidyPDFText(s string) (string, bool) {
	s = stripC0(strings.ToValidUTF8(s, ""))
	s = lineTail.ReplaceAllString(s, "\n")
	s = runSpaces.ReplaceAllString(s, "  ")
	s = runLines.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if len(s) > maxPDFStored {
		return strings.ToValidUTF8(s[:maxPDFStored], ""), true
	}
	return s, false
}

// PDFTextStatus is the progress of the pdf_text job.
type PDFTextStatus struct {
	Enabled bool `json:"enabled"`
	// Done counts PDF attachments whose outcome is recorded, Total all PDF
	// attachments in the index.
	Done     int  `json:"done"`
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
	// State is "disabled", "complete", "running" or "pending".
	State string `json:"state"`
	// Outcomes counts the distinct files by status (ok, timeout, ...).
	Outcomes map[string]int `json:"outcomes,omitempty"`
}

// PDFTextStatus reports the progress of the pdf_text job.
func (c *Cache) PDFTextStatus(ctx context.Context) (PDFTextStatus, error) {
	st := PDFTextStatus{Enabled: c.pdfExtractor() != nil}
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(text_extracted), 0) FROM attachments WHERE sha256 <> '' AND `+pdfCond).Scan(&st.Total, &st.Done)
	if err != nil {
		return st, fmt.Errorf("cache: pdf status: %w", err)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM pdf_text GROUP BY status`)
	if err != nil {
		return st, fmt.Errorf("cache: pdf status: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			s string
			n int
		)
		if err := rows.Scan(&s, &n); err != nil {
			return st, fmt.Errorf("cache: pdf status: %w", err)
		}
		if st.Outcomes == nil {
			st.Outcomes = map[string]int{}
		}
		st.Outcomes[s] = n
	}
	st.Complete = st.Done >= st.Total
	switch {
	case !st.Enabled:
		st.State = "disabled"
	default:
		st.State = jobStateName(st.Complete, &c.pdfJob)
	}
	return st, rows.Err()
}

// RunPDFText runs the job until ctx ends: a pass over every PDF attachment
// not yet done, then a rest, then another pass for what new mail brought. It
// returns at once when no extractor is set.
func (c *Cache) RunPDFText(ctx context.Context, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if c.pdfExtractor() == nil {
		return
	}
	// Files left by a crash mid-extraction; the sidecar needs none of them.
	_ = os.RemoveAll(c.pdfDir())
	for ctx.Err() == nil {
		wait := pdfRescanEvery
		stats, err := c.pdfPass(ctx, log)
		switch {
		case err == nil:
			if stats.Seen > 0 {
				log.Info("pdf_text pass done", "attachments", stats.Seen, "extracted", stats.Extracted, "cached", stats.Cached, "recorded_failed", stats.Failed)
			}
		case ctx.Err() != nil:
			return
		default:
			wait = pdfRetryAfter
			log.Warn("pdf_text paused", "error", err.Error(), "retry_in", wait.String())
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// pdfStats is what one pass did.
type pdfStats struct {
	Seen, Extracted, Cached, Failed int
}

type pdfAtt struct {
	rid               int64
	account, id, part string
	sha               string
	size              int64
}

// pdfPass processes every pending PDF attachment once. It returns an error
// (and stops) when the extractor is unavailable.
func (c *Cache) pdfPass(ctx context.Context, log *slog.Logger) (pdfStats, error) {
	var st pdfStats
	c.pdfJob.running.Store(true)
	defer c.pdfJob.running.Store(false)
	x := c.pdfExtractor()
	if x == nil {
		return st, nil
	}
	attempts := map[string]int{}
	var after int64
	for {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		batch, err := c.pendingPDFs(ctx, after)
		if err != nil {
			return st, err
		}
		if len(batch) == 0 {
			return st, nil
		}
		for _, a := range batch {
			after = a.rid
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			st.Seen++
			var held time.Duration
			err := c.retryBusy(ctx, log, "pdf_text", func() error {
				var err error
				held, err = c.pdfOne(ctx, x, a, attempts, &st)
				return err
			})
			if err != nil {
				return st, err
			}
			if !c.yield(ctx, held, backfillMinYield) {
				return st, ctx.Err()
			}
		}
	}
}

func (c *Cache) pendingPDFs(ctx context.Context, after int64) ([]pdfAtt, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT rowid, account, stable_id, part, sha256, size FROM attachments
WHERE text_extracted = 0 AND sha256 <> '' AND rowid > ? AND `+pdfCond+`
ORDER BY rowid LIMIT ?`, after, pdfBatch)
	if err != nil {
		return nil, fmt.Errorf("list pdf attachments: %w", err)
	}
	defer rows.Close()
	var out []pdfAtt
	for rows.Next() {
		var a pdfAtt
		if err := rows.Scan(&a.rid, &a.account, &a.id, &a.part, &a.sha, &a.size); err != nil {
			return nil, fmt.Errorf("list pdf attachments: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// pdfOne brings one attachment to done: from the cache by hash if the file
// was seen, else through the extractor. It returns the longest write hold.
func (c *Cache) pdfOne(ctx context.Context, x PDFExtractor, a pdfAtt, attempts map[string]int, st *pdfStats) (time.Duration, error) {
	res, cached, err := c.cachedPDF(ctx, a.sha)
	if err != nil {
		return 0, err
	}
	if cached {
		st.Cached++
	} else {
		res, err = c.extractPDF(ctx, x, a)
		if err != nil {
			if errors.Is(err, ErrPDFUnavailable) || ctx.Err() != nil {
				return 0, err
			}
			// The call itself failed (the sidecar died mid-job, say): a few
			// tries, then the file is recorded as failed, so a file that
			// kills the sidecar cannot loop forever.
			attempts[a.sha]++
			if attempts[a.sha] < pdfMaxAttempts {
				return 0, err
			}
			res = PDFResult{Status: "failed"}
		}
		if res.Status == "ok" {
			st.Extracted++
		} else {
			st.Failed++
		}
	}
	return c.storePDF(ctx, a.sha, res, !cached)
}

// cachedPDF looks the file up by hash.
func (c *Cache) cachedPDF(ctx context.Context, sha string) (PDFResult, bool, error) {
	var (
		r         PDFResult
		tr, pages int
	)
	err := c.db.QueryRowContext(ctx, `SELECT status, text, truncated, pages_capped FROM pdf_text WHERE sha256 = ?`, sha).Scan(&r.Status, &r.Text, &tr, &pages)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("read pdf text: %w", err)
	}
	r.Truncated, r.PagesCapped = tr == 1, pages == 1
	return r, true, nil
}

func (c *Cache) pdfDir() string { return filepath.Join(c.dir, "pdf") }

// pdfFile is where the sidecar looks for the file with the given hash.
func (c *Cache) pdfFile(sha string) string {
	if !sumRE.MatchString(sha) {
		return ""
	}
	return filepath.Join(c.pdfDir(), sha[:2], sha)
}

// extractPDF takes the attachment out of its message, hands the file to the
// extractor and removes it again. Outcomes that are the file's own (too big,
// missing, not what the index said) are results; only an extractor error is
// an error.
func (c *Cache) extractPDF(ctx context.Context, x PDFExtractor, a pdfAtt) (PDFResult, error) {
	if a.size > maxPDFInput {
		return PDFResult{Status: "too_large"}, nil
	}
	var blob string
	if err := c.db.QueryRowContext(ctx, `SELECT blob_sha256 FROM messages WHERE account = ? AND stable_id = ?`, a.account, a.id).Scan(&blob); err != nil {
		return PDFResult{Status: "failed"}, nil // the message is gone
	}
	path := c.BlobPath(blob)
	if path == "" {
		return PDFResult{Status: "failed"}, nil
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() > maxPDFBlob {
		if err == nil {
			return PDFResult{Status: "too_large"}, nil
		}
		return PDFResult{Status: "failed"}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return PDFResult{Status: "failed"}, nil
	}
	data, ok := partBody(raw, a.part, maxPDFInput)
	if !ok {
		return PDFResult{Status: "failed"}, nil
	}
	if len(data) > maxPDFInput {
		return PDFResult{Status: "too_large"}, nil
	}
	if h := sha256.Sum256(data); hex.EncodeToString(h[:]) != a.sha {
		return PDFResult{Status: "failed"}, nil
	}
	dst := c.pdfFile(a.sha)
	if err := writePDFFile(dst, data); err != nil {
		return PDFResult{}, fmt.Errorf("stage pdf: %w", err)
	}
	defer os.Remove(dst)
	r, err := x.Extract(ctx, a.sha)
	if err != nil {
		return PDFResult{}, err
	}
	return r, nil
}

// writePDFFile stores data at dst atomically, owner-readable only.
func writePDFFile(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o400); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// partBody returns the decoded body of the MIME part at path (numbered as in
// extractAttachments), reading at most limit+1 bytes. This is MIME, not PDF:
// the bytes are not interpreted.
func partBody(raw []byte, path string, limit int64) (out []byte, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		return nil, false
	}
	leaf := findPart(e, "", path, 0)
	if leaf == nil {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(leaf.Body, limit+1))
	if err != nil {
		return nil, false
	}
	return b, true
}

func findPart(e *message.Entity, path, want string, depth int) *message.Entity {
	if depth > maxMIMEDepth {
		return nil
	}
	if mr := e.MultipartReader(); mr != nil {
		for i := 1; ; i++ {
			p, err := mr.NextPart()
			if err != nil && (p == nil || errors.Is(err, io.EOF) || !message.IsUnknownCharset(err)) {
				return nil
			}
			child := strconv.Itoa(i)
			if path != "" {
				child = path + "." + child
			}
			if got := findPart(p, child, want, depth+1); got != nil {
				return got
			}
		}
	}
	if path == "" {
		path = "1"
	}
	if path == want {
		return e
	}
	return nil
}

// storePDF records the outcome for hash sha and marks the attachments that
// carry it done, indexing the text of an ok result. Writes are short
// transactions: at most writeRows attachments each. It returns the longest
// hold.
func (c *Cache) storePDF(ctx context.Context, sha string, r PDFResult, record bool) (time.Duration, error) {
	text := ""
	trunc := r.Truncated
	if r.Status == "ok" {
		var cut bool
		text, cut = tidyPDFText(r.Text)
		trunc = trunc || cut
	}
	var longest time.Duration
	for first := true; ; first = false {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return longest, fmt.Errorf("begin: %w", err)
		}
		began := time.Now()
		if first && record {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO pdf_text (sha256, status, text, truncated, pages_capped, updated_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (sha256) DO NOTHING`, sha, r.Status, text, b2i(trunc), b2i(r.PagesCapped), c.now().Unix()); err != nil {
				_ = tx.Rollback()
				return longest, fmt.Errorf("record pdf text: %w", err)
			}
		}
		n, err := applyPDFTx(ctx, tx, sha, text)
		if err != nil {
			_ = tx.Rollback()
			return longest, err
		}
		held, err := c.commitHeld(tx, began)
		if err != nil {
			return longest, err
		}
		longest = max(longest, held)
		if n < writeRows {
			return longest, nil
		}
		if !c.yield(ctx, held, backfillMinYield) {
			return longest, ctx.Err()
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// applyPDFTx marks up to writeRows pending PDF attachments with hash sha done
// and puts text into their attachment_fts rows, whose rowid is the
// attachments rowid. A part without a file name has no row yet and gets one.
// It returns how many attachments it handled.
func applyPDFTx(ctx context.Context, tx *sql.Tx, sha, text string) (int, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT rowid, account, stable_id, part, filename FROM attachments
WHERE sha256 = ? AND text_extracted = 0 AND `+pdfCond+` LIMIT ?`, sha, writeRows)
	if err != nil {
		return 0, fmt.Errorf("list pdf attachments: %w", err)
	}
	type ref struct {
		rid                  int64
		account, id, part, f string
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.rid, &r.account, &r.id, &r.part, &r.f); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("list pdf attachments: %w", err)
		}
		refs = append(refs, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, fmt.Errorf("list pdf attachments: %w", err)
	}
	for _, r := range refs {
		if text != "" {
			res, err := tx.ExecContext(ctx, `UPDATE attachment_fts SET text = ? WHERE rowid = ?`, text, r.rid)
			if err != nil {
				return 0, fmt.Errorf("index pdf text: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO attachment_fts (rowid, account, stable_id, part, filename, text) VALUES (?, ?, ?, ?, ?, ?)`,
					r.rid, r.account, r.id, r.part, r.f, text); err != nil {
					return 0, fmt.Errorf("index pdf text: %w", err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attachments SET text_extracted = 1 WHERE rowid = ?`, r.rid); err != nil {
			return 0, fmt.Errorf("mark pdf done: %w", err)
		}
	}
	return len(refs), nil
}

// AttachmentText is the extracted text of one attachment.
type AttachmentText struct {
	Part      string
	Filename  string
	Text      string
	Truncated bool
}

// AttachmentTexts returns the extracted PDF text of a message's attachments,
// at most perAtt bytes each and total bytes overall, cut on rune boundaries
// (Truncated says so). Attachments without text are left out.
func (c *Cache) AttachmentTexts(ctx context.Context, account, stableID string, perAtt, total int) ([]AttachmentText, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT a.part, a.filename, p.text, p.truncated FROM attachments a JOIN pdf_text p ON p.sha256 = a.sha256
WHERE a.account = ? AND a.stable_id = ? AND p.status = 'ok' AND p.text <> '' ORDER BY a.rowid`, account, stableID)
	if err != nil {
		return nil, fmt.Errorf("cache: attachment text: %w", err)
	}
	defer rows.Close()
	var out []AttachmentText
	left := total
	for rows.Next() {
		var (
			t  AttachmentText
			tr int
		)
		if err := rows.Scan(&t.Part, &t.Filename, &t.Text, &tr); err != nil {
			return nil, fmt.Errorf("cache: attachment text: %w", err)
		}
		t.Truncated = tr == 1
		limit := min(perAtt, left)
		if limit <= 0 {
			break
		}
		if len(t.Text) > limit {
			t.Text, t.Truncated = strings.ToValidUTF8(t.Text[:limit], ""), true
		}
		left -= len(t.Text)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RunPDFTextOnce makes one pass over the pending PDF attachments and returns.
// It is RunPDFText without the loop, for callers (and tests) that want to
// drive the schedule themselves.
func (c *Cache) RunPDFTextOnce(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	_, err := c.pdfPass(ctx, log)
	return err
}
