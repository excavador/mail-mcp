//go:build linux

package pdftext

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// applyLimits sets the resource limits of the current process. It runs in the
// limit-and-exec child, before exec, so pdftotext starts with the limits
// already in force: there is no window in which it runs unconstrained, which
// prlimit(2) on a started child would leave.
func applyLimits(cpuSec, asBytes uint64) error {
	for _, l := range []struct {
		res  int
		name string
		v    uint64
	}{
		{syscall.RLIMIT_CPU, "cpu", cpuSec},
		{syscall.RLIMIT_AS, "as", asBytes},
		{syscall.RLIMIT_FSIZE, "fsize", 0},
		{syscall.RLIMIT_NOFILE, "nofile", 16},
		{syscall.RLIMIT_CORE, "core", 0},
		// One process for the uid at the time of the check; exec needs no
		// fork, so pdftotext runs, and any fork it attempts fails.
		{unix.RLIMIT_NPROC, "nproc", 1},
	} {
		max := l.v
		if l.res == syscall.RLIMIT_CPU {
			// SIGXCPU at the soft limit, SIGKILL two seconds later: with equal
			// limits the kernel sends only SIGKILL, which cannot be told from
			// an OOM kill.
			max += 2
		}
		if err := syscall.Setrlimit(l.res, &syscall.Rlimit{Cur: l.v, Max: max}); err != nil {
			return fmt.Errorf("setrlimit %s: %w", l.name, err)
		}
	}
	return nil
}
