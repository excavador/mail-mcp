package server

import (
	"strings"
	"unicode"
)

// Everything the read tools return from a message was written by a third
// party, and reaches a model. These helpers remove what has no honest use in
// it and could hide or reorder text: control characters, Unicode format
// characters (zero-width characters, bidi overrides and isolates, BOM, soft
// hyphen) and invalid UTF-8.

const (
	maxFieldRunes = 512  // one header-like field
	maxListRunes  = 4096 // an address list (already capped at 50 entries)
)

// clean sanitises one header-like value. Tab is kept; line breaks and other
// vertical whitespace become a space, so a header cannot span lines.
func clean(s string) string { return sanitize(s, false) }

// cleanBody sanitises a message body. Tab and newline are kept; CR is
// dropped (CRLF becomes LF); other vertical whitespace becomes a space.
func cleanBody(s string) string { return sanitize(s, true) }

func sanitize(s string, body bool) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteRune(r)
		case r == '\n':
			if body {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		case r == '\r':
			if !body {
				b.WriteByte(' ')
			}
		case r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			b.WriteByte(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// capRunes truncates s to n runes (on a rune boundary), marking the cut.
func capRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos] + "…"
		}
		i++
	}
	return s
}

// field is clean followed by the per-field cap.
func field(s string) string { return capRunes(clean(s), maxFieldRunes) }

// fieldAll is field for each element.
func fieldAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = field(s)
	}
	return out
}

// list is field for an address list, which may legitimately be longer.
func list(s string) string { return capRunes(clean(s), maxListRunes) }
