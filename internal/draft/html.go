package draft

// The text/html alternative of a draft. Gmail opens a text/plain-only draft in
// "Plain text mode" and hard-wraps every line at about 70 characters when it is
// sent; an HTML part lets the recipient's client reflow the text.
//
// The HTML is generated from the same input as the plain part and never taken
// from the caller: everything is escaped, and the only markup is paragraphs,
// line breaks, lists and one blockquote with an inline style. No scripts, no
// styles beyond that, no external resources.

import (
	"html"
	"regexp"
	"strings"
)

const quoteStyle = "margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex"

var orderedItemRE = regexp.MustCompile(`^\d+\. `)

// renderHTML builds the HTML document for body and, when not nil, the quoted
// original of a reply.
func renderHTML(body string, q *quoted) string {
	var b strings.Builder
	b.WriteString(`<html><body><div dir="ltr">`)
	b.WriteString(blocksHTML(body))
	if q != nil {
		b.WriteString("<div>" + esc(q.Attribution) + "</div>")
		b.WriteString(`<blockquote style="` + quoteStyle + `">`)
		lines := append([]string(nil), q.Lines...)
		if q.Cut {
			lines = append(lines, "[quote truncated]")
		}
		escaped := make([]string, len(lines))
		for i, ln := range lines {
			escaped[i] = esc(ln)
		}
		b.WriteString(strings.Join(escaped, "<br>"))
		b.WriteString("</blockquote>")
	}
	b.WriteString("</div></body></html>\n")
	return b.String()
}

func esc(s string) string { return html.EscapeString(s) }

// blocksHTML turns text into paragraphs (split on blank lines), lists and
// line breaks.
func blocksHTML(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out strings.Builder
	var para []string
	flush := func() {
		if len(para) == 0 {
			return
		}
		out.WriteString(paragraphHTML(para))
		para = nil
	}
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		para = append(para, ln)
	}
	flush()
	return out.String()
}

func paragraphHTML(lines []string) string {
	if items, ok := listItems(lines, func(l string) (string, bool) {
		if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ") {
			return l[2:], true
		}
		return "", false
	}); ok {
		return listHTML("ul", items)
	}
	if items, ok := listItems(lines, func(l string) (string, bool) {
		if loc := orderedItemRE.FindStringIndex(l); loc != nil {
			return l[loc[1]:], true
		}
		return "", false
	}); ok {
		return listHTML("ol", items)
	}
	escaped := make([]string, len(lines))
	for i, l := range lines {
		escaped[i] = esc(l)
	}
	return "<p>" + strings.Join(escaped, "<br>") + "</p>"
}

// listItems returns the items when every line is one.
func listItems(lines []string, item func(string) (string, bool)) ([]string, bool) {
	items := make([]string, 0, len(lines))
	for _, l := range lines {
		it, ok := item(l)
		if !ok {
			return nil, false
		}
		items = append(items, it)
	}
	return items, true
}

func listHTML(tag string, items []string) string {
	var b strings.Builder
	b.WriteString("<" + tag + ">")
	for _, it := range items {
		b.WriteString("<li>" + esc(it) + "</li>")
	}
	b.WriteString("</" + tag + ">")
	return b.String()
}
