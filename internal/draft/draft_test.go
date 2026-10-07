package draft

import (
	"bytes"
	"net/mail"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

var now = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

func fixedRand() *bytes.Reader {
	return bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
}

// acct has no exported constructor for aliases, so the tests build the struct.
func acct() accounts.Account {
	return accounts.Account{
		Name: "work", Provider: accounts.Proton, Username: "me@example.com",
		Aliases: []string{"me.alias@example.com", "@example.org"},
	}
}

func orig() *Original {
	return &Original{
		StableID:   "s1",
		MessageID:  "<m3@x.test>",
		References: "<m1@x.test> <m2@x.test>",
		Subject:    "Lunch?",
		Date:       time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		From:       []Addr{{"Alice A", "alice@x.test"}},
		To:         []Addr{{"Me", "me.alias@example.com"}, {"Bob", "bob@x.test"}},
		Cc:         []Addr{{"", "carol@x.test"}, {"", "me@example.com"}},
		Body:       "Shall we?\n\nYes.\n",
	}
}

func compose(t *testing.T, in Input) *Message {
	t.Helper()
	if in.Account.Name == "" {
		in.Account = acct()
	}
	in.Now, in.Rand = now, fixedRand()
	m, err := Compose(in)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return m
}

func refused(t *testing.T, in Input, want string) {
	t.Helper()
	if in.Account.Name == "" {
		in.Account = acct()
	}
	in.Now, in.Rand = now, fixedRand()
	_, err := Compose(in)
	if err == nil {
		t.Fatalf("Compose accepted the input, want a refusal containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func TestGoldenNewMessageASCII(t *testing.T) {
	m := compose(t, Input{To: []string{"Bob <bob@x.test>"}, Subject: "Hello", Body: "Hi Bob,\nsee you.\n"})
	want := crlf(`Date: Wed, 07 Oct 2026 09:30:00 +0000
From: <me@example.com>
To: "Bob" <bob@x.test>
Subject: Hello
Message-ID: <000102030405060708090a0b@example.com>
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Hi Bob,
see you.
`)
	if string(m.Raw) != want {
		t.Errorf("raw message:\n%q\nwant:\n%q", m.Raw, want)
	}
	if m.InReplyTo != "" || strings.Contains(string(m.Raw), "References") {
		t.Error("a new message carries threading headers")
	}
}

func TestGoldenReplyNonASCIIWithQuoteAndThreading(t *testing.T) {
	a := acct()
	a.DisplayName = "Oleg Tsarev"
	m := compose(t, Input{Account: a, Original: orig(), QuoteOriginal: true, Body: "Ja, gern — 12:30?\n"})
	want := crlf(`Date: Wed, 07 Oct 2026 09:30:00 +0000
From: "Oleg Tsarev" <me.alias@example.com>
To: "Alice A" <alice@x.test>
Subject: Re: Lunch?
Message-ID: <000102030405060708090a0b@example.com>
In-Reply-To: <m3@x.test>
References: <m1@x.test> <m2@x.test> <m3@x.test>
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Ja, gern =E2=80=94 12:30?

On 2026-03-04 05:06 UTC, Alice A <alice@x.test> wrote:
> Shall we?
>
> Yes.
`)
	if string(m.Raw) != want {
		t.Errorf("raw message:\n%s\nwant:\n%s", m.Raw, want)
	}
	if m.FromAddr != "me.alias@example.com" || m.InReplyTo != "<m3@x.test>" {
		t.Errorf("FromAddr %q, InReplyTo %q", m.FromAddr, m.InReplyTo)
	}
}

func TestNonASCIISubjectIsEncodedAndRoundTrips(t *testing.T) {
	subj := "Überraschung für Zoë — 日本語 and a rather long tail to force the encoded words to wrap over several lines"
	m := compose(t, Input{To: []string{"bob@x.test"}, Subject: subj, Body: "x"})
	raw := string(m.Raw)
	if !strings.Contains(raw, "Subject: =?utf-8?q?") {
		t.Fatalf("subject not RFC 2047-encoded:\n%s", raw)
	}
	for _, ln := range strings.Split(raw, "\r\n") {
		if len(ln) > 78 {
			t.Errorf("line over 78 bytes (%d): %q", len(ln), ln)
		}
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := new(wordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || got != subj {
		t.Errorf("decoded subject = %q, %v; want %q", got, err, subj)
	}
	if h := msg.Header.Get("Date"); h == "" {
		t.Error("no Date")
	}
}

func TestReplySubjectDoesNotStack(t *testing.T) {
	for in, want := range map[string]string{
		"Lunch?":      "Re: Lunch?",
		"Re: Lunch?":  "Re: Lunch?",
		"RE: Lunch?":  "RE: Lunch?",
		"re:Lunch?":   "re:Lunch?",
		"  Re : x":    "Re : x",
		"Reading":     "Re: Reading",
		"":            "Re:",
		"a\r\nBcc: x": "Re: aBcc: x", // control characters of the original never survive
	} {
		if got := ReplySubject(in); got != want {
			t.Errorf("ReplySubject(%q) = %q, want %q", in, got, want)
		}
	}
	o := orig()
	o.Subject = "Re: Lunch?"
	m := compose(t, Input{Original: o, Body: "ok"})
	if m.Subject != "Re: Lunch?" {
		t.Errorf("subject = %q", m.Subject)
	}
}

func TestChooseFrom(t *testing.T) {
	a := acct()
	cases := []struct {
		name, req string
		o         *Original
		want      string
		err       string
	}{
		{"username by default", "", nil, "me@example.com", ""},
		{"exact alias", "me.alias@example.com", nil, "me.alias@example.com", ""},
		{"alias case-insensitive, name dropped", "Me <ME.Alias@Example.com>", nil, "ME.Alias@Example.com", ""},
		{"domain alias, any local part", "sales@example.org", nil, "sales@example.org", ""},
		{"domain alias, not a subdomain", "x@a.example.org", nil, "", "not an address of account"},
		{"stranger", "mallory@evil.test", nil, "", "not an address of account"},
		{"lookalike of the username", "me@example.com.evil.test", nil, "", "not an address of account"},
		{"default: the address the original went to", "", orig(), "me.alias@example.com", ""},
		{"default: a domain-alias address in Cc", "", &Original{Cc: []Addr{{"", "info@example.org"}}}, "info@example.org", ""},
		{"default: Delivered-To", "", &Original{To: []Addr{{"", "list@x.test"}}, DeliveredTo: []string{"me.alias@example.com"}}, "me.alias@example.com", ""},
		{"default: nothing of ours addressed", "", &Original{To: []Addr{{"", "list@x.test"}}}, "me@example.com", ""},
		{"username that is not an address", "", nil, "", "not an e-mail address"},
		{"not an address", "not an address", nil, "", "not a valid address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := a
			if strings.HasPrefix(c.name, "username that") {
				a.Username = "u"
			}
			got, err := ChooseFrom(a, c.req, c.o)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || got.Address != c.want {
				t.Fatalf("From = %v, %v; want %s", got, err, c.want)
			}
		})
	}
}

func addrs(l []*mail.Address) []string {
	var out []string
	for _, a := range l {
		out = append(out, a.Address)
	}
	return out
}

func TestRecipients(t *testing.T) {
	a := acct()
	o := orig()
	to, cc, err := Recipients(a, o, false)
	if err != nil || strings.Join(addrs(to), ",") != "alice@x.test" || len(cc) != 0 {
		t.Errorf("reply: to %v cc %v err %v", addrs(to), addrs(cc), err)
	}
	// reply-all: the owner's addresses (alias in To, username in Cc) are dropped, so are duplicates.
	o.Cc = append(o.Cc, Addr{"", "BOB@x.test"}, Addr{"", "alice@x.test"}, Addr{"", "sales@example.org"})
	to, cc, err = Recipients(a, o, true)
	if err != nil || strings.Join(addrs(to), ",") != "alice@x.test" || strings.Join(addrs(cc), ",") != "bob@x.test,carol@x.test" {
		t.Errorf("reply-all: to %v cc %v err %v", addrs(to), addrs(cc), err)
	}
	// Reply-To wins over From.
	o.ReplyTo = []Addr{{"List", "list@x.test"}}
	to, _, _ = Recipients(a, o, false)
	if strings.Join(addrs(to), ",") != "list@x.test" {
		t.Errorf("reply-to: %v", addrs(to))
	}
	// Replying to the owner's own message answers the people it went to.
	own := &Original{From: []Addr{{"", "me.alias@example.com"}}, To: []Addr{{"", "bob@x.test"}}, Cc: []Addr{{"", "carol@x.test"}}}
	to, cc, err = Recipients(a, own, true)
	if err != nil || strings.Join(addrs(to), ",") != "bob@x.test" || strings.Join(addrs(cc), ",") != "carol@x.test" {
		t.Errorf("own message: to %v cc %v err %v", addrs(to), addrs(cc), err)
	}
	// Nobody but the owner: nothing to reply to.
	self := &Original{From: []Addr{{"", "me@example.com"}}, To: []Addr{{"", "me@example.com"}}}
	if _, _, err := Recipients(a, self, true); err == nil {
		t.Error("a message from and to the owner alone produced recipients")
	}
}

func TestExplicitRecipientsOverrideAndDedupe(t *testing.T) {
	m := compose(t, Input{
		Original: orig(), ReplyAll: true, Body: "x",
		To: []string{"dave@x.test"}, Cc: []string{"alice@x.test", "dave@x.test"}, Bcc: []string{"erin@x.test", "ALICE@x.test"},
	})
	if len(m.To) != 1 || !strings.Contains(m.To[0], "dave@x.test") {
		t.Errorf("To = %v", m.To)
	}
	// Computed Cc (bob, carol) + explicit alice; dave (already in To) and the duplicate are dropped.
	got := strings.Join(m.Cc, " | ")
	for _, w := range []string{"bob@x.test", "carol@x.test", "alice@x.test"} {
		if !strings.Contains(got, w) {
			t.Errorf("Cc %q lacks %s", got, w)
		}
	}
	if len(m.Cc) != 3 || len(m.Bcc) != 1 || !strings.Contains(m.Bcc[0], "erin@x.test") {
		t.Errorf("Cc %v Bcc %v", m.Cc, m.Bcc)
	}
	if !strings.Contains(string(m.Raw), "\r\nBcc: ") {
		t.Error("Bcc is not kept in the draft headers")
	}
}

func TestHeaderInjectionRefused(t *testing.T) {
	bad := []string{"a\r\nBcc: evil@x.test", "a\nb", "a\rb", "a\x00b", "a\x7fb", "a b"}
	for _, s := range bad {
		refused(t, Input{To: []string{"bob@x.test"}, Subject: s, Body: "x"}, "control character")
		refused(t, Input{To: []string{"bob@x.test" + s}, Subject: "s", Body: "x"}, "")
		refused(t, Input{To: []string{"bob@x.test"}, Cc: []string{s}, Subject: "s", Body: "x"}, "")
		refused(t, Input{To: []string{"bob@x.test"}, Bcc: []string{"<bob@x.test>" + s}, Subject: "s", Body: "x"}, "")
		refused(t, Input{To: []string{"bob@x.test"}, From: "me@example.com" + s, Subject: "s", Body: "x"}, "")
	}
	refused(t, Input{To: []string{"Bob\r\nX: y <bob@x.test>"}, Subject: "s", Body: "x"}, "control character")
	refused(t, Input{To: []string{"bob@x.test"}, Subject: "s", Body: "a\x00b"}, "NUL")
	// A display name in a config cannot carry one either (accounts.validate), but
	// a hostile one in the original is cleaned, not trusted.
	o := orig()
	o.From = []Addr{{"Eve\r\nBcc: evil@x.test", "eve@x.test"}}
	o.Subject = "hi\r\nBcc: evil@x.test"
	m := compose(t, Input{Original: o, QuoteOriginal: true, Body: "x"})
	for _, ln := range strings.Split(string(m.Raw), "\r\n") {
		if strings.HasPrefix(ln, "Bcc:") {
			t.Errorf("injected header survived: %q", ln)
		}
	}
	head, _, _ := strings.Cut(string(m.Raw), "\r\n\r\n")
	if strings.Contains(head, "evil") && !strings.Contains(head, "Subject: Re: hiBcc: evil@x.test") {
		t.Errorf("hostile text outside the subject in the headers:\n%s", head)
	}
}

func TestInvalidAddressesAndLimits(t *testing.T) {
	refused(t, Input{To: []string{"nobody"}, Subject: "s", Body: "x"}, "not a valid address")
	refused(t, Input{To: []string{"bob@x.test, carol@x.test"}, Subject: "s", Body: "x"}, "not a valid address")
	refused(t, Input{To: []string{"Group: a@x.test, b@x.test;"}, Subject: "s", Body: "x"}, "not a valid address")
	refused(t, Input{To: []string{"bob@exämple.test"}, Subject: "s", Body: "x"}, "plain ASCII")
	refused(t, Input{Subject: "s", Body: "x"}, "to is required")
	refused(t, Input{To: []string{"bob@x.test"}, Body: "x"}, "subject is required")
	refused(t, Input{To: []string{"bob@x.test"}, Subject: "s", Body: "  \n"}, "body is empty")
	refused(t, Input{To: []string{"bob@x.test"}, Subject: "s", Body: strings.Repeat("a", MaxBodyBytes+1)}, "the most is")
	refused(t, Input{To: []string{"bob@x.test"}, Subject: "s", Body: "\xff\xfe"}, "UTF-8")
	refused(t, Input{To: []string{"bob@x.test"}, Subject: strings.Repeat("s", MaxSubjectRunes+1), Body: "x"}, "subject is longer")
	var many []string
	for i := range MaxRecipients + 1 {
		many = append(many, "u"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))+"@x.test")
	}
	refused(t, Input{To: many, Subject: "s", Body: "x"}, "recipients; the most is")
	off := acct()
	f := false
	off.Drafts = &f
	refused(t, Input{Account: off, To: []string{"bob@x.test"}, Subject: "s", Body: "x"}, "turned off")
	refused(t, Input{To: []string{"bob@x.test"}, From: "mallory@evil.test", Subject: "s", Body: "x"}, "not an address of account")
}

func TestReferencesDedupedAndCapped(t *testing.T) {
	o := orig()
	var refs []string
	for i := range 40 {
		refs = append(refs, "<r"+string(rune('a'+i%26))+string(rune('a'+i/26))+"@x.test>")
	}
	o.References = strings.Join(refs, " ") + " <ra@x.test> junk <bad id> <m3@x.test>"
	_, got := threadHeaders(o)
	if len(got) != maxReferences {
		t.Fatalf("References has %d ids, want %d: %v", len(got), maxReferences, got)
	}
	if got[0] != refs[0] || got[len(got)-1] != "<m3@x.test>" {
		t.Errorf("first %s last %s", got[0], got[len(got)-1])
	}
	seen := map[string]bool{}
	for _, g := range got {
		if seen[g] {
			t.Errorf("duplicate %s", g)
		}
		seen[g] = true
	}
	// No usable Message-ID: no threading headers at all.
	o.MessageID = "m3@x.test"
	if irt, refs := threadHeaders(o); irt != "" || refs != nil {
		t.Errorf("threading headers from a malformed Message-ID: %q %v", irt, refs)
	}
}

func TestQuoteIsCappedAndPrefixed(t *testing.T) {
	o := orig()
	o.Body = strings.Repeat("line of text\n", 5000)
	q := quote(o)
	if len(q) > MaxQuoteBytes+MaxQuoteBytes/4 || !strings.HasSuffix(q, "> [quote truncated]\n") {
		t.Errorf("quote is %d bytes, ends %q", len(q), q[len(q)-30:])
	}
	for _, ln := range strings.Split(strings.TrimRight(q, "\n"), "\n")[1:] {
		if !strings.HasPrefix(ln, ">") {
			t.Fatalf("unquoted line %q", ln)
		}
	}
	// Not asked for: not quoted. Asked for but nothing to quote: no empty attribution.
	m := compose(t, Input{Original: orig(), Body: "ok"})
	if strings.Contains(m.Body, "wrote:") {
		t.Error("quoted without quote_original")
	}
	o2 := orig()
	o2.Body = " \n"
	m = compose(t, Input{Original: o2, QuoteOriginal: true, Body: "ok"})
	if strings.Contains(m.Body, "wrote:") {
		t.Error("attribution for an empty original")
	}
}

func TestLongAddressListFoldsAndParses(t *testing.T) {
	var to []string
	for _, n := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		to = append(to, n+"@a-fairly-long-domain-name.example.test")
	}
	m := compose(t, Input{To: to, Subject: "s", Body: "x"})
	for _, ln := range strings.Split(string(m.Raw), "\r\n") {
		if len(ln) > 78 {
			t.Errorf("line over 78 bytes: %q", ln)
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(m.Raw))
	if err != nil {
		t.Fatal(err)
	}
	l, err := msg.Header.AddressList("To")
	if err != nil || len(l) != 8 {
		t.Errorf("To parses to %d addresses, %v", len(l), err)
	}
}

func TestBodyIsQuotedPrintableCRLF(t *testing.T) {
	m := compose(t, Input{To: []string{"bob@x.test"}, Subject: "s", Body: "a = b\r\nlast line without newline é " + strings.Repeat("w", 200)})
	raw := string(m.Raw)
	_, body, _ := strings.Cut(raw, "\r\n\r\n")
	if strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\r") {
		t.Error("a bare CR or LF in the message")
	}
	for _, ln := range strings.Split(body, "\r\n") {
		if len(ln) > 76 {
			t.Errorf("QP line of %d bytes", len(ln))
		}
	}
	if !strings.Contains(body, "a =3D b") || !strings.Contains(body, "=C3=A9") {
		t.Errorf("body not quoted-printable:\n%s", body)
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

type wordDecoder = mimeWordDecoder

func TestEncodedWordLookalikeSubjectIsEncoded(t *testing.T) {
	subj := "Invoice =?utf-8?q?paid?= now"
	m := compose(t, Input{To: []string{"bob@x.test"}, Subject: subj, Body: "x"})
	head, _, _ := strings.Cut(string(m.Raw), "\r\n\r\n")
	if strings.Contains(head, "Subject: Invoice =?utf-8?q?paid") {
		t.Fatalf("lookalike subject sent literally:\n%s", head)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(m.Raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := new(wordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || got != subj {
		t.Errorf("decoded subject = %q, %v; want %q", got, err, subj)
	}
}

func TestOverlongIDsAndNamesAreHandled(t *testing.T) {
	o := orig()
	long := "<" + strings.Repeat("a", 300) + "@x.test>"
	o.References = long + " <m1@x.test>"
	_, refs := threadHeaders(o)
	for _, r := range refs {
		if len(r) > maxIDLen {
			t.Errorf("over-long token kept: %d", len(r))
		}
	}
	o.MessageID = long
	if irt, refs := threadHeaders(o); irt != "" || refs != nil {
		t.Error("over-long Message-ID used")
	}
	o = orig()
	o.From = []Addr{{strings.Repeat("N", 5000), "alice@x.test"}}
	m := compose(t, Input{Original: o, Body: "x"})
	for _, ln := range strings.Split(string(m.Raw), "\r\n") {
		if len(ln) > 998 {
			t.Fatalf("header line of %d bytes", len(ln))
		}
	}
}

func TestReplyToRedirectAndBareAddresses(t *testing.T) {
	o := orig()
	o.ReplyTo = []Addr{{"", "alice@x.test"}} // same as From: not a redirect
	m := compose(t, Input{Original: o, Body: "x"})
	if m.ReplyToRedirect {
		t.Error("same-address Reply-To flagged")
	}
	o.ReplyTo = []Addr{{"", "Attacker@Evil.test"}}
	m = compose(t, Input{Original: o, Body: "x"})
	if !m.ReplyToRedirect || strings.Join(m.ToAddrs, ",") != "attacker@evil.test" {
		t.Errorf("redirect %v to %v", m.ReplyToRedirect, m.ToAddrs)
	}
	// An explicit To is not a redirect.
	m = compose(t, Input{Original: o, To: []string{"bob@x.test"}, Body: "x"})
	if m.ReplyToRedirect {
		t.Error("explicit To flagged as redirect")
	}
}
