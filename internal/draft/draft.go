// Package draft renders a draft message: pure text in, RFC 5322 bytes out.
//
// It knows nothing of IMAP or of the cache. A draft is written into the
// account's Drafts folder by the caller and sent, if ever, by the owner from
// their own mail client; nothing here (or anywhere in mail-mcp) sends mail.
//
// The original message of a reply is untrusted. It is only ever quoted as
// text, after control characters are removed, and its addresses are checked
// the same way as the addresses a caller types.
package draft

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/excavador/mail-mcp/internal/accounts"
)

const (
	// MaxBodyBytes caps the body the caller writes.
	MaxBodyBytes = 100 << 10
	// MaxQuoteBytes caps the quoted part of the original.
	MaxQuoteBytes = 20 << 10
	// MaxRecipients caps To, Cc and Bcc together.
	MaxRecipients = 50
	// MaxSubjectRunes caps the subject.
	MaxSubjectRunes = 300
	// maxReferences is how many Message-IDs References keeps: the first (the
	// root of the thread) and the most recent ones.
	maxReferences = 20
	// maxIDLen caps one Message-ID token; maxHeaderLine caps a header line
	// (RFC 5322 section 2.1.1).
	maxIDLen      = 255
	maxHeaderLine = 998
	maxNameRunes  = 100
)

// Original is the message a reply answers, as the cache holds it. Every field
// is third-party content.
type Original struct {
	StableID    string
	MessageID   string // with angle brackets
	References  string
	Subject     string
	Date        time.Time
	From        []Addr
	ReplyTo     []Addr
	To, Cc      []Addr
	DeliveredTo []string
	Body        string
}

// Addr is one address of the original.
type Addr struct{ Name, Address string }

// Input is everything Compose renders a message from.
type Input struct {
	Account accounts.Account

	// Original is set for a reply. ReplyAll widens the recipients to everyone
	// else the original went to.
	Original *Original
	ReplyAll bool
	// QuoteOriginal appends "On <date>, <from> wrote:" and the quoted text.
	QuoteOriginal bool

	// To, Cc and Bcc are addresses ("a@b" or "Name <a@b>"). For a reply, a
	// non-empty To replaces the computed To; Cc and Bcc are added to it.
	To, Cc, Bcc []string
	Subject     string // empty: "Re: <original subject>" for a reply
	Body        string // UTF-8 plain text
	From        string // empty: chosen, see ChooseFrom

	Now  time.Time // zero: time.Now()
	Rand io.Reader // nil: crypto/rand
}

// Message is a rendered draft.
type Message struct {
	// Raw is exactly what will be appended to the Drafts folder.
	Raw []byte
	// FromAddr is the bare sender address; From is the header value.
	FromAddr, From string
	// To, Cc and Bcc are the header values, one address each.
	To, Cc, Bcc []string
	Subject     string
	MessageID   string
	InReplyTo   string
	// Body is the plain text of the message (the typed text plus any quote).
	Body string
	// ToAddrs, CcAddrs and BccAddrs are the bare, lowercase addresses.
	ToAddrs, CcAddrs, BccAddrs []string
	// ReplyToRedirect: the recipients come from a Reply-To that names someone
	// other than the original's sender.
	ReplyToRedirect bool
}

// Error is a refusal whose text is safe to show the caller.
type Error string

func (e Error) Error() string { return string(e) }

func refusef(f string, a ...any) error { return Error(fmt.Sprintf(f, a...)) }

// headerSafe reports whether s may stand in a header value: no CR, LF, NUL
// or other control character (a tab is allowed).
func headerSafe(s string) bool {
	for _, r := range s {
		if r == '\t' {
			continue
		}
		if r < ' ' || r == 0x7f || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// cleanName removes control characters from a display name taken from the
// original message, so a hostile name cannot break a header or the quote line.
func cleanName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// parseAddr checks one address typed by the caller.
func parseAddr(field, raw string) (*mail.Address, error) {
	if !headerSafe(raw) || strings.ContainsRune(raw, '\t') {
		return nil, refusef("%s: %q contains a control character", field, trunc(raw))
	}
	a, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return nil, refusef("%s: %q is not a valid address", field, trunc(raw))
	}
	return checkAddr(field, a)
}

func checkAddr(field string, a *mail.Address) (*mail.Address, error) {
	if !validAddrSpec(a.Address) {
		return nil, refusef("%s: %q is not a plain ASCII address (name@domain)", field, trunc(a.Address))
	}
	a.Name = cleanName(a.Name)
	if r := []rune(a.Name); len(r) > maxNameRunes {
		a.Name = string(r[:maxNameRunes])
	}
	return a, nil
}

// addrSpecRE is a deliberately plain subset: net/mail has accepted it, so this
// only rules out what a header or a mail server would choke on.
var addrSpecRE = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

func validAddrSpec(s string) bool { return len(s) <= 254 && addrSpecRE.MatchString(s) }

func trunc(s string) string {
	if utf8.RuneCountInString(s) > 80 {
		return string([]rune(s)[:80]) + "..."
	}
	return s
}

// ChooseFrom picks the sender address.
//
// With a requested address, it must be one the owner owns on the account (the
// username, an alias, or any address at an "@domain" alias); anything else is
// refused. Without one, the address the original was sent to wins (in To, then
// Cc, then Delivered-To), as long as the owner owns it; otherwise the username.
func ChooseFrom(a accounts.Account, requested string, orig *Original) (*mail.Address, error) {
	name := a.DisplayName
	if strings.TrimSpace(requested) != "" {
		x, err := parseAddr("from", requested)
		if err != nil {
			return nil, err
		}
		if !a.Owns(x.Address) {
			return nil, refusef("from: %s is not an address of account %s (its username or one of its aliases)", x.Address, a.Name)
		}
		return &mail.Address{Name: name, Address: x.Address}, nil
	}
	if orig != nil {
		var cands []string
		for _, l := range [][]Addr{orig.To, orig.Cc} {
			for _, x := range l {
				cands = append(cands, x.Address)
			}
		}
		cands = append(cands, orig.DeliveredTo...)
		for _, c := range cands {
			c = strings.TrimSpace(c)
			if validAddrSpec(c) && a.Owns(c) {
				return &mail.Address{Name: name, Address: c}, nil
			}
		}
	}
	u := strings.TrimSpace(a.Username)
	if !validAddrSpec(u) {
		return nil, refusef("from: the account username %q is not an e-mail address; give from (an alias)", trunc(u))
	}
	return &mail.Address{Name: name, Address: u}, nil
}

// Recipients computes To and Cc of a reply from the original.
//
// A reply goes to the original's Reply-To, else From. When that is the owner
// (replying to one's own message) it goes to the original's To instead. A
// reply-all adds the original's To and Cc, minus every owner address and minus
// anyone already addressed.
func Recipients(a accounts.Account, o *Original, replyAll bool) (to, cc []*mail.Address, err error) {
	conv := func(l []Addr) []*mail.Address {
		var out []*mail.Address
		for _, x := range l {
			if p, err := checkAddr("original", &mail.Address{Name: x.Name, Address: strings.TrimSpace(x.Address)}); err == nil {
				out = append(out, p)
			}
		}
		return out
	}
	primary := conv(o.ReplyTo)
	if len(primary) == 0 {
		primary = conv(o.From)
	}
	fromOwner := len(primary) > 0
	for _, p := range primary {
		if !a.Owns(p.Address) {
			fromOwner = false
		}
	}
	others := append(conv(o.To), conv(o.Cc)...)
	if fromOwner {
		// The owner's own message: answer the people it went to.
		primary = conv(o.To)
		others = conv(o.Cc)
	}
	seen := map[string]bool{}
	add := func(dst *[]*mail.Address, l []*mail.Address) {
		for _, p := range l {
			k := strings.ToLower(p.Address)
			if seen[k] || a.Owns(p.Address) {
				continue
			}
			seen[k] = true
			*dst = append(*dst, p)
		}
	}
	add(&to, primary)
	if replyAll {
		add(&cc, others)
	}
	if len(to) == 0 && len(cc) > 0 {
		to, cc = cc, nil
	}
	if len(to) == 0 {
		return nil, nil, refusef("the original has no recipient to reply to besides the owner; give to")
	}
	return to, cc, nil
}

var rePrefix = regexp.MustCompile(`(?i)^\s*re\s*:`)

// ReplySubject is "Re: <subject>" without stacking a second "Re:".
func ReplySubject(orig string) string {
	orig = strings.TrimSpace(cleanName(orig))
	if rePrefix.MatchString(orig) {
		return orig
	}
	if orig == "" {
		return "Re:"
	}
	return "Re: " + orig
}

var msgIDTokenRE = regexp.MustCompile(`^<[!-;=?-~]+>$`)

func validMsgID(t string) bool { return len(t) <= maxIDLen && msgIDTokenRE.MatchString(t) }

// threadHeaders builds In-Reply-To and References from the original's
// Message-ID and References: the original's chain plus the original, without
// duplicates, the first and the most recent maxReferences-1 kept.
func threadHeaders(o *Original) (inReplyTo string, refs []string) {
	id := strings.TrimSpace(o.MessageID)
	if !validMsgID(id) {
		return "", nil
	}
	seen := map[string]bool{}
	for _, t := range append(strings.Fields(o.References), id) {
		if validMsgID(t) && !seen[t] {
			seen[t] = true
			refs = append(refs, t)
		}
	}
	if len(refs) > maxReferences {
		refs = append([]string{refs[0]}, refs[len(refs)-(maxReferences-1):]...)
	}
	return id, refs
}

// quote renders the attribution line and the "> " quoted text of the original.
func quote(o *Original) string {
	who := ""
	if len(o.From) > 0 {
		x := o.From[0]
		who = cleanName(x.Address)
		if n := cleanName(x.Name); n != "" {
			who = n + " <" + cleanName(x.Address) + ">"
		}
	}
	if who == "" {
		who = "the sender"
	}
	when := ""
	if !o.Date.IsZero() {
		when = "On " + o.Date.UTC().Format("2006-01-02 15:04 UTC") + ", "
	} else {
		when = "Earlier, "
	}
	body := strings.ToValidUTF8(o.Body, "")
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	cut := false
	if len(body) > MaxQuoteBytes {
		body, cut = strings.ToValidUTF8(body[:MaxQuoteBytes], ""), true
	}
	var b strings.Builder
	b.WriteString(when + who + " wrote:\n")
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		ln = stripControls(ln)
		if ln == "" {
			b.WriteString(">\n")
		} else {
			b.WriteString("> " + ln + "\n")
		}
	}
	if cut {
		b.WriteString("> [quote truncated]\n")
	}
	return b.String()
}

func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || (r >= ' ' && r != 0x7f && r != ' ' && r != ' ') {
			return r
		}
		return -1
	}, s)
}

// replyToRedirects reports whether the original's Reply-To names an address
// that is not among its From addresses: a reply would go somewhere the
// sender's own address is not.
func replyToRedirects(o *Original) bool {
	if len(o.ReplyTo) == 0 {
		return false
	}
	from := map[string]bool{}
	for _, f := range o.From {
		from[strings.ToLower(strings.TrimSpace(f.Address))] = true
	}
	for _, r := range o.ReplyTo {
		if !from[strings.ToLower(strings.TrimSpace(r.Address))] {
			return true
		}
	}
	return false
}

func formatAddr(a *mail.Address) string { return a.String() }

// uniq returns the addresses of l not seen before (case-insensitive), marking
// them seen.
func uniq(seen map[string]bool, l []*mail.Address) []*mail.Address {
	var out []*mail.Address
	for _, p := range l {
		k := strings.ToLower(p.Address)
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

func parseAll(field string, in []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		a, err := parseAddr(field, s)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Compose validates the input and renders the message.
func Compose(in Input) (*Message, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	rnd := in.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	if !in.Account.DraftsEnabled() {
		return nil, refusef("drafts are turned off for account %s", in.Account.Name)
	}

	// Body.
	if !utf8.ValidString(in.Body) {
		return nil, refusef("body is not valid UTF-8")
	}
	if strings.ContainsRune(in.Body, 0) {
		return nil, refusef("body contains a NUL byte")
	}
	if len(in.Body) > MaxBodyBytes {
		return nil, refusef("body is %d bytes; the most is %d", len(in.Body), MaxBodyBytes)
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, refusef("body is empty")
	}

	// Recipients.
	redirect := false
	to, err := parseAll("to", in.To)
	if err != nil {
		return nil, err
	}
	cc, err := parseAll("cc", in.Cc)
	if err != nil {
		return nil, err
	}
	bcc, err := parseAll("bcc", in.Bcc)
	if err != nil {
		return nil, err
	}
	if o := in.Original; o != nil {
		rto, rcc, err := Recipients(in.Account, o, in.ReplyAll)
		if err != nil && len(to) == 0 {
			return nil, err
		}
		if len(to) == 0 {
			to = rto
		}
		cc = append(rcc, cc...)
		if len(in.To) == 0 && replyToRedirects(o) {
			redirect = true
		}
	}
	if len(to) == 0 {
		return nil, refusef("to is required when there is no reply_to")
	}
	seen := map[string]bool{}
	to, cc, bcc = uniq(seen, to), uniq(seen, cc), uniq(seen, bcc)
	if n := len(to) + len(cc) + len(bcc); n > MaxRecipients {
		return nil, refusef("%d recipients; the most is %d", n, MaxRecipients)
	}

	// From.
	from, err := ChooseFrom(in.Account, in.From, in.Original)
	if err != nil {
		return nil, err
	}

	// Subject.
	subject := in.Subject
	if strings.TrimSpace(subject) == "" {
		if in.Original == nil {
			return nil, refusef("subject is required when there is no reply_to")
		}
		subject = ReplySubject(in.Original.Subject)
	}
	if !headerSafe(subject) || strings.ContainsRune(subject, '\t') {
		return nil, refusef("subject contains a control character")
	}
	subject = strings.TrimSpace(subject)
	if !utf8.ValidString(subject) {
		return nil, refusef("subject is not valid UTF-8")
	}
	if utf8.RuneCountInString(subject) > MaxSubjectRunes {
		return nil, refusef("subject is longer than %d characters", MaxSubjectRunes)
	}

	// Body with quote.
	text := in.Body
	if in.Original != nil && in.QuoteOriginal && strings.TrimSpace(in.Original.Body) != "" {
		text = strings.TrimRight(text, "\r\n") + "\n\n" + quote(in.Original)
	}

	// Message-ID.
	var rb [12]byte
	if _, err := io.ReadFull(rnd, rb[:]); err != nil {
		return nil, fmt.Errorf("draft: random: %w", err)
	}
	domain := from.Address[strings.LastIndexByte(from.Address, '@')+1:]
	msgID := "<" + hex.EncodeToString(rb[:]) + "@" + strings.ToLower(domain) + ">"

	m := &Message{
		FromAddr: from.Address, From: formatAddr(from), Subject: subject, MessageID: msgID, Body: text,
	}
	for _, p := range to {
		m.To = append(m.To, formatAddr(p))
	}
	for _, p := range cc {
		m.Cc = append(m.Cc, formatAddr(p))
	}
	for _, p := range bcc {
		m.Bcc = append(m.Bcc, formatAddr(p))
	}
	bare := func(l []*mail.Address) []string {
		var out []string
		for _, p := range l {
			out = append(out, strings.ToLower(p.Address))
		}
		return out
	}
	m.ToAddrs, m.CcAddrs, m.BccAddrs, m.ReplyToRedirect = bare(to), bare(cc), bare(bcc), redirect

	var b strings.Builder
	hdr := func(name, value string) error {
		if !headerSafe(value) {
			return refusef("%s: header value contains a control character", name)
		}
		b.WriteString(foldHeader(name, value))
		return nil
	}
	steps := []struct{ n, v string }{
		{"Date", now.Format(time.RFC1123Z)},
		{"From", m.From},
		{"To", joinAddrs(m.To)},
	}
	if len(m.Cc) > 0 {
		steps = append(steps, struct{ n, v string }{"Cc", joinAddrs(m.Cc)})
	}
	if len(m.Bcc) > 0 {
		steps = append(steps, struct{ n, v string }{"Bcc", joinAddrs(m.Bcc)})
	}
	steps = append(steps, struct{ n, v string }{"Subject", encodeSubject(subject)}, struct{ n, v string }{"Message-ID", msgID})
	if in.Original != nil {
		if irt, refs := threadHeaders(in.Original); irt != "" {
			m.InReplyTo = irt
			steps = append(steps, struct{ n, v string }{"In-Reply-To", irt}, struct{ n, v string }{"References", strings.Join(refs, " ")})
		}
	}
	steps = append(steps,
		struct{ n, v string }{"MIME-Version", "1.0"},
		struct{ n, v string }{"Content-Type", "text/plain; charset=utf-8"},
		struct{ n, v string }{"Content-Transfer-Encoding", "quoted-printable"})
	for _, s := range steps {
		if err := hdr(s.n, s.v); err != nil {
			return nil, err
		}
	}
	for _, ln := range strings.Split(b.String(), "\r\n") {
		if len(ln) > maxHeaderLine {
			return nil, refusef("a header line is longer than %d bytes", maxHeaderLine)
		}
	}
	b.WriteString("\r\n")
	var qp strings.Builder
	w := quotedprintable.NewWriter(&qp)
	if _, err := w.Write([]byte(normalizeNewlines(text))); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	b.WriteString(qp.String())
	if !strings.HasSuffix(qp.String(), "\r\n") {
		b.WriteString("\r\n")
	}
	m.Raw = []byte(b.String())
	return m, nil
}

// normalizeNewlines makes every line break "\n" and ends the text with one;
// the quoted-printable writer turns them into CRLF.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.TrimRight(s, "\n") + "\n"
}

func joinAddrs(l []string) string { return strings.Join(l, ", ") }

// encodeSubject RFC 2047-encodes a subject that is not plain ASCII.
//
// A plain ASCII subject that itself looks like an encoded word ("=?...?=") is
// encoded too, so a client shows the subject that was previewed and does not
// decode it into something else.
func encodeSubject(s string) string {
	if strings.Contains(s, "=?") && strings.Contains(s, "?=") && mime.QEncoding.Encode("utf-8", s) == s {
		return forceQ(s)
	}
	return mime.QEncoding.Encode("utf-8", s)
}

// forceQ Q-encodes an ASCII string in encoded words of at most 30 source bytes.
func forceQ(s string) string {
	var words []string
	for len(s) > 0 {
		n := min(30, len(s))
		var b strings.Builder
		for i := 0; i < n; i++ {
			c := s[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "=%02X", c)
			}
		}
		words = append(words, "=?utf-8?q?"+b.String()+"?=")
		s = s[n:]
	}
	return strings.Join(words, "\r\n ")
}

const foldAt = 76

// foldHeader writes "Name: value\r\n", folding a long value at the spaces
// (and after the commas of an address list) so that lines stay short. A value
// that already holds a fold (an encoded-word chain) is written as it is.
func foldHeader(name, value string) string {
	if strings.Contains(value, "\r\n") {
		return name + ": " + value + "\r\n"
	}
	words := strings.Split(value, " ")
	var b strings.Builder
	line := name + ":"
	for i, w := range words {
		if i == 0 {
			line += " " + w
			continue
		}
		if len(line)+1+len(w) > foldAt {
			b.WriteString(line + "\r\n")
			line = " " + w
			continue
		}
		line += " " + w
	}
	b.WriteString(line + "\r\n")
	return b.String()
}
