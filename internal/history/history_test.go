package history

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/organise"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func rec(acct, kind string) Record {
	return Record{Account: acct, Kind: kind, Action: "move", Target: "X",
		Touched: map[string][]string{"INBOX": {"mid:1"}}}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestAppendWritesOneLinePerRecordAndFillsIDAndAt(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	a, err := s.Append(rec("a", KindCreateFolder))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Append(rec("a", "apply"))
	if a.ID == "" || a.At.IsZero() || a.ID == b.ID {
		t.Fatalf("ids/at not filled or not unique: %+v %+v", a, b)
	}
	ls := lines(t, filepath.Join(dir, "history.jsonl"))
	if len(ls) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(ls), ls)
	}
	for _, l := range ls {
		if strings.ContainsAny(l, "\n\r") || !strings.HasPrefix(l, "{") {
			t.Errorf("not one JSON object per line: %q", l)
		}
	}
	if got, ok := s.Get(b.ID); !ok || got.ID != b.ID {
		t.Errorf("Get = %+v %v", got, ok)
	}
	if _, ok := s.Get("nope"); ok {
		t.Error("Get found a record that does not exist")
	}
}

func TestOpenCreatesPrivateFileAndDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "h")
	s := open(t, dir)
	_, _ = s.Append(rec("a", "apply"))
	di, _ := os.Stat(dir)
	fi, _ := os.Stat(filepath.Join(dir, "history.jsonl"))
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Errorf("dir %v file %v, want 0700 / 0600", di.Mode().Perm(), fi.Mode().Perm())
	}
	// Existing loose permissions are tightened.
	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loose, "history.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	open(t, loose)
	di, _ = os.Stat(loose)
	fi, _ = os.Stat(filepath.Join(loose, "history.jsonl"))
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Errorf("existing: dir %v file %v, want 0700 / 0600", di.Mode().Perm(), fi.Mode().Perm())
	}
	if _, err := Open(""); err == nil {
		t.Error("empty dir accepted")
	}
}

func TestReopenReloadsNewestFirstAndFiltersByAccount(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, acct := range []string{"a", "b", "a", "b", "a"} {
		r := rec(acct, "apply")
		r.At = time.Unix(1000+int64(i), 0).UTC()
		got, err := s.Append(r)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, got.ID)
	}
	_ = s.Close()
	s2 := open(t, dir)
	all := s2.List("", 100)
	if len(all) != 5 {
		t.Fatalf("reloaded %d, want 5", len(all))
	}
	for i, r := range all {
		if r.ID != ids[4-i] {
			t.Errorf("position %d = %s, want newest-first %s", i, r.ID, ids[4-i])
		}
	}
	if got := s2.List("a", 100); len(got) != 3 || got[0].ID != ids[4] {
		t.Errorf("account a = %d records", len(got))
	}
	if got := s2.List("", 2); len(got) != 2 || got[0].ID != ids[4] {
		t.Errorf("limit 2 = %d records", len(got))
	}
	if got := s2.List("zzz", 5); len(got) != 0 {
		t.Errorf("unknown account listed %d", len(got))
	}
	if r, ok := s2.Get(ids[1]); !ok || len(r.Touched["INBOX"]) != 1 || r.Touched["INBOX"][0] != "mid:1" {
		t.Errorf("touched lost across reopen: %+v", r)
	}
}

func goodLine(id string) string {
	return `{"id":"` + id + `","at":"2026-01-01T00:00:00Z","account":"a","kind":"apply","preview":{"matched":0,"sampled":0}}`
}

func TestLoadSkipsCorruptEmptyIDAndOversizeLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	huge := strings.Repeat("x", maxLineBytes+10)
	content := goodLine("r1") + "\n" +
		"{not json\n" +
		`{"account":"a","kind":"apply"}` + "\n" + // valid JSON, no id
		"\n" +
		`{"id":"` + huge + `"}` + "\n" +
		goodLine("r2") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	got := s.List("", 10)
	if len(got) != 2 || got[0].ID != "r2" || got[1].ID != "r1" {
		t.Fatalf("loaded %+v, want r2 and r1 only", got)
	}
}

func TestMissingFinalNewlineDoesNotGlueRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	if err := os.WriteFile(path, []byte(goodLine("r1")+"\n"+goodLine("r2")), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(Record{ID: "r3", Account: "a", Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	ls := lines(t, path)
	if len(ls) != 3 {
		t.Fatalf("%d lines, want 3 (records glued?): %q", len(ls), ls)
	}
	s2 := open(t, dir)
	got := s2.List("", 10)
	if len(got) != 3 || got[0].ID != "r3" || got[1].ID != "r2" || got[2].ID != "r1" {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestFileOnlyGrowsAcrossAppendsAndReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	// Even a file full of garbage is never truncated or rewritten.
	garbage := "garbage line\n" + goodLine("r1")
	if err := os.WriteFile(path, []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	last := int64(0)
	for i := range 4 {
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := os.Stat(path); st.Size() < last {
			t.Fatalf("reopen %d shrank the file: %d < %d", i, st.Size(), last)
		}
		if _, err := s.Append(rec("a", "apply")); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(path)
		if st.Size() <= last {
			t.Fatalf("append %d did not grow the file: %d <= %d", i, st.Size(), last)
		}
		last = st.Size()
		_ = s.Close()
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), garbage) {
		t.Error("the original bytes were rewritten")
	}
}

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + strings.Repeat("0", 8) + strconv.Itoa(i)
	}
	return out
}

func TestAppendGroupSplitsLargeRecordsAndGetMergesThem(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Account: "a", Kind: "apply", Action: "move", Intent: &organise.Intent{Target: "X"},
		Touched:         map[string][]string{"INBOX": ids("mid:", 45000)},
		AlreadyInTarget: map[string][]string{"INBOX": ids("aid:", 10)}}
	parts, err := s.AppendGroup(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("%d parts, want 3", len(parts))
	}
	group := parts[0].ID
	for i, p := range parts {
		if p.Group != group || p.Part != i+1 || p.Parts != 3 {
			t.Errorf("part %d = group %q part %d/%d", i, p.Group, p.Part, p.Parts)
		}
	}
	path := filepath.Join(dir, "history.jsonl")
	ls := lines(t, path)
	if len(ls) != 3 {
		t.Fatalf("%d lines, want 3", len(ls))
	}
	for _, l := range ls {
		if len(l) > MaxRecordBytes {
			t.Errorf("a part is %d bytes", len(l))
		}
	}
	check := func(s *Store, id string) {
		t.Helper()
		got, ok := s.Get(id)
		if !ok {
			t.Fatalf("Get(%s) not found", id)
		}
		if got.ID != group || len(got.Touched["INBOX"]) != 45000 || len(got.AlreadyInTarget["INBOX"]) != 10 {
			t.Errorf("merged = id %s touched %d already %d", got.ID, len(got.Touched["INBOX"]), len(got.AlreadyInTarget["INBOX"]))
		}
		seen := map[string]bool{}
		for _, x := range got.Touched["INBOX"] {
			if seen[x] {
				t.Fatalf("id %s twice", x)
			}
			seen[x] = true
		}
	}
	check(s, parts[0].ID)
	check(s, parts[2].ID) // any part names the whole apply
	if g := s.Grouped("a", 10); len(g) != 1 || g[0].ID != group || len(g[0].Touched["INBOX"]) != 45000 {
		t.Errorf("Grouped = %d entries", len(g))
	}
	if l := s.List("a", 10); len(l) != 3 {
		t.Errorf("List = %d lines", len(l))
	}
	// Undone is by group id, whichever part is named.
	if s.Undone(group) || s.Undone(parts[1].ID) {
		t.Error("Undone before any undo")
	}
	if _, err := s.Append(Record{Account: "a", Kind: "undo", Undoes: group}); err != nil {
		t.Fatal(err)
	}
	if !s.Undone(group) || !s.Undone(parts[1].ID) {
		t.Error("Undone false after an undo of the group")
	}
	_ = s.Close()
	s2 := open(t, dir)
	check(s2, parts[1].ID)
	if g := s2.Grouped("a", 10); len(g) != 2 {
		t.Errorf("Grouped after reopen = %d, want group + undo", len(g))
	}
}

func TestAppendGroupKeepsSmallRecordsWhole(t *testing.T) {
	s := open(t, t.TempDir())
	parts, err := s.AppendGroup(Record{Account: "a", Kind: "apply", Touched: map[string][]string{"INBOX": ids("mid:", 20000)}})
	if err != nil || len(parts) != 1 || parts[0].Group != "" || parts[0].Parts != 0 {
		t.Fatalf("parts = %+v, %v", parts, err)
	}
	parts, err = s.AppendGroup(Record{Account: "a", Kind: "apply", Touched: map[string][]string{"INBOX": ids("mid:", 20001)}})
	if err != nil || len(parts) != 2 {
		t.Fatalf("20001 ids: %d parts, %v", len(parts), err)
	}
}

func TestAppendRefusesRecordsOverMaxRecordBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	s := open(t, dir)
	if _, err := s.Append(rec("a", "apply")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	big := rec("a", "apply")
	big.Touched = map[string][]string{"INBOX": {strings.Repeat("x", MaxRecordBytes+1)}}
	if _, err := s.Append(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("a refused record changed the file")
	}
	if n := len(s.List("", 10)); n != 1 {
		t.Errorf("refused record was remembered: %d", n)
	}
	// Just under the limit is written and read back after a reopen.
	ok := rec("a", "apply")
	ok.Touched = map[string][]string{"INBOX": {strings.Repeat("x", MaxRecordBytes-1024)}}
	if _, err := s.Append(ok); err != nil {
		t.Fatalf("near-limit record refused: %v", err)
	}
	_ = s.Close()
	if n := len(open(t, dir).List("", 10)); n != 2 {
		t.Errorf("near-limit record lost on reload: %d records", n)
	}
}

func TestTargetHistory(t *testing.T) {
	s := open(t, t.TempDir())
	if applied, created := s.TargetHistory("a", "X"); applied || !created.IsZero() {
		t.Fatal("fresh store reports history")
	}
	_, _ = s.Append(Record{Account: "a", Kind: KindCreateFolder, Target: "X"})
	_, _ = s.Append(Record{Account: "a", Kind: "apply", Intent: &organise.Intent{Target: "Y"}, Error: "boom"})
	_, _ = s.Append(Record{Account: "b", Kind: "apply", Intent: &organise.Intent{Target: "X"}})
	applied, created := s.TargetHistory("a", "X")
	if applied || created.IsZero() {
		t.Errorf("X: applied %v created %v, want only created", applied, created)
	}
	if applied, _ := s.TargetHistory("a", "Y"); applied {
		t.Error("a failed apply counts as having used the target")
	}
	if applied, _ := s.TargetHistory("b", "X"); !applied {
		t.Error("account b applied to X")
	}
}
