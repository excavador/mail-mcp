package cache

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type qaLog struct {
	t   *testing.T
	c   *Cache
	now time.Time
}

func newQALog(t *testing.T) *qaLog {
	c := thrOpen(t)
	l := &qaLog{t: t, c: c, now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	c.now = func() time.Time { return l.now }
	return l
}

func (l *qaLog) search(q string, page bool, refs ...ResultRef) int64 {
	l.t.Helper()
	id, err := l.c.LogSearch(context.Background(), SearchRecord{Account: "p", Query: q, Key: QueryKey("p", q), Mode: "thread", Page: page, Hits: len(refs), Total: len(refs), Refs: refs})
	if err != nil {
		l.t.Fatal(err)
	}
	return id
}

func (l *qaLog) fetched(id int64) int {
	return thrCount(l.t, l.c, `SELECT fetched FROM search_log WHERE id = ?`, id)
}

func (l *qaLog) reform(id int64) int64 {
	var r *int64
	_ = l.c.db.QueryRow(`SELECT reformulated_of FROM search_log WHERE id = ?`, id).Scan(&r)
	if r == nil {
		return 0
	}
	return *r
}

func TestQAEverySearchCallWritesARow(t *testing.T) {
	l := newQALog(t)
	l.search("alpha", false, ResultRef{"p", "pm:1", "j:1"})
	l.search("alpha", false) // repeat
	l.search("alpha", true)  // page
	l.search("alpha", true)
	l.search("zzz", false)
	if n := thrCount(t, l.c, `SELECT COUNT(*) FROM search_log`); n != 5 {
		t.Errorf("%d rows for 5 calls", n)
	}
	if n := thrCount(t, l.c, `SELECT COUNT(*) FROM search_log WHERE mode = 'thread:page'`); n != 2 {
		t.Errorf("%d page rows, want 2", n)
	}
	// pages never enter the ring
	if n := len(l.c.searches.ring); n != 3 {
		t.Errorf("ring %d, want 3 (pages are not ring entries)", n)
	}
}

func TestQAFetchWindowBoundary(t *testing.T) {
	l := newQALog(t)
	ctx := context.Background()
	ref := func(i int) ResultRef { return ResultRef{"p", fmt.Sprintf("pm:%d", i), fmt.Sprintf("j:%d", i)} }
	a := l.search("one", false, ref(1))
	b := l.search("two", false, ref(2))
	c := l.search("three", false, ref(3))
	l.now = l.now.Add(fetchWindow) // exactly 10 minutes
	l.c.NoteFetch(ctx, "p", "j:1")
	l.now = l.now.Add(time.Second) // 10 minutes and a second
	l.c.NoteFetch(ctx, "p", "j:2")
	l.c.NoteFetch(ctx, "p", "", "pm:3")
	if l.fetched(a) != 1 {
		t.Error("a fetch at exactly 10 minutes should count (window is inclusive)")
	}
	if l.fetched(b) != 0 || l.fetched(c) != 0 {
		t.Errorf("a fetch after 10 minutes marked a search: b=%d c=%d", l.fetched(b), l.fetched(c))
	}
	// A fetch of something no search returned marks nothing; another account's tid too.
	d := l.search("four", false, ref(4))
	l.c.NoteFetch(ctx, "q", "j:4")
	l.c.NoteFetch(ctx, "p", "j:999")
	if l.fetched(d) != 0 {
		t.Error("unrelated fetch marked a search")
	}
	// A page's results mark the original search.
	e := l.search("five", false, ref(5))
	l.search("five", true, ref(6))
	l.c.NoteFetch(ctx, "p", "j:6")
	if l.fetched(e) != 1 {
		t.Error("fetching a result from page 2 must mark the search")
	}
}

func TestQAReformulationWindowAndRepeats(t *testing.T) {
	l := newQALog(t)
	s1 := l.search("billing statement", false)
	l.now = l.now.Add(reformulateWindow) // exactly 2 minutes
	s2 := l.search("billing statement march", false)
	if l.reform(s2) != s1 {
		t.Errorf("at exactly 2 minutes: reformulated_of = %d, want %d", l.reform(s2), s1)
	}
	l.now = l.now.Add(reformulateWindow + time.Second)
	s3 := l.search("billing statement april", false)
	if l.reform(s3) != 0 {
		t.Errorf("after 2m01s: reformulated_of = %d", l.reform(s3))
	}
	// A different topic is not a reformulation.
	l.now = l.now.Add(10 * time.Second)
	s4 := l.search("holiday photos", false)
	if l.reform(s4) != 0 {
		t.Error("unrelated query counted as reformulation")
	}
	// A repeat of the same query is not; neither are its pages.
	s5 := l.search("holiday photos", false)
	p := l.search("holiday photos", true)
	if l.reform(s5) != 0 || l.reform(p) != 0 {
		t.Error("repeat or page counted as reformulation")
	}
	// A previous search that was read is not abandoned: a follow-up is not a reformulation of it.
	l.now = l.now.Add(10 * time.Minute)
	s6 := l.search("tax return", false, ResultRef{"p", "pm:t", "j:t"})
	l.c.NoteFetch(context.Background(), "p", "j:t")
	l.now = l.now.Add(10 * time.Second)
	s7 := l.search("tax return 2025", false)
	if l.reform(s7) != 0 {
		t.Errorf("follow-up to a fetched search reformulated_of %d (s6 %d)", l.reform(s7), s6)
	}
}

func TestQARingCappedAtFiftyAndPagesDoNotEvictSearches(t *testing.T) {
	l := newQALog(t)
	for i := 0; i < 120; i++ {
		l.search(fmt.Sprintf("q%d words", i), false, ResultRef{"p", fmt.Sprintf("pm:%d", i), ""})
		if i%3 == 0 {
			l.search(fmt.Sprintf("q%d words", i), true)
		}
		l.now = l.now.Add(time.Second)
		if n := len(l.c.searches.ring); n > searchRingSize {
			t.Fatalf("ring grew to %d", n)
		}
	}
	if n := len(l.c.searches.ring); n != 50 {
		t.Errorf("ring = %d, want 50", n)
	}
	// The oldest survivor is search 70; a fetch of search 69's result marks nothing, of 70's does.
	l.c.NoteFetch(context.Background(), "p", "", "pm:69")
	l.c.NoteFetch(context.Background(), "p", "", "pm:70")
	if n := thrCount(t, l.c, `SELECT COUNT(*) FROM search_log WHERE fetched = 1`); n != 1 {
		t.Errorf("%d searches marked fetched, want exactly the oldest ring entry", n)
	}
	// The table itself keeps every row (the ring only bounds memory).
	if n := thrCount(t, l.c, `SELECT COUNT(*) FROM search_log WHERE mode = 'thread'`); n != 120 {
		t.Errorf("%d search rows", n)
	}
}

// qaStatsLog logs `abandoned` abandon-and-reformulate pairs and fills up to
// total searches with ones that were read.
func qaStatsLog(t *testing.T, abandoned, total int) SearchStats {
	t.Helper()
	l := newQALog(t)
	n := 0
	for i := 0; i < abandoned; i++ {
		l.now = l.now.Add(5 * time.Minute)
		l.search(fmt.Sprintf("alpha%d beta%d", i, i), false)
		l.now = l.now.Add(10 * time.Second)
		id := l.search(fmt.Sprintf("alpha%d beta%d gamma%d", i, i, i), false)
		l.c.NoteFetch(context.Background(), "p", "", "none")                     // nothing
		_, _ = l.c.db.Exec(`UPDATE search_log SET fetched = 1 WHERE id = ?`, id) // the reformulation itself was read
		n += 2
	}
	for ; n < total; n++ {
		l.now = l.now.Add(5 * time.Minute)
		q := fmt.Sprintf("solo%dzz", n)
		l.search(q, false, ResultRef{"p", fmt.Sprintf("pm:%d", n), ""})
		l.c.NoteFetch(context.Background(), "p", "", fmt.Sprintf("pm:%d", n))
	}
	// pages never count
	l.search("alpha0 beta0", true)
	st, err := l.c.SearchStatsSince(context.Background(), 14)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestQAStatsVerdictNeedsTwentySearches(t *testing.T) {
	st := qaStatsLog(t, 5, 19)
	if st.Searches != 19 || st.VectorSearch || st.Verdict == "" || st.AbandonedRe != 5 {
		t.Fatalf("19 searches: %+v", st)
	}
	if !contains(st.Verdict, "insufficient") {
		t.Errorf("verdict with 19 searches = %q", st.Verdict)
	}
	st = qaStatsLog(t, 5, 20)
	if st.Searches != 20 || contains(st.Verdict, "insufficient") || !st.VectorSearch {
		t.Errorf("20 searches, 25%% abandoned: %+v", st)
	}
}

func TestQAStatsTriggerIsAboveTenPercent(t *testing.T) {
	st := qaStatsLog(t, 2, 20) // 2/20 = exactly 10%
	if st.AbandonedRe != 2 || st.Searches != 20 || st.VectorSearch || contains(st.Verdict, "insufficient") {
		t.Errorf("exactly 10%%: %+v", st)
	}
	st = qaStatsLog(t, 3, 20) // 15%
	if !st.VectorSearch {
		t.Errorf("15%%: %+v", st)
	}
	st = qaStatsLog(t, 0, 25)
	if st.VectorSearch || st.AbandonedRe != 0 || st.ZeroFetch != 0 {
		t.Errorf("all read: %+v", st)
	}
}

func TestQAStatsZeroFetchAloneDoesNotTrigger(t *testing.T) {
	l := newQALog(t)
	// 30 distinct searches, none fetched, none reformulated (distinct words, spaced out).
	for i := 0; i < 30; i++ {
		l.now = l.now.Add(5 * time.Minute)
		l.search(fmt.Sprintf("topic%dxx", i), false, ResultRef{"p", fmt.Sprintf("pm:%d", i), ""})
	}
	st, _ := l.c.SearchStatsSince(context.Background(), 14)
	if st.Searches != 30 || st.ZeroFetch != 30 || st.Reformulate != 0 || st.VectorSearch {
		t.Errorf("zero fetch with no reformulation must not trigger: %+v", st)
	}
	// Reformulation of searches that WERE read does not trigger either.
	l2 := newQALog(t)
	for i := 0; i < 30; i += 2 {
		l2.now = l2.now.Add(5 * time.Minute)
		l2.search(fmt.Sprintf("alpha%d beta%d", i, i), false, ResultRef{"p", fmt.Sprintf("pm:%d", i), ""})
		l2.c.NoteFetch(context.Background(), "p", "", fmt.Sprintf("pm:%d", i))
		l2.now = l2.now.Add(10 * time.Second)
		l2.search(fmt.Sprintf("alpha%d beta%d more", i, i), false)
	}
	st, _ = l2.c.SearchStatsSince(context.Background(), 14)
	if st.Reformulate != 0 || st.VectorSearch {
		t.Errorf("follow-ups to read searches: %+v", st)
	}
	// Old rows fall outside the window.
	l.now = l.now.Add(15 * 24 * time.Hour)
	st, _ = l.c.SearchStatsSince(context.Background(), 14)
	if st.Searches != 0 {
		t.Errorf("rows older than 14 days counted: %+v", st)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
