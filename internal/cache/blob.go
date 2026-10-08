package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var sumRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BlobPath is where the blob with the given SHA-256 (hex) lives. The first
// two hex characters fan the files out over 256 directories so no single
// directory grows to hundreds of thousands of entries.
//
// Anything that is not exactly 64 lowercase hex characters yields "", never a
// path: a digest that reaches here from outside must not be able to name
// "../..", and "" fails every os call made with it.
func (c *Cache) BlobPath(sum string) string {
	if !sumRE.MatchString(sum) {
		return ""
	}
	return filepath.Join(c.dir, "blobs", sum[:2], sum)
}

// putBlob stores raw under its own digest and returns the digest.
//
// A blob is write-once: if the file is already there it is left exactly as it
// is -- the content is, by construction, identical, and not rewriting it is
// what makes "immutable" a property of the disk rather than a hope. New files
// are written to a temporary name and renamed into place, so a crash leaves
// either no blob or a complete one, never a torn one.
func (c *Cache) putBlob(raw []byte) (string, error) {
	h := sha256.Sum256(raw)
	sum := hex.EncodeToString(h[:])
	path := c.BlobPath(sum)

	if _, err := os.Stat(path); err == nil {
		return sum, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("cache: create blob directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("cache: create blob: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("cache: write blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("cache: sync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("cache: close blob: %w", err)
	}
	if err := os.Chmod(tmpName, 0o440); err != nil {
		cleanup()
		return "", fmt.Errorf("cache: chmod blob: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return "", fmt.Errorf("cache: place blob: %w", err)
	}
	// Persist the rename itself: without a directory fsync a crash can lose
	// the entry for a blob whose index row is already committed.
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("cache: sync blob directory: %w", err)
	}
	return sum, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// staleTemp is how old a ".tmp-" file must be before sweepTemp treats it as
// left behind by a crash rather than a write in progress in another process.
const staleTemp = time.Hour

// sweepTemp removes temporary files a crashed write left behind. Blobs are
// only ever renamed into place complete, so a ".tmp-" file is never a blob.
func sweepTemp(blobs string) {
	fans, err := os.ReadDir(blobs)
	if err != nil {
		return
	}
	for _, f := range fans {
		if !f.IsDir() {
			continue
		}
		dir := filepath.Join(blobs, f.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasPrefix(e.Name(), ".tmp-") {
				continue
			}
			// Another process may share this directory (the old pod of a
			// rolling update): its write in flight is a fresh ".tmp-" file,
			// and removing it would fail that write's rename.
			if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < staleTemp {
				continue
			}
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
