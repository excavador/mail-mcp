package cache

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
)

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{"Case/Acme-1": "case/acme-1", " x ": "x", "a.b_c": "a.b_c"} {
		if got, err := NormalizeTag(in); err != nil || got != want {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	long := make([]byte, 101)
	for i := range long {
		long[i] = 'a'
	}
	for _, bad := range []string{"", "-x", "/x", "a b", "a;b", "é", string(long), "a'--"} {
		if _, err := NormalizeTag(bad); !errors.Is(err, ErrBadTag) {
			t.Errorf("NormalizeTag(%q) accepted", bad)
		}
	}
}

func TestTagsAddRemoveSearchAndUndoScoping(t *testing.T) {
	c := sndFixture(t)
	ctx := context.Background()
	ids := []string{"m1", "m2", "nope"}
	ex, err := c.ExistingIDs(ctx, "acc", ids)
	if err != nil || !slices.Equal(ex, []string{"m1", "m2"}) {
		t.Fatalf("ExistingIDs: %v %v", ex, err)
	}
	added, err := c.AddTags(ctx, "acc", "Case/X", ids, "h1")
	if err != nil || !slices.Equal(added, []string{"m1", "m2"}) {
		t.Fatalf("AddTags: %v %v", added, err)
	}
	again, _ := c.AddTags(ctx, "acc", "case/x", []string{"m1", "m3"}, "h2")
	if !slices.Equal(again, []string{"m3"}) {
		t.Errorf("second add should only add m3: %v", again)
	}
	tl, err := c.ListTags(ctx, "acc")
	if err != nil || len(tl) != 1 || tl[0].Tag != "case/x" || tl[0].Count != 3 {
		t.Errorf("ListTags: %+v %v", tl, err)
	}
	res, err := c.SearchV2(ctx, SearchOptions{SearchQuery: SearchQuery{Account: "acc", Tag: "case/x"}, GroupBy: "message"})
	if err != nil || res.Total != 3 {
		t.Errorf("search by tag: total %d, %v", res.Total, err)
	}
	got, err := c.ResolveSearch(ctx, SearchQuery{Account: "acc", Text: "hello", Tag: "case/x"}, 10)
	if err != nil || !slices.Equal(got, []string{"m3", "m1"}) {
		t.Errorf("ResolveSearch text+tag: %v %v", got, err)
	}
	// Undo of h1 removes only what h1 added (m1 stays? no: m1 was added by h1).
	rm, _ := c.RemoveTags(ctx, "acc", "case/x", []string{"m1", "m2", "m3"}, "h1")
	if !slices.Equal(rm, []string{"m1", "m2"}) {
		t.Errorf("history-scoped removal: %v", rm)
	}
	rm, _ = c.RemoveTags(ctx, "acc", "case/x", []string{"m3"}, "")
	if !slices.Equal(rm, []string{"m3"}) {
		t.Errorf("unscoped removal: %v", rm)
	}
	if _, err := c.AddTags(ctx, "acc", "bad tag", ids, ""); !errors.Is(err, ErrBadTag) {
		t.Errorf("bad tag: %v", err)
	}
	big := make([]string, MaxTagTargets+1)
	if _, err := c.AddTags(ctx, "acc", "x", big, ""); !errors.Is(err, ErrTagLimit) {
		t.Errorf("cap: %v", err)
	}
}

func TestSavedQueries(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	f := SavedFilters{Query: "invoice", From: "acme", Tag: "Case/A"}
	if err := c.SaveQuery(ctx, "acc", "Acme.Invoices", f, "n"); err != nil {
		t.Fatal(err)
	}
	q, err := c.GetSavedQuery(ctx, "acc", "acme.invoices")
	if err != nil || q.Filters.Query != "invoice" || q.Filters.Tag != "case/a" || q.Note != "n" {
		t.Errorf("get: %+v %v", q, err)
	}
	if err := c.SaveQuery(ctx, "acc", "acme.invoices", SavedFilters{Query: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if l, _ := c.ListSavedQueries(ctx, "acc"); len(l) != 1 || l[0].Filters.Query != "x" {
		t.Errorf("replace: %+v", l)
	}
	if _, err := c.GetSavedQuery(ctx, "other", "acme.invoices"); !errors.Is(err, ErrNoSavedQuery) {
		t.Errorf("account scoping: %v", err)
	}
	if err := c.SaveQuery(ctx, "acc", "bad name", f, ""); !errors.Is(err, ErrBadTag) {
		t.Errorf("name: %v", err)
	}
}
