// Package memlimit sets the Go runtime's soft memory limit from the
// container's cgroup, since the runtime knows nothing of it: without a limit
// the GC lets the heap grow until the kernel OOM-kills the process.
package memlimit

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	cgroupV2Max = "/sys/fs/cgroup/memory.max"
	// fraction of the cgroup limit given to the Go heap; the rest is headroom
	// for stacks, cgo-free runtime overhead and page cache charged to us.
	fraction = 0.9
)

// Parse turns the content of a cgroup v2 memory.max file into a limit in
// bytes. "max" (no limit) yields ok == false.
func Parse(content string) (limit int64, ok bool, err error) {
	s := strings.TrimSpace(content)
	if s == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false, fmt.Errorf("memlimit: unusable memory.max %q", s)
	}
	return n, true, nil
}

// FromFile reads the cgroup limit at path and returns 90% of it. A missing
// file or "max" yields ok == false and no error: there is simply no limit.
func FromFile(path string) (limit int64, ok bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	n, ok, err := Parse(string(b))
	if err != nil || !ok {
		return 0, false, err
	}
	return int64(float64(n) * fraction), true, nil
}

// Set applies the soft memory limit: GOMEMLIMIT if set (the runtime has
// already honoured it, so it is left alone), else 90% of the cgroup v2 limit.
func Set(log *slog.Logger) { set(log, cgroupV2Max, os.Getenv("GOMEMLIMIT")) }

func set(log *slog.Logger, path, env string) {
	if env != "" {
		log.Info("memory limit from GOMEMLIMIT", "value", env)
		return
	}
	limit, ok, err := FromFile(path)
	switch {
	case err != nil:
		log.Warn("memory limit: cgroup unreadable, none set", "error", err.Error())
	case !ok:
		log.Info("memory limit: none (no cgroup limit)")
	default:
		debug.SetMemoryLimit(limit)
		log.Info("memory limit from cgroup", "bytes", limit)
	}
}
