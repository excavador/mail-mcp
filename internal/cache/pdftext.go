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

// ErrPDFHelperUnavailable is ErrPDFUnavailable as reported by the sidecar
// itself (its answer was "unavailable"), as opposed to a breaker that is open
// or a staging problem. It is counted per file: a file that gets it three
// times is recorded as failed, so one file that makes the helper look sick
// cannot stay first in line forever.
var ErrPDFHelperUnavailable = fmt.Errorf("%w: the extractor reported itself unavailable", ErrPDFUnavailable)

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
	// maxPDFStored caps the text kept per file, in pdf_text and in the index:
	// the same bound as indexed body text.
	maxPDFStored = maxIndexedText

	pdfBatch       = 20
	pdfRetryAfter  = 2 * time.Minute  // after the extractor was unavailable
	pdfRescanEvery = 10 * time.Minute // new mail may carry new PDFs
	pdfMaxAttempts = 3                // transport failures on one file before it is recorded as failed

	// pdfPending is the provisional status written before a file is staged.
	pdfPending = "pending"
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
	rows, err := c.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM pdf_text WHERE status <> 'pending' GROUP BY status`)
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
	_ = os.RemoveAll(c.pdfDir()) // a subdirectory of the staging root, never the mount point
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
			held, err := c.pdfOne(ctx, log, x, a, &st)
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
//
// Transport failures are counted per file across passes (c.pdfAttempts); the
// third records the file as failed, so a file that kills the sidecar cannot
// block the files behind it. ErrPDFUnavailable is not the file's fault and is
// not counted. A "pending" row is written before the risky steps; finding one
// again means the process stopped mid-file (a crash, or a restart), which is
// one strike, and the file becomes failed only at the third. Only the final
// write is retried on SQLITE_BUSY, never the extraction.
func (c *Cache) pdfOne(ctx context.Context, log *slog.Logger, x PDFExtractor, a pdfAtt, st *pdfStats) (time.Duration, error) {
	res, strikes, cached, err := c.cachedPDF(ctx, a.sha)
	if err != nil {
		return 0, err
	}
	record := !cached
	if cached && res.Status == pdfPending {
		strikes++
		if strikes >= pdfMaxAttempts {
			res, record = PDFResult{Status: "failed"}, true
			st.Failed++
			c.clearAttempts(a.sha)
			return c.storeRetry(ctx, log, a.sha, res, record)
		}
		if err := c.retryBusy(ctx, log, "pdf_text", func() error {
			_, e := c.db.ExecContext(ctx, `UPDATE pdf_text SET strikes = ? WHERE sha256 = ? AND status = ?`, strikes, a.sha, pdfPending)
			return e
		}); err != nil {
			return 0, err
		}
		cached, record = false, true
	}
	if cached {
		st.Cached++
	} else {
		res, err = c.extractPDF(ctx, log, x, a)
		if err != nil {
			if ctx.Err() != nil || (errors.Is(err, ErrPDFUnavailable) && !errors.Is(err, ErrPDFHelperUnavailable)) {
				return 0, err
			}
			if c.noteAttempt(a.sha) < pdfMaxAttempts {
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
	c.clearAttempts(a.sha)
	held, err := c.storeRetry(ctx, log, a.sha, res, record)
	if err != nil {
		// The extraction is not lost for good: drop the provisional row so the
		// next pass starts clean instead of counting a strike.
		c.dropPDFPending(log, a.sha)
	}
	return held, err
}

func (c *Cache) storeRetry(ctx context.Context, log *slog.Logger, sha string, res PDFResult, record bool) (held time.Duration, err error) {
	err = c.retryBusy(ctx, log, "pdf_text", func() error {
		var e error
		held, e = c.storePDF(ctx, sha, res, record)
		return e
	})
	return held, err
}

func (c *Cache) noteAttempt(sha string) int {
	c.pdfMu.Lock()
	defer c.pdfMu.Unlock()
	if c.pdfAttempts == nil {
		c.pdfAttempts = map[string]int{}
	}
	c.pdfAttempts[sha]++
	return c.pdfAttempts[sha]
}

func (c *Cache) clearAttempts(sha string) {
	c.pdfMu.Lock()
	delete(c.pdfAttempts, sha)
	c.pdfMu.Unlock()
}

// cachedPDF looks the file up by hash.
func (c *Cache) cachedPDF(ctx context.Context, sha string) (r PDFResult, strikes int, found bool, err error) {
	var tr, pages int
	err = c.db.QueryRowContext(ctx, `SELECT status, text, truncated, pages_capped, strikes FROM pdf_text WHERE sha256 = ?`, sha).Scan(&r.Status, &r.Text, &tr, &pages, &strikes)
	if errors.Is(err, sql.ErrNoRows) {
		return r, 0, false, nil
	}
	if err != nil {
		return r, 0, false, fmt.Errorf("read pdf text: %w", err)
	}
	r.Truncated, r.PagesCapped = tr == 1, pages == 1
	return r, strikes, true, nil
}

// SetPDFStageDir sets the root under which decoded PDFs are staged for the
// sidecar, as <root>/pdf/<hh>/<sha> (default root: the cache dir). In a pod it
// is a dedicated emptyDir, read-write here and read-only in the sidecar (its
// --root), so the sidecar never sees the mail store.
func (c *Cache) SetPDFStageDir(dir string) {
	c.pdfMu.Lock()
	c.pdfStage = dir
	c.pdfMu.Unlock()
}

func (c *Cache) pdfDir() string {
	c.pdfMu.RLock()
	defer c.pdfMu.RUnlock()
	root := c.pdfStage
	if root == "" {
		root = c.dir
	}
	return filepath.Join(root, "pdf")
}

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
//
// Memory stays small: the message blob is streamed through the MIME reader
// and the part is copied to the staging file through a hash, never held whole.
// A provisional "pending" outcome is written before any of that, so a crash
// (an OOM kill, say) cannot make the same file loop forever.
func (c *Cache) extractPDF(ctx context.Context, log *slog.Logger, x PDFExtractor, a pdfAtt) (res PDFResult, err error) {
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
	if _, serr := os.Stat(path); serr != nil {
		return PDFResult{Status: "failed"}, nil
	}
	// No cap on the blob size: it is streamed, and the copy of the part is
	// bounded by maxPDFInput and checked against the recorded hash.
	if err := c.retryBusy(ctx, log, "pdf_text", func() error { return c.markPDFPending(ctx, a.sha) }); err != nil {
		return PDFResult{}, err
	}
	defer func() {
		// Any error return (including a shutdown) is "try again later", not
		// an outcome, and must not leave a row that counts as a strike.
		if err != nil {
			c.dropPDFPending(log, a.sha)
		}
	}()
	dst := c.pdfFile(a.sha)
	status, serr := c.stagePart(path, a, dst)
	if serr != nil {
		// Writing the staging file failed (a full or broken emptyDir): the
		// environment's problem, not the file's. Retryable, and not a strike.
		return PDFResult{}, fmt.Errorf("stage pdf: %w", errors.Join(ErrPDFUnavailable, serr))
	}
	if status != "" {
		return PDFResult{Status: status}, nil
	}
	defer os.Remove(dst)
	return x.Extract(ctx, a.sha)
}

func (c *Cache) markPDFPending(ctx context.Context, sha string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO pdf_text (sha256, status, updated_at) VALUES (?, ?, ?) ON CONFLICT (sha256) DO NOTHING`, sha, pdfPending, c.now().Unix())
	if err != nil {
		return fmt.Errorf("mark pdf pending: %w", err)
	}
	return nil
}

// dropPDFPending removes the provisional row of sha, retrying on SQLITE_BUSY
// for a few seconds and on a context of its own, so it still runs at shutdown.
func (c *Cache) dropPDFPending(log *slog.Logger, sha string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.retryBusy(ctx, log, "pdf_text", func() error {
		_, err := c.db.ExecContext(ctx, `DELETE FROM pdf_text WHERE sha256 = ? AND status = ?`, sha, pdfPending)
		return err
	})
}

// stagePart copies the decoded MIME part a.part of the message blob at path
// to dst (atomically, owner-readable only), through a hash. It returns a
// non-empty status when the file is the file's own problem (too large,
// missing, not the bytes the index recorded) and an error only for staging
// failures.
func (c *Cache) stagePart(path string, a pdfAtt, dst string) (status string, err error) {
	defer func() {
		if recover() != nil {
			status, err = "failed", nil // a parser panic in MIME code is the file's problem
		}
	}()
	f, oerr := os.Open(path)
	if oerr != nil {
		return "failed", nil
	}
	defer f.Close()
	e, rerr := message.Read(f)
	if e == nil || (rerr != nil && !message.IsUnknownCharset(rerr) && !message.IsUnknownEncoding(rerr)) {
		return "failed", nil
	}
	leaf := findPart(e, "", a.part, 0)
	if leaf == nil {
		return "failed", nil
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(e error) (string, error) { _ = tmp.Close(); _ = os.Remove(name); return "", e }
	h := sha256.New()
	ew := &errWriter{w: io.MultiWriter(tmp, h)}
	n, cerr := io.Copy(ew, io.LimitReader(leaf.Body, maxPDFInput+1))
	if ew.err != nil { // the write side: ENOSPC, EIO
		return fail(ew.err)
	}
	if cerr != nil { // the read side: the part does not decode
		_ = tmp.Close()
		_ = os.Remove(name)
		return "failed", nil
	}
	if n > maxPDFInput {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "too_large", nil
	}
	if hex.EncodeToString(h.Sum(nil)) != a.sha {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "failed", nil
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Chmod(name, 0o400); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return "", nil
}

// errWriter remembers a write error, so a failure of the destination can be
// told from a failure to decode the source.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

func findPart(e *message.Entity, path, want string, depth int) *message.Entity {
	if depth > maxMIMEDepth {
		return nil
	}
	if mr := e.MultipartReader(); mr != nil {
		for i := 1; i <= maxAttachments; i++ {
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
		return nil
	}
	if path == "" {
		path = "1"
	}
	if path == want {
		return e
	}
	return nil
}

// pdfStoreHook, when set (tests), runs at the start of every storePDF try.
var pdfStoreHook func() error

// storePDF records the outcome for hash sha and marks the attachments that
// carry it done, indexing the text of an ok result. Writes are short
// transactions: at most writeRows attachments each. It returns the longest
// hold.
func (c *Cache) storePDF(ctx context.Context, sha string, r PDFResult, record bool) (time.Duration, error) {
	if pdfStoreHook != nil {
		if err := pdfStoreHook(); err != nil {
			return 0, err
		}
	}
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
ON CONFLICT (sha256) DO UPDATE SET status = excluded.status, text = excluded.text, truncated = excluded.truncated,
	pages_capped = excluded.pages_capped, updated_at = excluded.updated_at WHERE pdf_text.status = 'pending'`, sha, r.Status, text, b2i(trunc), b2i(r.PagesCapped), c.now().Unix()); err != nil {
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
