package cache

import (
	"math/rand"
	"sort"
	"testing"
)

func tm(id, mid string, date int64, subj string, refs ...string) ThreadMsg {
	return ThreadMsg{StableID: id, MsgID: mid, Refs: refs, Subject: subj, Date: date}
}

func byID(as []ThreadAssign) map[string]ThreadAssign {
	m := map[string]ThreadAssign{}
	for _, a := range as {
		m[a.StableID] = a
	}
	return m
}

func TestBuildThreadsSimpleChain(t *testing.T) {
	a := byID(BuildThreads([]ThreadMsg{
		tm("s1", "a", 1, "hello"),
		tm("s2", "b", 2, "Re: hello", "a"),
		tm("s3", "c", 3, "Re: hello", "a", "b"),
	}))
	if len(a) != 3 || a["s1"].TID != a["s2"].TID || a["s2"].TID != a["s3"].TID {
		t.Fatalf("not one thread: %+v", a)
	}
	if a["s1"].Depth != 0 || a["s2"].Depth != 1 || a["s3"].Depth != 2 || a["s3"].Parent != "s2" || a["s2"].Parent != "s1" || a["s1"].Parent != "" {
		t.Errorf("shape: %+v", a)
	}
}

func TestBuildThreadsOrderIndependent(t *testing.T) {
	msgs := []ThreadMsg{
		tm("s1", "a", 1, "x"),
		tm("s2", "b", 2, "Re: x", "a"),
		tm("s3", "c", 3, "Re: x", "a", "b"),
		tm("s4", "d", 4, "Re: x", "a"),
		tm("s5", "z", 5, "other"),
	}
	want := byID(BuildThreads(msgs))
	for i := 0; i < 20; i++ {
		sh := append([]ThreadMsg(nil), msgs...)
		rand.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
		got := byID(BuildThreads(sh))
		for id, w := range want {
			if got[id] != w {
				t.Fatalf("order changed the result for %s: %+v vs %+v", id, got[id], w)
			}
		}
	}
}

func TestBuildThreadsMissingParentsUseDummies(t *testing.T) {
	// Only the replies arrived; the originals "root" and "mid" never did.
	got := byID(BuildThreads([]ThreadMsg{
		tm("s1", "r1", 10, "Re: x", "root", "mid"),
		tm("s2", "r2", 11, "Re: x", "root", "mid"),
		tm("s3", "other", 12, "different"),
	}))
	if got["s1"].TID != got["s2"].TID {
		t.Fatalf("siblings of a missing parent must share a thread: %+v", got)
	}
	if got["s1"].TID == got["s3"].TID {
		t.Errorf("unrelated message joined: %+v", got)
	}
	if got["s1"].Depth != 0 || got["s1"].Parent != "" {
		t.Errorf("a dummy parent must not count as an ancestor: %+v", got["s1"])
	}
	// The tid is the same once the missing root arrives: late parent, no churn.
	late := byID(BuildThreads([]ThreadMsg{
		tm("s0", "root", 1, "x"),
		tm("s1", "r1", 10, "Re: x", "root", "mid"),
		tm("s2", "r2", 11, "Re: x", "root", "mid"),
	}))
	if late["s0"].TID != got["s1"].TID {
		t.Errorf("tid changed when the root arrived: %s -> %s", got["s1"].TID, late["s0"].TID)
	}
	if late["s1"].Parent != "" && late["s1"].Parent != "s0" {
		t.Errorf("parent after late root: %+v", late["s1"])
	}
	if late["s0"].Depth != 0 || late["s1"].Depth != 1 {
		t.Errorf("depths after late root: %+v", late)
	}
}

func TestBuildThreadsLateParentRelinksChildren(t *testing.T) {
	// B replies to A, C replies to B. Threaded without B, C hangs under the
	// dummy for B; with B it is a grandchild of A.
	before := byID(BuildThreads([]ThreadMsg{tm("a", "A", 1, "s"), tm("c", "C", 3, "Re: s", "A", "B")}))
	if before["c"].TID != before["a"].TID || before["c"].Parent != "a" || before["c"].Depth != 1 {
		t.Fatalf("before: %+v", before)
	}
	after := byID(BuildThreads([]ThreadMsg{tm("a", "A", 1, "s"), tm("b", "B", 2, "Re: s", "A"), tm("c", "C", 3, "Re: s", "A", "B")}))
	if after["c"].Parent != "b" || after["c"].Depth != 2 || after["b"].Parent != "a" {
		t.Errorf("after: %+v", after)
	}
	if after["a"].TID != before["a"].TID {
		t.Errorf("tid changed")
	}
}

func TestBuildThreadsLoopsInReferences(t *testing.T) {
	cases := map[string][]ThreadMsg{
		"mutual": {tm("s1", "a", 1, "x", "b"), tm("s2", "b", 2, "x", "a")},
		"self":   {tm("s1", "a", 1, "x", "a")},
		"repeat": {tm("s1", "a", 1, "x"), tm("s2", "b", 2, "x", "a", "c", "a", "c")},
		"three":  {tm("s1", "a", 1, "x", "c"), tm("s2", "b", 2, "x", "a"), tm("s3", "c", 3, "x", "b")},
	}
	for name, msgs := range cases {
		got := BuildThreads(msgs) // must terminate and not lose messages
		if len(got) != len(msgs) {
			t.Errorf("%s: %d assignments for %d messages", name, len(got), len(msgs))
		}
		seen := map[string]bool{}
		for _, a := range got {
			if seen[a.StableID] {
				t.Errorf("%s: %s assigned twice", name, a.StableID)
			}
			seen[a.StableID] = true
			if a.Depth > len(msgs) {
				t.Errorf("%s: depth %d", name, a.Depth)
			}
		}
	}
	// "repeat": a repeated id in one header must not break the chain a -> c -> b.
	g := byID(BuildThreads(cases["repeat"]))
	if g["s1"].TID != g["s2"].TID {
		t.Errorf("repeat: %+v", g)
	}
}

func TestBuildThreadsDuplicateMessageIDs(t *testing.T) {
	got := byID(BuildThreads([]ThreadMsg{
		tm("s1", "dup", 1, "x"),
		tm("s2", "dup", 2, "x"), // a second copy (a redelivery)
		tm("s3", "r", 3, "Re: x", "dup"),
	}))
	if len(got) != 3 {
		t.Fatalf("lost a message: %+v", got)
	}
	if got["s3"].TID != got["s1"].TID {
		t.Errorf("reply must join the first copy: %+v", got)
	}
	if got["s3"].Parent != "s1" {
		t.Errorf("reply's parent = %q, want the first copy s1", got["s3"].Parent)
	}
	// The duplicate is not dropped and is not the first copy's child.
	if got["s2"].Parent == "s1" || got["s2"].Parent == "s3" {
		t.Errorf("duplicate placed under %q", got["s2"].Parent)
	}
}

func TestBuildThreadsNoMessageID(t *testing.T) {
	got := byID(BuildThreads([]ThreadMsg{tm("s1", "", 1, "x"), tm("s2", "", 2, "x"), tm("s3", "", 3, "y")}))
	if len(got) != 3 || got["s1"].TID == got["s2"].TID {
		t.Errorf("messages without ids must not merge by id: %+v", got)
	}
}

func TestBuildThreadsSubjectFallbackIsNarrow(t *testing.T) {
	const day = 24 * 3600
	got := byID(BuildThreads([]ThreadMsg{
		tm("s1", "a", 1*day, "Quarterly plan"),
		tm("s2", "b", 3*day, "Re: Quarterly plan"),        // no headers, reply marker: joins
		tm("s3", "c", 4*day, "Quarterly plan"),            // no marker: a new topic, never joins by subject
		tm("s4", "d", 60*day, "Re: Quarterly plan"),       // outside the 30-day window
		tm("s5", "e", 5*day, "Re: Quarterly plan", "zzz"), // has a header: not subject-joined
	}))
	if got["s2"].TID != got["s1"].TID {
		t.Errorf("header-less reply did not join: %+v", got)
	}
	if got["s3"].TID == got["s1"].TID {
		t.Errorf("a message without a reply marker joined by subject: %+v", got)
	}
	if got["s4"].TID == got["s1"].TID {
		t.Errorf("joined outside the 30-day window")
	}
	if got["s5"].TID == got["s1"].TID {
		t.Errorf("a message with headers joined by subject")
	}
}

func TestNormSubjectAndChain(t *testing.T) {
	for in, want := range map[string]string{
		"Re: Fwd:  Hello   World": "hello world", "AW: x": "x", "RE[2]: y": "y", "plain": "plain", "": "",
	} {
		if got := normSubject(in); got != want {
			t.Errorf("normSubject(%q) = %q, want %q", in, got, want)
		}
	}
	got := threadChain([]string{"a", "b"}, "<b>")
	if len(got) != 2 {
		t.Errorf("in-reply-to equal to last reference duplicated: %v", got)
	}
	got = threadChain([]string{"a"}, "<B@x> trailing")
	if len(got) != 2 || got[1] != "b@x" {
		t.Errorf("chain = %v", got)
	}
	if got := threadChain(nil, ""); len(got) != 0 {
		t.Errorf("empty chain = %v", got)
	}
	ids := idTokens("<A@x>  <b@y>\n <c@z> junk <>")
	sort.Strings(ids)
	if len(ids) != 3 || ids[0] != "a@x" {
		t.Errorf("idTokens = %v", ids)
	}
}
