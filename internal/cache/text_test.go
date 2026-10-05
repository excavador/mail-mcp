package cache

import (
	"strings"
	"testing"
)

func TestCleanBody(t *testing.T) {
	long := strings.Repeat("Dit is een lange alinea met echte inhoud die we willen bewaren. ", 8)
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain unchanged", "Hello Bob,\n\nSee you at 10.\n\nAlice", "Hello Bob,\n\nSee you at 10.\n\nAlice"},
		{"quoted lines", "Sounds good.\n\n> earlier text\n> more\nThanks", "Sounds good.\n\nThanks"},
		{"EN reply header", "Yes, do it.\n\nOn Mon, Jan 5, 2026 at 10:00 AM Bob Smith <bob@example.com> wrote:\n> Can we ship?\n> Please confirm.\n\nmore quoted junk", "Yes, do it."},
		{"EN wrapped header", "Agreed.\n\nOn Mon, Jan 5, 2026 at 10:00 AM Bob Smith <bob.smith@example.com>\nwrote:\n\n> Can we ship?", "Agreed."},
		{"DE", "Passt für mich.\n\nAm 05.01.2026 um 10:00 schrieb Hans Müller <hans@example.de>:\n> Treffen wir uns?", "Passt für mich."},
		{"NL", "Prima, tot dan.\n\nOp 5 jan. 2026 om 10:00 schreef Jan de Vries <jan@example.nl>:\n> Zullen we afspreken?", "Prima, tot dan."},
		{"RU pishet", "Согласен.\n\nИван Петров пишет:\n> Встретимся завтра?", "Согласен."},
		{"RU gmail", "Хорошо, договорились.\n\nпн, 5 янв. 2026 г. в 10:00, Иван Петров <ivan@example.ru>:\n> Встретимся?", "Хорошо, договорились."},
		{"RU napisal", "Ок.\n\n05.01.2026, 10:00, \"Иван\" <i@example.ru> написал(а):\n> привет", "Ок."},
		{"Outlook original", "Please see below.\n\n-----Original Message-----\nFrom: Bob <bob@example.com>\nSent: Monday, January 5, 2026 10:00 AM\nTo: Alice\nSubject: Re: plan\n\nold stuff", "Please see below."},
		{"Outlook block", "Thanks!\n\nFrom: Bob <bob@example.com>\nSent: Monday, January 5, 2026 10:00 AM\nTo: Alice\nSubject: plan\n\nold stuff", "Thanks!"},
		{"DE Outlook block", "Danke.\n\nVon: Hans <hans@example.de>\nGesendet: Montag, 5. Januar 2026 10:00\nAn: Alice\nBetreff: Plan\n\nalt", "Danke."},
		{"NL Outlook block", "Bedankt.\n\nVan: Jan <jan@example.nl>\nVerzonden: maandag 5 januari 2026 10:00\nAan: Alice\nOnderwerp: Plan\n\noud", "Bedankt."},
		{"signature", "Regards\n\nBody text here.\n-- \nAlice Example\nCTO, Example BV", "Regards\n\nBody text here."},
		{"mobile signature", "On my way.\n\nSent from my iPhone", "On my way."},
		{"mobile DE", "Bin gleich da.\n\nGesendet von meinem iPad", "Bin gleich da."},
		{"mobile RU", "Еду.\n\nОтправлено с iPhone", "Еду."},
		{"gmail forward", "FYI\n\n---------- Forwarded message ---------\nFrom: Carol <carol@example.com>\nDate: Mon, Jan 5, 2026 at 9:00 AM\nSubject: Invoice 42\nTo: <alice@example.com>\n\nPlease pay invoice 42.", "FYI\n\n" + ForwardedMarker + "\n\nPlease pay invoice 42."},
		{"forward only, header on top", "From: Carol <carol@example.com>\nDate: Mon, 5 Jan 2026\nSubject: Invoice 42\nTo: alice@example.com\n\nPlease pay invoice 42.", ForwardedMarker + "\n\nPlease pay invoice 42."},
		{"apple forward", "Look at this\n\nBegin forwarded message:\n\nFrom: Carol <c@example.com>\nSubject: Offer\nDate: 5 January 2026\nTo: me@example.com\n\nThe offer is valid.", "Look at this\n\n" + ForwardedMarker + "\n\nThe offer is valid."},
		{"only quote keeps original", "> a\n> b", "> a\n> b"},
		{"header at the end only drops the header", long + "\n" + "On Mon, Jan 5, 2026 at 10:00 AM Bob <b@example.com> wrote:", strings.TrimSpace(long)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanBody(tc.in)
			want := tc.want
			if got != want {
				t.Fatalf("CleanBody(%q)\n got %q\nwant %q", tc.in, got, want)
			}
		})
	}
}

func TestCleanBodyShortReplyOverLongQuoteIsKept(t *testing.T) {
	body := "ok\n\nOn Mon, Jan 5, 2026 at 10:00 AM Bob <b@example.com> wrote:\n" + strings.Repeat("> quoted line of text\n", 400)
	if got := CleanBody(body); got != "ok" {
		t.Fatalf("expected the new reply only, got %d bytes %q", len(got), got[:min(len(got), 40)])
	}
	// Nothing left at all: the original is kept so the message stays findable.
	if got := CleanBody("> a\n> b"); got != "> a\n> b" {
		t.Fatalf("got %q", got)
	}
}

func TestCleanBodyEmpty(t *testing.T) {
	if CleanBody("  \n ") != "" {
		t.Fatal("blank body must stay empty")
	}
}

func TestStripC0(t *testing.T) {
	if got := stripC0("a\x01b\x02c\td\ne\x7ff\r"); got != "abc\td\nef" {
		t.Fatalf("got %q", got)
	}
}

func TestRewriteBodyFilterSkipsQuotedPhrases(t *testing.T) {
	for in, want := range map[string]string{
		`body:foo`:               `{body_new body_full}:foo`,
		`"body:foo"`:             `"body:foo"`,
		`"a" OR body:x`:          `"a" OR {body_new body_full}:x`,
		`"say ""body:"" now"`:    `"say ""body:"" now"`,
		`subject:s AND body:"y"`: `subject:s AND {body_new body_full}:"y"`,
	} {
		if got := rewriteBodyFilter(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestPlainTextEntitiesDecodedOnce(t *testing.T) {
	raw := "From: a@x.com\r\nTo: b@x.com\r\nSubject: s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Use &lt;ul&gt; &amp; &quot;li&quot; &#39;x&#39; AT&amp;T &amp;lt; see https://x.test/?a=1&copy=2\r\n"
	p := parseMessage([]byte(raw))
	want := `Use <ul> & "li" 'x' AT&T &lt; see https://x.test/?a=1&copy=2`
	if p.Body != want || p.BodyNew != want {
		t.Errorf("body %q, new %q; want %q", p.Body, p.BodyNew, want)
	}
	plain := "From: a@x.com\r\nSubject: s\r\nContent-Type: text/plain\r\n\r\nno entities & here <b>\r\n"
	if got := parseMessage([]byte(plain)).Body; got != "no entities & here <b>" {
		t.Errorf("untouched plain: %q", got)
	}
}

func TestHTMLEntitiesDecodedAfterTagsStripped(t *testing.T) {
	got := stripHTML("<p>a &lt;b&gt;bold&lt;/b&gt; &amp; c</p>")
	if got != "a <b>bold</b> & c" {
		t.Errorf("got %q", got)
	}
}
