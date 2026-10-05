package memlimit

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParse(t *testing.T) {
	if _, ok, err := Parse("max\n"); ok || err != nil {
		t.Errorf("max: ok=%v err=%v", ok, err)
	}
	if n, ok, err := Parse("536870912\n"); !ok || err != nil || n != 536870912 {
		t.Errorf("536870912: n=%d ok=%v err=%v", n, ok, err)
	}
	for _, bad := range []string{"garbage", "0", "-5", ""} {
		if _, ok, err := Parse(bad); err == nil || ok {
			t.Errorf("Parse(%q): ok=%v err=%v, want error", bad, ok, err)
		}
	}
}

func TestFromFile(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := FromFile(filepath.Join(dir, "missing")); ok || err != nil {
		t.Errorf("missing: ok=%v err=%v", ok, err)
	}
	p := filepath.Join(dir, "memory.max")
	if err := os.WriteFile(p, []byte("805306368\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := FromFile(p); !ok || err != nil || n != 724775731 {
		t.Errorf("n=%d ok=%v err=%v, want 724775731", n, ok, err)
	}
	if err := os.WriteFile(p, []byte("max\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FromFile(p); ok || err != nil {
		t.Errorf("max file: ok=%v err=%v", ok, err)
	}
	if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := FromFile(p); err == nil {
		t.Error("junk file: want error")
	}
}

func TestSetEnvWinsAndCgroupApplies(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(orig)
	p := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(p, []byte("805306368\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	set(quiet(), p, "1GiB")
	if got := debug.SetMemoryLimit(-1); got != orig {
		t.Errorf("non-empty env changed the limit: %d -> %d", orig, got)
	}
	set(quiet(), p, "")
	if got := debug.SetMemoryLimit(-1); got != 724775731 {
		t.Errorf("cgroup limit = %d, want 724775731", got)
	}
	debug.SetMemoryLimit(orig)
	set(quiet(), filepath.Join(t.TempDir(), "none"), "")
	if got := debug.SetMemoryLimit(-1); got != orig {
		t.Errorf("missing cgroup file changed the limit: %d", got)
	}
}
