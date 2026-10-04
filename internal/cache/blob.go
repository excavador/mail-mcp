package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// BlobPath is where the blob with the given SHA-256 (hex) lives. The first
// two hex characters fan the files out over 256 directories so no single
// directory grows to hundreds of thousands of entries.
func (c *Cache) BlobPath(sum string) string {
	if len(sum) < 2 {
		return filepath.Join(c.dir, "blobs", sum)
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
	return sum, nil
}
