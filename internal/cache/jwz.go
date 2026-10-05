package cache

// Message threading: the JWZ algorithm (jwz.org/doc/threading.html), the
// same one RFC 5256 describes as REFERENCES threading, as a pure function.
//
// BuildThreads takes the messages of one connected component (everything that
// shares a Message-ID, In-Reply-To or References id with another member) and
// says, for every message, which thread it is in, who its parent is and how
// deep it sits. It has no database and no clock, so it is tested on fixtures.
// The database layer (threads.go) is responsible for gathering a component
// and for re-running this function when a late message links two trees.

import (
	"crypto/sha1" //nolint:gosec // a short stable label, not a security boundary
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// ThreadMsg is the part of a message that threading looks at.
type ThreadMsg struct {
	StableID string
	// MsgID is the normalised Message-ID, "" when the message has none.
	MsgID string
	// Refs is the ancestor chain, oldest first: the References ids, then the
	// In-Reply-To id when it is not already the last of them. See threadChain.
	Refs    []string
	Subject string
	Date    int64 // unix seconds, 0 when unknown
}

// ThreadAssign is the place of one message in its thread.
type ThreadAssign struct {
	StableID string
	TID      string // "j:" + hash of the topmost container id
	Parent   string // stable id of the nearest real ancestor, "" at the top
	Depth    int    // number of real ancestors
}

// subjectWindow is how far apart two messages may be for the subject
// fallback to join them.
const subjectWindow = 30 * 24 * 3600

var (
	idTokenRE = regexp.MustCompile(`<([^<>\s]+)>`)
	// subjectPrefixRE strips reply and forward markers in the common
	// languages, repeatedly ("Re: Fwd: AW: x").
	subjectPrefixRE = regexp.MustCompile(`(?i)^\s*((re|fwd?|aw|wg|sv|vs|antw|ref|odp|rv|tr|enc)\s*(\[\d+\])?\s*:\s*)+`)
	spaceRE         = regexp.MustCompile(`\s+`)
)

// normID makes one message id comparable: no brackets, lower case. Ids with
// whitespace or nothing inside are not ids.
func normID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.ContainsAny(s, " \t\r\n<>\x00") || len(s) > 998 {
		return ""
	}
	return s
}

// idTokens returns every <id> in s, normalised, in order.
func idTokens(s string) []string {
	var out []string
	for _, m := range idTokenRE.FindAllStringSubmatch(s, -1) {
		if id := normID(m[1]); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// threadChain builds the ancestor chain from the References ids and the
// In-Reply-To header: References as written, then the first In-Reply-To id
// when it is not already the last reference. A message with only In-Reply-To
// (common for automated replies) gets a one-element chain.
func threadChain(refs []string, inReplyTo string) []string {
	out := append([]string(nil), refs...)
	irt := ""
	if t := idTokens(inReplyTo); len(t) > 0 {
		irt = t[0]
	}
	if irt != "" && (len(out) == 0 || out[len(out)-1] != irt) {
		out = append(out, irt)
	}
	return out
}

// normSubject is a subject with reply markers removed, folded and squeezed:
// what two messages of one conversation have in common.
func normSubject(s string) string {
	s = subjectPrefixRE.ReplaceAllString(s, "")
	s = strings.ToLower(spaceRE.ReplaceAllString(strings.TrimSpace(s), " "))
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}

// isReplySubject reports whether the subject carries a reply or forward marker.
func isReplySubject(s string) bool { return subjectPrefixRE.MatchString(s) }

// threadTID is the thread id for a JWZ tree whose topmost container is key.
func threadTID(key string) string {
	h := sha1.Sum([]byte(key)) //nolint:gosec
	return "j:" + hex.EncodeToString(h[:8])
}

type container struct {
	key      string
	msg      *ThreadMsg // nil: a dummy standing for a message we have not seen
	parent   *container
	children []*container
}

func link(parent, child *container) {
	child.parent = parent
	parent.children = append(parent.children, child)
}

func unlink(child *container) {
	p := child.parent
	if p == nil {
		return
	}
	for i, c := range p.children {
		if c == child {
			p.children = append(p.children[:i], p.children[i+1:]...)
			break
		}
	}
	child.parent = nil
}

// wouldLoop reports whether making child a child of parent would close a
// cycle: that is, whether child is parent or one of its ancestors.
func wouldLoop(parent, child *container) bool {
	for n, c := 0, parent; c != nil; c, n = c.parent, n+1 {
		if c == child {
			return true
		}
		if n > 1<<20 { // cannot happen while links are loop-checked; belt and braces
			return true
		}
	}
	return false
}

func topOf(c *container) *container {
	for c.parent != nil {
		c = c.parent
	}
	return c
}

// BuildThreads threads msgs. The result has one entry per input message and
// does not depend on the input order (messages are processed by date, then
// stable id).
//
// Edge cases, all covered by tests:
//   - a parent that is not in msgs becomes a dummy container, so replies to a
//     message we never received still share a thread, and the thread id does
//     not change when that message turns up;
//   - loops in References (A names B, B names A; a message naming itself;
//     the same id twice in one header) never create a cycle: a link that
//     would close one is dropped;
//   - two messages with the same Message-ID: the first owns the id, the
//     other is a separate container with its own parents, so neither is lost;
//   - a message's own References win over a parent inferred from a
//     descendant's header;
//   - subject fallback: only a message with no References and no
//     In-Reply-To whose subject carries a Re:/Fwd: marker, and only onto a
//     top-level message with the same normalised subject within 30 days.
//     Messages whose subject has no marker never join by subject, so a
//     newsletter does not become one thread.
func BuildThreads(msgs []ThreadMsg) []ThreadAssign {
	sorted := append([]ThreadMsg(nil), msgs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Date != sorted[j].Date {
			return sorted[i].Date < sorted[j].Date
		}
		return sorted[i].StableID < sorted[j].StableID
	})

	byKey := map[string]*container{}
	var all []*container
	mk := func(key string) *container {
		c := &container{key: key}
		all = append(all, c)
		return c
	}
	get := func(key string) *container {
		if c := byKey[key]; c != nil {
			return c
		}
		c := mk(key)
		byKey[key] = c
		return c
	}

	own := make([]*container, len(sorted))
	for i := range sorted {
		m := &sorted[i]
		key := m.MsgID
		if key == "" {
			key = "\x00nomid:" + m.StableID
		}
		c := byKey[key]
		switch {
		case c == nil:
			c = get(key)
		case c.msg == nil: // a dummy made by an earlier reference: now real
		default: // duplicate Message-ID
			c = mk(key + "\x00dup:" + m.StableID)
		}
		c.msg = m
		own[i] = c
	}

	for i := range sorted {
		c := own[i]
		var prev *container
		seen := map[string]bool{c.key: true, sorted[i].MsgID: true}
		for _, r := range sorted[i].Refs {
			if r == "" || seen[r] {
				continue // a message naming itself, or an id repeated in one header
			}
			seen[r] = true
			rc := get(r)
			if prev != nil && rc.parent == nil && !wouldLoop(prev, rc) {
				link(prev, rc)
			}
			prev = rc
		}
		if prev != nil && prev != c && !wouldLoop(prev, c) && c.parent != prev {
			unlink(c)
			link(prev, c)
		}
	}

	// Representative message of each top (the earliest real one in its tree),
	// for the subject fallback.
	rep := func(top *container) *ThreadMsg {
		var best *ThreadMsg
		stack := []*container{top}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n.msg != nil && (best == nil || n.msg.Date < best.Date || (n.msg.Date == best.Date && n.msg.StableID < best.StableID)) {
				best = n.msg
			}
			stack = append(stack, n.children...)
		}
		return best
	}
	var tops []*container
	repOf := map[*container]*ThreadMsg{}
	for _, c := range all {
		if c.parent == nil {
			tops = append(tops, c)
			repOf[c] = rep(c)
		}
	}
	sort.SliceStable(tops, func(i, j int) bool {
		ri, rj := repOf[tops[i]], repOf[tops[j]]
		if ri.Date != rj.Date {
			return ri.Date < rj.Date
		}
		return tops[i].key < tops[j].key
	})
	for _, t := range tops {
		m := t.msg
		if m == nil || len(m.Refs) != 0 || !isReplySubject(m.Subject) || t.parent != nil {
			continue
		}
		ns := normSubject(m.Subject)
		if ns == "" {
			continue
		}
		for _, cand := range tops {
			if cand == t {
				continue
			}
			ct := topOf(cand)
			cm := repOf[cand]
			if ct == t || cm == nil || normSubject(cm.Subject) != ns || abs64(cm.Date-m.Date) > subjectWindow {
				continue
			}
			link(ct, t)
			break
		}
	}

	var out []ThreadAssign
	for _, t := range tops {
		if t.parent != nil {
			continue // joined another tree by subject
		}
		tid := threadTID(t.key)
		type frame struct {
			c      *container
			parent string
			depth  int
		}
		stack := []frame{{t, "", 0}}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			parent, depth := f.parent, f.depth
			if f.c.msg != nil {
				out = append(out, ThreadAssign{StableID: f.c.msg.StableID, TID: tid, Parent: f.parent, Depth: f.depth})
				parent, depth = f.c.msg.StableID, f.depth+1
			}
			for _, ch := range f.c.children {
				stack = append(stack, frame{ch, parent, depth})
			}
		}
	}
	return out
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
