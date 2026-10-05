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
	"sync"
	"time"

	"github.com/excavador/mail-mcp/internal/organise"
)

// Kinds.
const KindCreateFolder = "create_folder"

// maxLineBytes bounds one line when loading. An apply lists every touched
// stable id (up to 50000, around a hundred bytes each), so the bound is well
// above a megabyte; a line beyond it is skipped and logged.
const maxLineBytes = 16 << 20

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
	ID      string             `json:"id"`
	At      time.Time          `json:"at"`
	Account string             `json:"account"`
	Kind    string             `json:"kind"` // create_folder, apply, undo or reapply
	Intent  *organise.Intent   `json:"intent,omitempty"`
	Action  string             `json:"action,omitempty"`
	Target  string             `json:"target,omitempty"` // create_folder: the new folder
	Preview PreviewInfo        `json:"preview"`
	Touched []organise.Touched `json:"touched,omitempty"`
	Skipped int                `json:"skipped,omitempty"`
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
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(line); err != nil { // O_APPEND: one write, at the end
		return r, fmt.Errorf("history: write: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return r, fmt.Errorf("history: sync: %w", err)
	}
	s.recs = append(s.recs, r)
	return r, nil
}

// Get returns the record with the given id.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
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

// Close closes the file.
func (s *Store) Close() error { return s.f.Close() }
