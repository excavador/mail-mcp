package cache

import (
	"regexp"
	"strings"
)

// ForwardedMarker replaces the header block of a forwarded message in
// CleanBody's output, so the boundary stays visible and the forwarded content
// itself is kept.
const ForwardedMarker = "[forwarded message]"

// minTrivialBody is the size below which CleanBody never falls back on the
// "removed nearly everything" rule: a one-line reply over a long quote is
// legitimately short.
const minTrivialBody = 200

// keepFraction is the share of a non-trivial body that must survive stripping.
const keepFraction = 0.05

var (
	// Reply headers. Gmail wraps the line, so the matcher is given up to
	// three consecutive lines joined by spaces and must match all of them.
	replyHeaderRE = regexp.MustCompile(`(?is)^(on|am|op|el|le|il|em|den|w dniu|dne)\s.{3,400}?\s(wrote|schrieb|schreef|écrit|escribió|escribio|scritto|skrev|napisał|napisała|escreveu)(\s.{0,200}<[^<>]*@[^<>]*>)?\s*:\s*$`)
	// Russian: "… пишет:", "… написал(а):", and Gmail's "пн, 1 янв. 2024 г. в 10:00, Имя <a@b>:".
	replyHeaderRU = regexp.MustCompile(`(?i)^.{3,400}?\s(пишет|написал|написала|написал\(а\))\s*:\s*$`)
	replyHeaderRD = regexp.MustCompile(`(?i)^(пн|вт|ср|чт|пт|сб|вс)[,.]\s.{3,300}<[^<>@\s]+@[^<>\s]+>\s*:\s*$`)

	// Outlook / client separators that open a quoted original.
	originalRE    = regexp.MustCompile(`(?i)^[\s_\-=]*(original message|original-nachricht|urspr[uü]ngliche nachricht|oorspronkelijk bericht|oorspronkelijke bericht|message d'origine|mensaje original|messaggio originale|исходное сообщение|первоначальное сообщение)[\s_\-=]*$`)
	underscoresRE = regexp.MustCompile(`^_{20,}\s*$`)

	// Forwarded markers; the content that follows is kept.
	forwardedRE = regexp.MustCompile(`(?i)^[\s\-=_]*(forwarded message|begin forwarded message:?|weitergeleitete nachricht|doorgestuurd bericht|doorgestuurd:|message transf[eé]r[eé]|mensaje reenviado|пересылаемое сообщение|пересланное сообщение)[\s\-=_:]*$`)

	// "From: x" style block opener and the other header keys of such a block.
	blockFromRE = regexp.MustCompile(`(?i)^\**(from|von|van|de|от)\**\s*:\s*\S`)
	blockKeyRE  = regexp.MustCompile(`(?i)^\**(sent|date|datum|gesendet|verzonden|verstuurd|отправлено|дата|to|an|aan|кому|subject|betreff|onderwerp|тема|cc|kopie|копия)\**\s*:`)

	mobileSigRE = regexp.MustCompile(`(?i)^(sent from my [\w ()+.\-]{1,40}|sent from (mail|outlook|yahoo mail|proton ?mail|spark|my mobile)\b.{0,40}|sent with (proton ?mail|spark|superhuman)\b.{0,60}|get outlook for (ios|android)\b.{0,20}|gesendet von meinem [\w ()+.\-]{1,40}|gesendet mit der .{0,40}app|von meinem [\w ()+.\-]{1,40} gesendet|verzonden vanaf mijn [\w ()+.\-]{1,40}|verstuurd vanaf mijn [\w ()+.\-]{1,40}|verzonden met .{0,40}|envoy[eé] de mon [\w ()+.\-]{1,40}|enviado desde mi [\w ()+.\-]{1,40}|отправлено с (моего )?[\w ()+.\-]{1,40}|отправлено из .{1,40}|отправлено с устройства .{1,40})\s*\.?\s*$`)
)

func isReplyHeader(s string) bool {
	return replyHeaderRE.MatchString(s) || replyHeaderRU.MatchString(s) || replyHeaderRD.MatchString(s)
}

// replyHeaderAt reports how many lines (1-3) starting at i form a reply
// header, or 0.
func replyHeaderAt(lines []string, i int) int {
	for n := 1; n <= 3 && i+n <= len(lines); n++ {
		parts := make([]string, 0, n)
		for _, l := range lines[i : i+n] {
			t := strings.TrimSpace(l)
			if t == "" {
				break // a header never spans a blank line
			}
			parts = append(parts, t)
		}
		if len(parts) != n {
			return 0
		}
		if isReplyHeader(strings.Join(parts, " ")) {
			return n
		}
	}
	return 0
}

// headerBlockAt reports the length of an Outlook-style "From: .. Sent: .."
// block starting at line i (0 when there is none): a From line followed by at
// least two more header-key lines inside the next five.
func headerBlockAt(lines []string, i int) int {
	if !blockFromRE.MatchString(strings.TrimSpace(lines[i])) {
		return 0
	}
	keys, last := 0, 0
	for j := i + 1; j < len(lines) && j <= i+6; j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			break
		}
		if blockKeyRE.MatchString(t) {
			keys++
			last = j
		}
	}
	if keys < 2 {
		return 0
	}
	return last - i + 1
}

func isSigDelimiter(l string) bool {
	return l == "-- " || l == "--" || strings.TrimRight(l, " \t") == "--"
}

// CleanBody strips the parts of a message body that repeat other messages:
// quoted lines, reply headers with everything after them, Outlook-style
// original-message blocks, signatures after "-- " and common mobile
// signatures. A forwarded message keeps its content; only its header block is
// replaced by ForwardedMarker.
//
// It is conservative. If nothing would remain, or a non-trivial body would
// lose more than 95% of its text, the original (trimmed) is returned, since a
// message whose only content is a forward or a bottom-posted answer must stay
// findable.
func CleanBody(text string) string {
	orig := strings.TrimSpace(text)
	if orig == "" {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	seenContent := false // any non-blank line kept so far
	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\r")
		t := strings.TrimSpace(l)

		if isSigDelimiter(l) {
			break
		}
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), ">") {
			continue
		}
		if n := replyHeaderAt(lines, i); n > 0 {
			break
		}
		if originalRE.MatchString(t) {
			// "-----Original Message-----" is followed by From:/Sent: lines
			// (or by nothing); a forward at the very top of the text is
			// content, anything after text of ours is a quote.
			if !seenContent {
				out = append(out, ForwardedMarker)
				i += skipHeaderBlock(lines, i+1)
				seenContent = true
				continue
			}
			break
		}
		if underscoresRE.MatchString(t) && i+1 < len(lines) && headerBlockAt(lines, i+1) > 0 {
			if !seenContent {
				out = append(out, ForwardedMarker)
				i += headerBlockAt(lines, i+1)
				seenContent = true
				continue
			}
			break
		}
		if forwardedRE.MatchString(t) {
			out = append(out, ForwardedMarker)
			i += skipHeaderBlock(lines, i+1)
			seenContent = true
			continue
		}
		if n := headerBlockAt(lines, i); n > 0 {
			if !seenContent {
				out = append(out, ForwardedMarker)
				i += n - 1
				seenContent = true
				continue
			}
			break
		}
		if mobileSigRE.MatchString(t) {
			continue
		}
		if t != "" {
			seenContent = true
		}
		out = append(out, l)
	}
	cleaned := strings.TrimSpace(collapseBlank(strings.Join(out, "\n")))
	if cleaned == "" {
		return orig
	}
	if len(orig) >= minTrivialBody && float64(len(cleaned)) < keepFraction*float64(len(orig)) {
		return orig
	}
	return cleaned
}

// skipHeaderBlock returns how many lines from i belong to the header block
// that follows a forward marker: consecutive "Key: value" lines (after at most
// one blank line), including wrapped continuations. Content starts after it.
func skipHeaderBlock(lines []string, i int) int {
	n := 0
	for i+n < len(lines) && strings.TrimSpace(lines[i+n]) == "" && n < 1 {
		n++
	}
	for i+n < len(lines) {
		t := strings.TrimSpace(lines[i+n])
		if t == "" {
			break
		}
		if !blockFromRE.MatchString(t) && !blockKeyRE.MatchString(t) {
			break
		}
		n++
	}
	return n
}

var multiBlank = regexp.MustCompile(`\n{3,}`)

func collapseBlank(s string) string { return multiBlank.ReplaceAllString(s, "\n\n") }
