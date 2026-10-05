// Package history is the append-only record of what mail-mcp changed in a
// mailbox: one JSON line per create_folder, apply, undo or reapply.
//
// Unlike the cache, history cannot be rebuilt from the mailbox. Undo needs
// the stable ids an apply touched and where each came from, and nothing else
// holds that. So the file is only ever appended to: it is never truncated,
// rewritten or compacted by this package, and a corrupt line is skipped, not
// repaired. Backing it up is a deployment concern.
package history

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/excavador/mail-mcp/internal/organise"
)

// Kinds.
const KindCreateFolder = "create_folder"

const (
	// maxLineBytes bounds one line when loading; a line beyond it is skipped
	// and logged.
	maxLineBytes = 16 << 20
	// MaxRecordBytes is the largest record Append writes. It is far below
	// maxLineBytes, so a record that was written can always be read back.
	MaxRecordBytes = 4 << 20
	// partIDs is how many stable ids (touched plus already-in-target) one
	// record of a group carries: at about 100 bytes each, well under
	// MaxRecordBytes.
	partIDs = 20000
)

// ErrTooLarge is Append refusing a record over MaxRecordBytes.
var ErrTooLarge = errors.New("history: record too large")

// Approval channels recorded in PreviewInfo.ApprovedBy.
const (
	ApprovedElicitation = "elicitation"
	ApprovedClientTool  = "client-tool-approval"
)

// PreviewInfo is what the owner was shown and how they approved it.
type PreviewInfo struct {
	Matched    int    `json:"matched"`
	Sampled    int    `json:"sampled"`
	ApprovedBy string `json:"approved_by,omitempty"`
}

// Record is one history line.
type Record struct {
	ID      string           `json:"id"`
	At      time.Time        `json:"at"`
	Account string           `json:"account"`
	Kind    string           `json:"kind"` // create_folder, apply, undo or reapply
	Intent  *organise.Intent `json:"intent,omitempty"`
	Action  string           `json:"action,omitempty"`
	Target  string           `json:"target,omitempty"` // create_folder: the new folder
	Preview PreviewInfo      `json:"preview"`
	// Touched is the stable ids acted on, grouped by the folder they came from.
	Touched map[string][]string `json:"touched,omitempty"`
	// AlreadyInTarget lists, by source folder, acted-on ids that were in the
	// target before. They are not in Touched; undo restores their source by
	// COPY (keeping the target they had) instead of moving them back.
	AlreadyInTarget map[string][]string `json:"already_in_target,omitempty"`
	// CopiedBack (undo records only): ids COPYed back into the folder they
	// had been moved out of, by folder.
	CopiedBack map[string][]string `json:"copied_back,omitempty"`
	Skipped    int                 `json:"skipped,omitempty"`
	// Group, Part and Parts tie together the records of one apply too large
	// for a single line. Group is the id of part 1; both are zero otherwise.
	Group string `json:"group,omitempty"`
	Part  int    `json:"part,omitempty"`
	Parts int    `json:"parts,omitempty"`
	// Error is a fixed string, set when the operation stopped part-way (the
	// progress made is in Touched).
	Error     string `json:"error,omitempty"`
	Undoes    string `json:"undoes,omitempty"`
	Reapplies string `json:"reapplies,omitempty"`
}

// Store is the history file plus an in-memory copy of its records.
type Store struct {
	mu   sync.Mutex
	f    *os.File
	recs []Record
}

// Open creates dir (0700) and the file (0600) if needed, loads every record
// and opens the file for appending.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("history: directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("history: create directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("history: restrict directory: %w", err)
	}
	path := filepath.Join(dir, "history.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("history: open: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("history: restrict file: %w", err)
	}
	s := &Store{f: f}
	s.recs = load(f)
	// A crash mid-write can leave a last line without its newline; the next
	// append would glue a record onto it and lose both. Start a fresh line.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("history: repair line end: %w", err)
			}
		}
	}
	return s, nil
}

// load parses the file from the start, skipping and logging lines that are
// too long or not a record.
func load(f *os.File) []Record {
	var out []Record
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, 1<<62), 64<<10)
	for n := 1; ; n++ {
		line, tooLong, err := readLine(r)
		if len(line) > 0 || tooLong {
			var rec Record
			switch {
			case tooLong:
				slog.Warn("history: skipping oversize line", "line", n)
			case json.Unmarshal(line, &rec) != nil || rec.ID == "":
				slog.Warn("history: skipping corrupt line", "line", n)
			default:
				out = append(out, rec)
			}
		}
		if err != nil {
			return out
		}
	}
}

// readLine reads one line of at most maxLineBytes; a longer line is consumed
// to its end and reported as tooLong.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, e := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxLineBytes {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		return line, tooLong, e
	}
}

// newID is time-ordered: nanoseconds since the epoch, then four random bytes
// so two records in one instant differ.
func newID() string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(rnd[:]))
}

// Append writes r as one line, syncs it to disk and only then remembers it.
// It fills in ID and At when empty. The returned record carries them.
func (s *Store) Append(r Record) (Record, error) {
	if r.ID == "" {
		r.ID = newID()
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	line, err := json.Marshal(r)
	if err != nil {
		return r, fmt.Errorf("history: encode: %w", err)
	}
	if len(line) > MaxRecordBytes {
		return r, ErrTooLarge
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(line); err != nil { // O_APPEND: one write, at the end
		// A partial line may be on disk. End it, so the next record starts on
		// a line of its own, and fail: the file is only ever appended to.
		_, _ = s.f.Write([]byte{'\n'})
		return r, fmt.Errorf("history: write: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return r, fmt.Errorf("history: sync: %w", err)
	}
	s.recs = append(s.recs, r)
	return r, nil
}

// Group turns an outcome's touched list into the stored form.
func Group(ts []organise.Touched) map[string][]string {
	if len(ts) == 0 {
		return nil
	}
	m := map[string][]string{}
	for _, t := range ts {
		m[t.FromFolder] = append(m[t.FromFolder], t.StableID)
	}
	return m
}

// AppendGroup appends r, split into continuation records sharing a group id
// when its id lists are too long for one line. It returns the records
// written; the first one's id names the whole apply. On error, parts already
// written stay (history is append-only) and are returned.
func (s *Store) AppendGroup(r Record) ([]Record, error) {
	lists := []*map[string][]string{&r.Touched, &r.AlreadyInTarget, &r.CopiedBack}
	n := 0
	for _, l := range lists {
		for _, ids := range *l {
			n += len(ids)
		}
	}
	if n <= partIDs {
		rec, err := s.Append(r)
		return []Record{rec}, err
	}
	// Flatten to (list, folder, id), then deal the entries into parts.
	type entry struct {
		list       int
		folder, id string
	}
	var all []entry
	for i, l := range lists {
		folders := make([]string, 0, len(*l))
		for f := range *l {
			folders = append(folders, f)
		}
		sort.Strings(folders)
		for _, f := range folders {
			for _, id := range (*l)[f] {
				all = append(all, entry{i, f, id})
			}
		}
	}
	parts := (len(all) + partIDs - 1) / partIDs
	group := newID()
	var out []Record
	for i := range parts {
		p := r
		p.ID, p.Group, p.Part, p.Parts = "", group, i+1, parts
		if i == 0 {
			p.ID = group
		}
		p.Touched, p.AlreadyInTarget, p.CopiedBack = nil, nil, nil
		dst := []*map[string][]string{&p.Touched, &p.AlreadyInTarget, &p.CopiedBack}
		k := min(partIDs, len(all))
		for _, e := range all[:k] {
			if *dst[e.list] == nil {
				*dst[e.list] = map[string][]string{}
			}
			(*dst[e.list])[e.folder] = append((*dst[e.list])[e.folder], e.id)
		}
		all = all[k:]
		rec, err := s.Append(p)
		out = append(out, rec)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// merged combines the parts of one group into a single record whose id is the
// group id. Metadata comes from part 1; At is the last part's.
func merged(parts []Record) Record {
	m := parts[0]
	if m.Group == "" {
		return m
	}
	m.ID, m.Part, m.Parts = m.Group, 0, 0
	m.Touched, m.AlreadyInTarget, m.CopiedBack = map[string][]string{}, map[string][]string{}, map[string][]string{}
	for _, p := range parts {
		for f, ids := range p.Touched {
			m.Touched[f] = append(m.Touched[f], ids...)
		}
		for f, ids := range p.AlreadyInTarget {
			m.AlreadyInTarget[f] = append(m.AlreadyInTarget[f], ids...)
		}
		for f, ids := range p.CopiedBack {
			m.CopiedBack[f] = append(m.CopiedBack[f], ids...)
		}
		m.At = p.At
	}
	return m
}

// Get returns the record with the given id, as one apply: for any part of a
// group it returns the merged group (id: the group id).
func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

func (s *Store) get(id string) (Record, bool) {
	var hit *Record
	for i := range s.recs {
		if s.recs[i].ID == id {
			hit = &s.recs[i]
			break
		}
	}
	if hit == nil {
		return Record{}, false
	}
	if hit.Group == "" {
		return *hit, true
	}
	var parts []Record
	for _, r := range s.recs {
		if r.Group == hit.Group {
			parts = append(parts, r)
		}
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].Part < parts[j].Part })
	return merged(parts), true
}

// Undone reports whether an undo of the apply (record or group id) exists.
func (s *Store) Undone(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.get(id)
	if ok {
		id = rec.ID
	}
	for _, r := range s.recs {
		if r.Undoes == id {
			return true
		}
	}
	return false
}

// TargetHistory says whether an earlier successful apply on the account used
// target, and when create_folder last created it (zero if never).
func (s *Store) TargetHistory(account, target string) (applied bool, created time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.Account != account {
			continue
		}
		switch {
		case r.Kind == KindCreateFolder && r.Target == target:
			created = r.At
		case r.Intent != nil && r.Intent.Target == target && r.Error == "":
			applied = true
		}
	}
	return applied, created
}

// List returns up to limit records, newest first, for one account (empty:
// all). Records are returned whole; callers drop Touched if they do not want it.
func (s *Store) List(account string, limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.recs) - 1; i >= 0 && len(out) < limit; i-- {
		if account == "" || s.recs[i].Account == account {
			out = append(out, s.recs[i])
		}
	}
	return out
}

// Grouped is List with each group of continuation records folded into one
// record (id: the group id, positioned at its newest part), so a caller sees
// one entry per apply. limit counts entries, not lines.
func (s *Store) Grouped(account string, limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	seen := map[string]bool{}
	for i := len(s.recs) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.recs[i]
		if account != "" && r.Account != account {
			continue
		}
		if r.Group != "" {
			if seen[r.Group] {
				continue
			}
			seen[r.Group] = true
			if m, ok := s.get(r.ID); ok {
				r = m
			}
		}
		out = append(out, r)
	}
	return out
}

// Close closes the file.
func (s *Store) Close() error { return s.f.Close() }
