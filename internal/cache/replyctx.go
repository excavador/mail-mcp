package cache

// What a reply needs from the message it answers. Like ReadMessage it reads
// the cached blob and nothing else; unlike it, it keeps the addresses parsed
// and the threading headers, which the read tools do not show.

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// Addr is one parsed address of a header.
type Addr struct {
	Name    string
	Address string
}

// maxReplyAddrs bounds each address list of a ReplyContext: a hostile message
// with thousands of recipients must not become thousands of reply recipients.
const maxReplyAddrs = 200

// ReplyContext is the part of a cached message a reply is built from. Every
// string in it was written by the sender (or whoever forged the sender).
type ReplyContext struct {
	StableID   string
	MessageID  string // as in the header, with its angle brackets
	InReplyTo  string
	References string
	Subject    string
	Date       time.Time // zero when the header is missing or unparseable

	From, ReplyTo, To, Cc []Addr
	// DeliveredTo holds Delivered-To and X-Original-To: the mailbox the
	// message was finally delivered to, which may be an alias.
	DeliveredTo []string

	// Body is the plain text (else HTML reduced to text), cut to the maxBody
	// given to ReadReplyContext on a UTF-8 boundary.
	Body      string
	Truncated bool
}

func addrList(h gomail.Header, key string) []Addr {
	l, err := h.AddressList(key)
	if err != nil {
		return nil
	}
	if len(l) > maxReplyAddrs {
		l = l[:maxReplyAddrs]
	}
	out := make([]Addr, 0, len(l))
	for _, a := range l {
		out = append(out, Addr{Name: a.Name, Address: a.Address})
	}
	return out
}

// hiddenEl matches a simple (non-nested) element that is hidden from the
// reader: display:none, visibility:hidden, font-size:0 or the hidden attribute.
// Best effort: nested same-name elements and CSS classes are not understood.
var hiddenEls = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, tag := range []string{"span", "div", "p", "td", "font", "b", "i", "u", "a", "li", "em", "strong"} {
		out = append(out, regexp.MustCompile(`(?is)<`+tag+`\b[^>]*(display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0(px|pt|em|%)?\s*([;"']|$)|\shidden\b)[^>]*>.*?</`+tag+`\s*>`))
	}
	return out
}()

func dropHidden(s string) string {
	for _, re := range hiddenEls {
		s = re.ReplaceAllString(s, " ")
	}
	return s
}

// ReadReplyContext loads the reply context of one cached message. A message
// that cannot be parsed is returned with no headers and no body, as
// ReadMessage returns it: the caller decides whether that is enough.
func (c *Cache) ReadReplyContext(ctx context.Context, account, stableID string, maxBody int) (*ReplyContext, error) {
	raw, err := c.readBlob(ctx, account, stableID)
	if err != nil {
		return nil, err
	}
	rc := &ReplyContext{StableID: stableID}
	e, perr := message.Read(bytes.NewReader(raw))
	if e == nil || (perr != nil && !message.IsUnknownCharset(perr) && !message.IsUnknownEncoding(perr)) {
		return rc, nil
	}
	h := gomail.Header{Header: e.Header}
	rc.From, rc.ReplyTo, rc.To, rc.Cc = addrList(h, "From"), addrList(h, "Reply-To"), addrList(h, "To"), addrList(h, "Cc")
	rc.Subject, _ = h.Subject()
	if rc.Subject == "" {
		rc.Subject = e.Header.Get("Subject")
	}
	if d, err := h.Date(); err == nil {
		rc.Date = d
	}
	rc.MessageID = strings.TrimSpace(e.Header.Get("Message-Id"))
	rc.InReplyTo = strings.TrimSpace(e.Header.Get("In-Reply-To"))
	rc.References = strings.Join(strings.Fields(e.Header.Get("References")), " ")
	for _, k := range []string{"Delivered-To", "X-Original-To"} {
		f := e.Header.FieldsByKey(k)
		for n := 0; f.Next() && n < 8; n++ {
			if v := strings.TrimSpace(f.Value()); v != "" {
				rc.DeliveredTo = append(rc.DeliveredTo, v)
			}
		}
	}

	var plain, htm strings.Builder
	collectText(e, &plain, &htm, 0)
	body := strings.TrimSpace(plain.String())
	if body == "" {
		body = stripHTML(dropHidden(htm.String()))
	}
	if maxBody > 0 && len(body) > maxBody {
		body = strings.ToValidUTF8(body[:maxBody], "")
		rc.Truncated = true
	}
	rc.Body = body
	return rc, nil
}

// ErrAmbiguousDrafts means more than one folder carries \Drafts.
var ErrAmbiguousDrafts = errors.New("ambiguous Drafts folder: more than one folder is marked \\Drafts")

// DraftsFolder names the account's Drafts folder from what the last refresh
// saw of LIST: the one folder carrying the \Drafts special-use attribute, else
// a folder named exactly "Drafts". Folders that cannot be selected or do not
// exist are skipped. ok is false when there is none; more than one \Drafts
// folder is ErrAmbiguousDrafts.
func (c *Cache) DraftsFolder(ctx context.Context, account string) (name string, ok bool, err error) {
	rows, err := c.db.QueryContext(ctx, `SELECT folder, attrs FROM folders WHERE account = ? ORDER BY folder`, account)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var byName bool
	var marked []string
	for rows.Next() {
		var f, attrs string
		if err := rows.Scan(&f, &attrs); err != nil {
			return "", false, err
		}
		pad := " " + strings.ToLower(attrs) + " "
		if strings.Contains(pad, ` \noselect `) || strings.Contains(pad, ` \nonexistent `) {
			continue
		}
		if strings.Contains(pad, ` \drafts `) {
			marked = append(marked, f)
		} else if f == "Drafts" {
			byName = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	switch {
	case len(marked) > 1:
		return "", false, ErrAmbiguousDrafts
	case len(marked) == 1:
		return marked[0], true, nil
	case byName:
		return "Drafts", true, nil
	}
	return "", false, nil
}
