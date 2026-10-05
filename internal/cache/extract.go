package cache

import (
	"bufio"
	"bytes"
	"errors"
	"html"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode legacy charsets in bodies and headers
	"github.com/emersion/go-message/mail"
)

// maxIndexedText caps the body text indexed per message. A newsletter or a
// mailing-list digest can be megabytes; the first quarter megabyte holds
// anything a person would search for, and bounds the FTS index.
const maxIndexedText = 256 << 10

// maxHeaderField caps each indexed header column, so one hostile header
// (a megabyte of Cc) cannot bloat the index.
const maxHeaderField = 4 << 10

// capField truncates s to maxHeaderField bytes on a UTF-8 boundary.
func capField(s string) string {
	if len(s) <= maxHeaderField {
		return s
	}
	return strings.ToValidUTF8(s[:maxHeaderField], "")
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }

// parsed is what the index keeps about one message.
type parsed struct {
	From, To, Cc, Subject string
	Date                  time.Time
	ListID, GitHubReason  string
	Body                  string // whole text, quotes included (message_fts)
	BodyNew               string // CleanBody(Body) (message_fts2.body_new)
	Atts                  []AttachmentMeta
}

// parseMessage extracts headers and searchable text from raw RFC 822. It is
// forgiving: a message that does not parse cleanly is still cached as a blob,
// and gets whatever headers could be read, because losing the message from
// search is worse than indexing it thinly.
//
// A panic in the MIME or charset code must not take the process down (the blob
// is already stored, so a poison message would crash-loop the pod): it is
// recovered, and what was parsed so far (at least nothing, at most the
// headers) is returned.
func parseMessage(raw []byte) (out parsed) {
	defer func() {
		if recover() != nil {
			out.Atts = nil
		}
	}()
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		return out
	}
	h := mail.Header{Header: e.Header}
	out.From = capField(stripC0(addrs(h, "From")))
	out.To = capField(stripC0(addrs(h, "To")))
	out.Cc = capField(stripC0(addrs(h, "Cc")))
	out.Subject, _ = h.Subject()
	if out.Subject == "" {
		out.Subject = e.Header.Get("Subject")
	}
	out.Subject = capField(stripC0(out.Subject))
	if d, err := h.Date(); err == nil {
		out.Date = d
	}
	out.ListID = capField(strings.TrimSpace(e.Header.Get("List-Id")))
	out.GitHubReason = capField(strings.TrimSpace(e.Header.Get("X-Github-Reason")))

	var plain, htm strings.Builder
	collectText(e, &plain, &htm, 0)
	body := strings.TrimSpace(plain.String())
	if body == "" {
		body = stripHTML(htm.String())
	}
	if len(body) > maxIndexedText {
		body = strings.ToValidUTF8(body[:maxIndexedText], "")
	}
	body = stripC0(body)
	out.Body = body
	out.BodyNew = CleanBody(body)
	out.Atts = extractAttachments(raw)
	return out
}

func addrs(h mail.Header, key string) string {
	l, err := h.AddressList(key)
	if err != nil || len(l) == 0 {
		return strings.TrimSpace(h.Get(key))
	}
	parts := make([]string, 0, len(l))
	for _, a := range l {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}

// collectText walks the MIME tree gathering text/plain and text/html bodies,
// skipping attachments. Plain text is preferred by the caller; HTML is only
// the fallback for messages that carry no plain part.
func collectText(e *message.Entity, plain, htm *strings.Builder, depth int) {
	if depth > 16 || plain.Len() >= maxIndexedText {
		return
	}
	if mr := e.MultipartReader(); mr != nil {
		walkParts(mr, plain, htm, depth)
		return
	}
	if disp, _, _ := e.Header.ContentDisposition(); disp == "attachment" {
		return
	}
	mt, _, err := e.Header.ContentType()
	if err != nil {
		mt = "text/plain"
	}
	var dst *strings.Builder
	switch mt {
	case "text/plain":
		dst = plain
	case "text/html":
		dst = htm
	default:
		return
	}
	if dst.Len() >= maxIndexedText {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(e.Body, int64(maxIndexedText-dst.Len())))
	if dst.Len() > 0 {
		dst.WriteByte('\n')
	}
	dst.WriteString(strings.ToValidUTF8(string(b), ""))
}

func walkParts(mr message.MultipartReader, plain, htm *strings.Builder, depth int) {
	for {
		p, err := mr.NextPart()
		if err != nil && (p == nil || errors.Is(err, io.EOF) || !message.IsUnknownCharset(err)) {
			return
		}
		collectText(p, plain, htm, depth+1)
	}
}

var (
	breaks     = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6])\b[^>]*>`)
	dropBlocks = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)\s*>`)
	inline     = regexp.MustCompile(`(?i)</?(a|b|i|em|strong|span|u|s|small|big|code|font|sub|sup|mark|abbr)(\s[^>]*)?/?>`)
	tags       = regexp.MustCompile(`(?s)<[^>]*>`)
	spaces     = regexp.MustCompile(`[ \t\r\f\v]+`)
	blank      = regexp.MustCompile(`\n\s*\n+`)
)

// stripHTML reduces an HTML body to its text: scripts and styles dropped,
// tags removed, entities decoded, whitespace collapsed. Good enough for a
// full-text index; it is not an HTML renderer.
func stripHTML(s string) string {
	s = dropBlocks.ReplaceAllString(s, " ")
	s = breaks.ReplaceAllString(s, "\n")
	// Inline tags vanish without a gap so "<b>wor</b>ld" stays one word;
	// every other tag separates words.
	s = inline.ReplaceAllString(s, "")
	s = tags.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = spaces.ReplaceAllString(s, " ")
	s = blank.ReplaceAllString(s, "\n")
	return strings.TrimSpace(s)
}

// stripC0 removes C0 control characters (and DEL) other than tab and newline
// from text headed for the index, so a stray \x01 or \x02 in a message cannot
// be mistaken for the snippet markers search uses.
func stripC0(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
