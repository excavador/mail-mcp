package draft

import (
	"regexp"
	"strings"
	"testing"
)

const testBoundary = "=_mailmcp_000102030405060708090a0b"

// bodyHTML is the HTML of the message without the fixed wrapper.
func bodyHTML(t *testing.T, htm string) string {
	t.Helper()
	const pre, post = `<html><body><div dir="ltr">`, "</div></body></html>\n"
	if !strings.HasPrefix(htm, pre) || !strings.HasSuffix(htm, post) {
		t.Fatalf("html wrapper missing: %q", htm)
	}
	return strings.TrimSuffix(strings.TrimPrefix(htm, pre), post)
}

func htmlOf(t *testing.T, in Input) string {
	t.Helper()
	if in.To == nil && in.Original == nil {
		in.To = []string{"bob@x.test"}
	}
	if in.Subject == "" && in.Original == nil {
		in.Subject = "s"
	}
	return bodyHTML(t, parse(t, compose(t, in).Raw).htm)
}

func TestPlainPartIsThePreviousPlainOutput(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"no quote", Input{To: []string{"bob@x.test"}, Subject: "s", Body: "one\n\ntwo   \n\n\n"}, "one\n\ntwo   \n"},
		{"crlf body", Input{To: []string{"bob@x.test"}, Subject: "s", Body: "a\r\nb\r\n"}, "a\nb\n"},
		{"quote requested", Input{Original: orig(), QuoteOriginal: true, Body: "Hi\n\n\n"},
			"Hi\n\nOn 2026-03-04 05:06 UTC, Alice A <alice@x.test> wrote:\n> Shall we?\n>\n> Yes.\n"},
		{"quote not requested", Input{Original: orig(), Body: "Hi\n"}, "Hi\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := parse(t, compose(t, c.in).Raw)
			if p.plain != c.want {
				t.Errorf("plain = %q, want %q", p.plain, c.want)
			}
		})
	}
}

func TestHTMLEscapesBodyQuoteAndAttribution(t *testing.T) {
	o := orig()
	o.From = []Addr{{`Eve <script>"x"</script>`, "eve@x.test"}}
	o.Body = `<script>alert('q')</script> & "quoted" <a href="http://evil.test">x</a> <img src="http://evil.test/p.png">` + "\n"
	m := compose(t, Input{Original: o, QuoteOriginal: true, Body: `<script>alert("b")</script> & 'single' "double" <a href="x">`})
	p := parse(t, m.Raw)

	for _, bad := range []string{"<script", "<a ", "<img", "</script"} {
		if strings.Contains(strings.ToLower(p.htm), bad) {
			t.Errorf("html contains %q:\n%s", bad, p.htm)
		}
	}
	// The hostile text mentions src= and href=; as escaped text it is harmless,
	// but no real tag may carry them.
	for _, tag := range regexp.MustCompile(`<[^>]*>`).FindAllString(p.htm, -1) {
		if strings.Contains(tag, "src=") || strings.Contains(tag, "href=") {
			t.Errorf("tag %q carries a URL attribute", tag)
		}
	}
	// The only style attribute is the blockquote's.
	if n := strings.Count(p.htm, "style="); n != 1 || !strings.Contains(p.htm, `<blockquote style="`+quoteStyle+`">`) {
		t.Errorf("style= appears %d times:\n%s", n, p.htm)
	}
	for _, want := range []string{
		"&lt;script&gt;alert(&#34;b&#34;)&lt;/script&gt; &amp; &#39;single&#39; &#34;double&#34; &lt;a href=&#34;x&#34;&gt;", // body
		"&lt;script&gt;alert(&#39;q&#39;)&lt;/script&gt; &amp; &#34;quoted&#34;",                                             // quoted original
		"<div>On 2026-03-04 05:06 UTC, Eve &lt;script&gt;&#34;x&#34;&lt;/script&gt; &lt;eve@x.test&gt; wrote:</div>",         // attribution
	} {
		if !strings.Contains(p.htm, want) {
			t.Errorf("html lacks %q:\n%s", want, p.htm)
		}
	}
	// No raw quote characters outside the markup's own attributes.
	if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(p.htm, `dir="ltr"`, ""), `style="`+quoteStyle+`"`, ""), `"`) {
		t.Errorf("an unescaped double quote:\n%s", p.htm)
	}
	if strings.Contains(p.htm, "'") {
		t.Errorf("an unescaped single quote:\n%s", p.htm)
	}
	// The plain part is untouched by escaping.
	if !strings.Contains(p.plain, `<script>alert("b")</script> & 'single'`) {
		t.Errorf("plain = %q", p.plain)
	}
}

func TestHTMLParagraphsAndLineBreaks(t *testing.T) {
	want := "<p>a<br>b</p><p>c</p>"
	for name, body := range map[string]string{
		"LF":                    "a\nb\n\nc\n",
		"CRLF":                  "a\r\nb\r\n\r\nc\r\n",
		"CR":                    "a\rb\r\rc\r",
		"whitespace-only blank": "a\nb\n \t \nc",
		"CRLF whitespace-only":  "a\r\nb\r\n  \r\nc\r\n",
		"many blank lines":      "a\nb\n\n\n\n\nc\n\n\n",
		"leading blank":         "\n\na\nb\n\nc",
	} {
		t.Run(name, func(t *testing.T) {
			if got := htmlOf(t, Input{Body: body}); got != want {
				t.Errorf("html = %q, want %q", got, want)
			}
		})
	}
	if got := htmlOf(t, Input{Body: "just one line"}); got != "<p>just one line</p>" {
		t.Errorf("single line: %q", got)
	}
}

func TestHTMLLists(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"dash", "- a\n- b\n", "<ul><li>a</li><li>b</li></ul>"},
		{"star", "* a\n* b", "<ul><li>a</li><li>b</li></ul>"},
		{"dash and star", "- a\n* b", "<ul><li>a</li><li>b</li></ul>"},
		{"single item", "- only", "<ul><li>only</li></ul>"},
		{"ordered", "1. a\n2. b\n10. c\n", "<ol><li>a</li><li>b</li><li>c</li></ol>"},
		{"ordered crlf", "1. a\r\n2. b\r\n", "<ol><li>a</li><li>b</li></ol>"},
		{"items are escaped", "- <b>&</b>\n- 'x'", "<ul><li>&lt;b&gt;&amp;&lt;/b&gt;</li><li>&#39;x&#39;</li></ul>"},
		{"mixed paragraph", "intro\n- a\n- b", "<p>intro<br>- a<br>- b</p>"},
		{"list then text", "- a\nb", "<p>- a<br>b</p>"},
		{"bullets and numbers", "- a\n1. b", "<p>- a<br>1. b</p>"},
		{"no space after dash", "-x\n-y", "<p>-x<br>-y</p>"},
		{"no space after star", "*x", "<p>*x</p>"},
		{"no space after number", "1.a\n2.b", "<p>1.a<br>2.b</p>"},
		{"number without dot", "1) a\n2) b", "<p>1) a<br>2) b</p>"},
		{"indented is text", " - a\n - b", "<p> - a<br> - b</p>"},
		{"paragraphs and lists", "Intro\n\n- a\n- b\n\n1. x\n2. y\n\nBye", "<p>Intro</p><ul><li>a</li><li>b</li></ul><ol><li>x</li><li>y</li></ol><p>Bye</p>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := htmlOf(t, Input{Body: c.body}); got != c.want {
				t.Errorf("html = %q, want %q", got, c.want)
			}
		})
	}
}

func TestHTMLQuoteShape(t *testing.T) {
	o := orig()
	o.Body = "first\n\nthird\r\nfourth\n\n\n"
	got := htmlOf(t, Input{Original: o, QuoteOriginal: true, Body: "ok"})
	want := `<p>ok</p><div>On 2026-03-04 05:06 UTC, Alice A &lt;alice@x.test&gt; wrote:</div>` +
		`<blockquote style="` + quoteStyle + `">first<br><br>third<br>fourth</blockquote>`
	if got != want {
		t.Errorf("html = %q\nwant   %q", got, want)
	}
	if strings.Count(got, "<blockquote") != 1 || strings.Index(got, "<div>On ") > strings.Index(got, "<blockquote") {
		t.Error("attribution does not precede the single blockquote")
	}
}

func TestHTMLQuoteTruncated(t *testing.T) {
	o := orig()
	o.Body = strings.Repeat("line of text\n", 5000) // 65 000 bytes, over MaxQuoteBytes
	m := compose(t, Input{Original: o, QuoteOriginal: true, Body: "ok"})
	p := parse(t, m.Raw)
	if !strings.Contains(p.htm, "<br>[quote truncated]</blockquote>") {
		t.Errorf("html does not end its quote with the truncation note: %q", p.htm[len(p.htm)-120:])
	}
	if !strings.HasSuffix(p.plain, "> [quote truncated]\n") {
		t.Errorf("plain does not end with the truncation note")
	}
	if n := strings.Count(p.htm, "line of text"); n < 1000 || n >= 5000 {
		t.Errorf("%d quoted lines", n)
	}
	// Below the cap there is no note.
	o.Body = "short\n"
	p = parse(t, compose(t, Input{Original: o, QuoteOriginal: true, Body: "ok"}).Raw)
	if strings.Contains(p.htm, "truncated") || strings.Contains(p.plain, "truncated") {
		t.Error("truncation note on a short quote")
	}
}

func TestNoBlockquoteWithoutQuote(t *testing.T) {
	empty := orig()
	empty.Body = " \n\n"
	for name, in := range map[string]Input{
		"quote_original false": {Original: orig(), Body: "ok"},
		"empty original body":  {Original: empty, QuoteOriginal: true, Body: "ok"},
		"no original":          {To: []string{"bob@x.test"}, Subject: "s", QuoteOriginal: true, Body: "ok"},
	} {
		t.Run(name, func(t *testing.T) {
			p := parse(t, compose(t, in).Raw)
			for _, bad := range []string{"<blockquote", "wrote:", "style="} {
				if strings.Contains(p.htm, bad) {
					t.Errorf("html contains %q: %s", bad, p.htm)
				}
			}
			if strings.Contains(p.plain, "wrote:") {
				t.Errorf("plain contains an attribution: %q", p.plain)
			}
			if got := bodyHTML(t, p.htm); got != "<p>ok</p>" {
				t.Errorf("html = %q", got)
			}
		})
	}
}

func TestNonASCIIRoundTripsInBothParts(t *testing.T) {
	o := orig()
	o.Body = "Привет — мир\n"
	body := "Здравствуйте, Алиса — до встречи в 12:30!\n\n- пункт один\n- пункт два\n"
	m := compose(t, Input{Original: o, QuoteOriginal: true, Body: body})
	p := parse(t, m.Raw)
	for name, got := range map[string]string{"plain": p.plain, "html": p.htm} {
		for _, want := range []string{"Здравствуйте, Алиса — до встречи в 12:30!", "Привет — мир"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s part lacks %q:\n%s", name, want, got)
			}
		}
	}
	if !strings.Contains(p.htm, "<li>пункт один</li><li>пункт два</li>") {
		t.Errorf("html list: %s", p.htm)
	}
	for i, r := range p.rawParts {
		for _, c := range []byte(r) {
			if c >= 0x80 {
				t.Fatalf("part %d has a raw non-ASCII byte", i)
			}
		}
	}
}

func TestMultipartFraming(t *testing.T) {
	body := "line one\n" + strings.Repeat("long ", 80) + "\n= sign é — \n--" + testBoundary + "\n" + testBoundary + "\n"
	o := orig()
	o.Body = "--" + testBoundary + "\n=_mailmcp_ quoted\n"
	m := compose(t, Input{Original: o, QuoteOriginal: true, Body: body})
	raw := string(m.Raw)
	p := parse(t, m.Raw)

	if p.boundary != testBoundary {
		t.Fatalf("boundary = %q", p.boundary)
	}
	head, rest, _ := strings.Cut(raw, "\r\n\r\n")
	if !strings.Contains(strings.ReplaceAll(head, "\r\n ", " "), `boundary="`+testBoundary+`"`) {
		t.Errorf("boundary not in the header:\n%s", head)
	}
	if n := strings.Count(rest, "\r\n--"+testBoundary+"\r\n"); n != 1 { // the second part; the first follows the header
		t.Errorf("%d inner delimiters, want 1", n)
	}
	if !strings.HasPrefix(rest, "--"+testBoundary+"\r\n") {
		t.Errorf("body does not open with a delimiter: %q", rest[:40])
	}
	if !strings.HasSuffix(raw, "\r\n--"+testBoundary+"--\r\n") {
		t.Errorf("closing delimiter missing: %q", raw[len(raw)-60:])
	}
	if strings.Count(raw, testBoundary+"--") != 1 {
		t.Error("more than one closing delimiter")
	}
	// A delimiter inside a part would split it: the boundary starts with "=",
	// which quoted-printable always encodes.
	for i, r := range p.rawParts {
		if strings.Contains(r, testBoundary) || strings.Contains(r, "--=_") {
			t.Errorf("boundary inside part %d:\n%s", i, r)
		}
	}
	// ... yet the text round-trips.
	if !strings.Contains(p.plain, "--"+testBoundary+"\n") || !strings.Contains(p.htm, "--"+testBoundary) {
		t.Error("a boundary-like body did not round-trip")
	}
	// CRLF only, QP lines within 76.
	if strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\r") {
		t.Error("a bare CR or LF in the message")
	}
	for _, ln := range strings.Split(rest, "\r\n") {
		if len(ln) > 76 {
			t.Errorf("line of %d bytes: %q", len(ln), ln)
		}
	}
	for i, r := range p.rawParts {
		for _, ln := range strings.Split(r, "\r\n") {
			if len(ln) > 76 {
				t.Errorf("part %d QP line of %d bytes", i, len(ln))
			}
		}
	}
}

func TestHTMLPartHasNoCaptureOrScriptSurface(t *testing.T) {
	p := parse(t, compose(t, Input{Original: orig(), QuoteOriginal: true, Body: "- a\n- b\n\n1. c\n\nplain text"}).Raw)
	for _, bad := range []string{"<script", "<style", "<link", "<img", "<iframe", "src=", "href=", "http", "url(", "onload", "onerror"} {
		if strings.Contains(strings.ToLower(p.htm), bad) {
			t.Errorf("html contains %q: %s", bad, p.htm)
		}
	}
}

func TestMaxBodyComposesInBothParts(t *testing.T) {
	for name, body := range map[string]string{
		"one long line": strings.Repeat("a", MaxBodyBytes),
		"many lines":    strings.Repeat("0123456789 abcdefghi\n", MaxBodyBytes/21),
		"paragraphs":    strings.Repeat("para of words\n\n", MaxBodyBytes/15),
		"non-ASCII":     strings.Repeat("я", MaxBodyBytes/2),
	} {
		t.Run(name, func(t *testing.T) {
			if len(body) > MaxBodyBytes {
				t.Fatalf("test body of %d bytes is over the cap", len(body))
			}
			m := compose(t, Input{To: []string{"bob@x.test"}, Subject: "s", Body: body})
			p := parse(t, m.Raw)
			if got, want := strings.TrimRight(p.plain, "\n"), strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), "\n"); got != want {
				t.Errorf("plain differs (%d vs %d bytes)", len(got), len(want))
			}
			if !strings.HasPrefix(p.htm, "<html>") || !strings.HasSuffix(p.htm, "</html>\n") {
				t.Error("html part incomplete")
			}
		})
	}
}
