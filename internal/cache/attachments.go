package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
)

const (
	maxAttachmentsN = maxAttachments // per message, same bound as the listing
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

}

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.Reader}

func decodeName(s string) string {
	if strings.Contains(s, "=?") {
		if d, err := wordDecoder.DecodeHeader(s); err == nil {
			s = d
		}
	}
	return capField(stripC0(strings.ToValidUTF8(s, "")))
}

// extractAttachments walks the MIME tree of raw and reports every attachment
// and inline part: anything marked attachment, carrying a filename or a
// Content-ID, or not text. The plain and HTML body parts themselves are not
// attachments. Each part is streamed through a hash and never buffered, and a
// bounded number of parts is recorded, so a hostile tree cannot flood the
// table. A parser panic yields whatever was collected so far.
//
// PDF text is deliberately NOT extracted: parsing untrusted PDFs in-process
// proved unsafe (decompression bombs, page-tree loops). A follow-up will do it
// in a killable subprocess with RLIMIT_AS.
func extractAttachments(raw []byte) (out []AttachmentMeta) {
	defer func() {
		if recover() != nil {
			// keep what was gathered
		}
	}()
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		return nil
	}
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
	if name == "" {
		name = rfc2231Filename(e.Header.Get("Content-Disposition"))
	}
	if name == "" {
		name = rfc2231Filename(e.Header.Get("Content-Type"))
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
	n, _ := io.Copy(h, e.Body)
	m.Size = n
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	*out = append(*out, m)
}

var (
	paramRE = regexp.MustCompile(`(?i)(?:^|;)\s*(?:filename|name)(?:\*(\d+))?(\*)?\s*=\s*("(?:[^"\\]|\\.)*"|[^;\s]*)`)
)

// rfc2231Filename recovers a filename that mime.ParseMediaType dropped:
// RFC 2231 values (filename*=ISO-8859-1”Caf%E9.pdf, also continued as
// filename*0*=..., filename*1*=...) in a charset other than UTF-8/ASCII. The
// bytes are percent-decoded and converted with go-message's charset table;
// when the charset is unknown the raw bytes are made valid UTF-8, so a name is
// never lost. It returns "" only when the header carries no such parameter.
func rfc2231Filename(hdr string) string {
	type seg struct {
		idx int
		val string
		enc bool
	}
	var segs []seg
	cs := ""
	for _, m := range paramRE.FindAllStringSubmatch(hdr, -1) {
		if m[2] != "*" && m[1] == "" {
			continue // a plain parameter: ParseMediaType already handled it
		}
		idx := 0
		if m[1] != "" {
			idx, _ = strconv.Atoi(m[1])
		}
		v := strings.Trim(m[3], `"`)
		enc := m[2] == "*"
		if enc && (idx == 0) {
			// charset'language'value
			parts := strings.SplitN(v, "'", 3)
			if len(parts) == 3 {
				cs, v = parts[0], parts[2]
			}
		}
		segs = append(segs, seg{idx, v, enc})
	}
	if len(segs) == 0 {
		return ""
	}
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].idx < segs[j].idx })
	var raw []byte
	for _, s := range segs {
		if !s.enc {
			raw = append(raw, s.val...)
			continue
		}
		if d, err := url.PathUnescape(s.val); err == nil {
			raw = append(raw, d...)
		} else {
			raw = append(raw, s.val...)
		}
	}
	if cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		if r, err := charset.Reader(cs, bytes.NewReader(raw)); err == nil {
			if b, err := io.ReadAll(io.LimitReader(r, 4<<10)); err == nil {
				raw = b
			}
		}
	}
	return string(raw)
}

// insertAttachmentsTx records atts for the message (account, stableID) inside
// tx. attachments carries one row per part. attachment_fts carries one row per
// part that has a filename, its rowid equal to the attachments rowid so a hit
// resolves to its part without a scan; the text column stays empty (no PDF
// text for now) and the filename is what is searchable. Plain INSERT with
// ON CONFLICT DO NOTHING: a part that is already there is left alone and gets
// no second FTS row, so the two rowids cannot drift apart.
func insertAttachmentsTx(ctx context.Context, tx *sql.Tx, account, stableID string, atts []AttachmentMeta) error {
	for _, a := range atts {
		inline := 0
		if a.Inline {
			inline = 1
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO attachments (account, stable_id, part, filename, mime, size, sha256, is_inline, content_id, text_extracted)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
ON CONFLICT (account, stable_id, part) DO NOTHING`,
			account, stableID, a.Part, a.Filename, a.Mime, a.Size, a.SHA256, inline, a.ContentID)
		if err != nil {
			return fmt.Errorf("index attachment: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 || a.Filename == "" {
			continue
		}
		rid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("index attachment: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attachment_fts (rowid, account, stable_id, part, filename, text) VALUES (?, ?, ?, ?, ?, '')`,
			rid, account, stableID, a.Part, a.Filename); err != nil {
			return fmt.Errorf("index attachment name: %w", err)
		}
	}
	return nil
}
