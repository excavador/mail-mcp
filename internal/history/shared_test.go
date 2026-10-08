package history

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/excavador/mail-mcp/internal/organise"
)

func idsOf(rs []Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func TestRecordAppendedByOtherStoreIsVisibleWithoutReopening(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir), open(t, dir)

	// B reads first so it has a snapshot that must be refreshed.
	if got := b.List("", 10); len(got) != 0 {
		t.Fatalf("B sees %d records before any append", len(got))
	}
	target := Record{ID: "t1", Account: "acc", Kind: "apply", Intent: &organise.Intent{Target: "Folder"}}
	if _, err := a.Append(target); err != nil {
		t.Fatal(err)
	}
	if r, ok := b.Get("t1"); !ok || r.Account != "acc" {
		t.Fatalf("B.Get = %+v, %v", r, ok)
	}
	if got := b.List("acc", 10); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("B.List = %v", idsOf(got))
	}
	if got := b.Grouped("", 10); len(got) != 1 {
		t.Fatalf("B.Grouped = %v", idsOf(got))
	}
	if applied, _ := b.TargetHistory("acc", "Folder"); !applied {
		t.Error("B.TargetHistory does not see A's apply")
	}
	if b.Undone("t1") {
		t.Fatal("B reports t1 undone before any undo")
	}
	if _, err := a.Append(Record{ID: "u1", Account: "acc", Kind: "undo", Undoes: "t1"}); err != nil {
		t.Fatal(err)
	}
	if !b.Undone("t1") {
		t.Error("B.Undone does not see A's undo")
	}
	// And the other way round: A sees what B appends.
	if _, err := b.Append(Record{ID: "t2", Account: "acc", Kind: "create_folder", Target: "New"}); err != nil {
		t.Fatal(err)
	}
	if _, created := a.TargetHistory("acc", "New"); created.IsZero() {
		t.Error("A.TargetHistory does not see B's create_folder")
	}
	if got := a.List("", 10); len(got) != 3 || got[0].ID != "t2" {
		t.Fatalf("A.List = %v", idsOf(got))
	}
}

func TestConcurrentAppendsFromTwoStoresKeepEveryRecordInOneOrder(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir), open(t, dir)
	const na, nb = 60, 40

	var wg sync.WaitGroup
	appendN := func(s *Store, prefix string, n int) {
		defer wg.Done()
		for i := range n {
			if _, err := s.Append(Record{ID: fmt.Sprintf("%s%03d", prefix, i), Account: "acc", Kind: "apply"}); err != nil {
				t.Errorf("append %s%d: %v", prefix, i, err)
				return
			}
		}
	}
	wg.Add(2)
	go appendN(a, "a", na)
	go appendN(b, "b", nb)
	wg.Wait()

	la, lb := a.List("", 1000), b.List("", 1000)
	if len(la) != na+nb || len(lb) != na+nb {
		t.Fatalf("A has %d, B has %d records, want %d", len(la), len(lb), na+nb)
	}
	ia, ib := idsOf(la), idsOf(lb)
	for i := range ia {
		if ia[i] != ib[i] {
			t.Fatalf("order differs at %d: %s vs %s", i, ia[i], ib[i])
		}
	}
	// The file: complete lines only, each a record, none corrupt.
	ls := lines(t, filepath.Join(dir, "history.jsonl"))
	if len(ls) != na+nb {
		t.Fatalf("%d lines, want %d", len(ls), na+nb)
	}
	// Each store's own records keep their own order.
	last := map[byte]string{}
	for i := len(ia) - 1; i >= 0; i-- { // oldest first
		p := ia[i][0]
		if last[p] != "" && ia[i] <= last[p] {
			t.Fatalf("%s after %s breaks per-writer order", ia[i], last[p])
		}
		last[p] = ia[i]
	}
	// A fresh open agrees.
	c := open(t, dir)
	if got := idsOf(c.List("", 1000)); fmt.Sprint(got) != fmt.Sprint(ia) {
		t.Error("a fresh Open orders records differently from the live stores")
	}
}

func TestPartialTrailingLineIsNotConsumedUntilItsNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	s := open(t, dir)
	if _, err := s.Append(Record{ID: "r1", Account: "a", Kind: "apply"}); err != nil {
		t.Fatal(err)
	}

	// Another process is mid-write: part of a line, no newline yet.
	full := goodLine("r2")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(full[:len(full)/2]); err != nil {
		t.Fatal(err)
	}
	if got := s.List("", 10); len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("partial line was consumed: %v", idsOf(got))
	}
	if _, ok := s.Get("r2"); ok {
		t.Fatal("r2 visible before its line is complete")
	}
	// Still nothing, however often it is read.
	if got := s.List("", 10); len(got) != 1 {
		t.Fatalf("second read: %v", idsOf(got))
	}
	if _, err := f.WriteString(full[len(full)/2:] + "\n"); err != nil {
		t.Fatal(err)
	}
	if r, ok := s.Get("r2"); !ok || r.Account != "a" {
		t.Fatalf("r2 not visible after its newline: %+v %v", r, ok)
	}
	if got := s.List("", 10); len(got) != 2 {
		t.Fatalf("after completion: %v", idsOf(got))
	}
}

func TestOpenRepairsCrashTruncatedLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	trunc := goodLine("r2")
	trunc = trunc[:len(trunc)-9] // cut mid-record, no newline
	if err := os.WriteFile(path, []byte(goodLine("r1")+"\n"+trunc), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	if got := s.List("", 10); len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("loaded %v, want r1 only", idsOf(got))
	}
	if _, err := s.Append(Record{ID: "r3", Account: "a", Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	// The new record is on its own line and visible; the torn line stays
	// skipped (the file is never rewritten).
	if got := idsOf(s.List("", 10)); fmt.Sprint(got) != "[r3 r1]" {
		t.Fatalf("after append: %v", got)
	}
	if got := idsOf(open(t, dir).List("", 10)); fmt.Sprint(got) != "[r3 r1]" {
		t.Fatalf("after reopen: %v", got)
	}
	if ls := lines(t, path); len(ls) != 3 {
		t.Fatalf("%d lines, want 3: %q", len(ls), ls)
	}
}

func TestOpenFoldsInOnlyCompleteLinesThenRepairsTheFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	first := goodLine("r1") + "\n"
	frag := goodLine("r2")
	frag = frag[:len(frag)-5]
	if err := os.WriteFile(path, []byte(first+frag), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	s.mu.Lock()
	off := s.off
	s.mu.Unlock()
	if off != int64(len(first)) {
		t.Fatalf("off = %d, want %d (just past the last newline)", off, len(first))
	}
	if got := idsOf(s.List("", 10)); fmt.Sprint(got) != "[r1]" {
		t.Fatalf("got %v, want [r1]", got)
	}
	// Open ended the fragment's line; a later complete record is read once.
	if _, err := s.Append(Record{ID: "r3", Account: "a", Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	if got := idsOf(s.List("", 10)); fmt.Sprint(got) != "[r3 r1]" {
		t.Fatalf("got %v, want [r3 r1]", got)
	}
}

func TestAppendKeepsItsRecordWhenReadBackFails(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Append(Record{ID: "r1", Account: "a", Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	// Make catchUp see nothing new: the offset is past the end of the file.
	s.mu.Lock()
	realOff := s.off
	s.off += 1 << 20
	s.mu.Unlock()

	if _, err := s.Append(Record{ID: "r2", Account: "a", Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	if got := idsOf(s.List("", 10)); fmt.Sprint(got) != "[r2 r1]" {
		t.Fatalf("record lost in memory: %v", got)
	}
	// Reading resumes: r2 is found in the file and must not appear twice.
	s.mu.Lock()
	s.off = realOff
	s.mu.Unlock()
	if got := idsOf(s.List("", 10)); fmt.Sprint(got) != "[r2 r1]" {
		t.Fatalf("after resuming the read: %v, want [r2 r1] (no duplicate)", got)
	}
	// Another store sees exactly the file.
	if got := idsOf(open(t, dir).List("", 10)); fmt.Sprint(got) != "[r2 r1]" {
		t.Fatalf("second store: %v", got)
	}
}
