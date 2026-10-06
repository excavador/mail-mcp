package pdftext

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
)

// ChildMode is the argv[1] that makes the helper binary act as the
// limit-and-exec stage: `pdftext __limit-exec <cpu-seconds> <as-bytes> -- <argv...>`.
//
// Go's os/exec has no way to set rlimits on a child, and prlimit(2) after Start
// is racy (the child runs, and can allocate, before the call lands). So the
// helper starts a copy of itself in this mode: it sets the limits on itself
// and execs the real program, which therefore begins life already limited.
const ChildMode = "__limit-exec"

// RunChild implements ChildMode. args is os.Args[2:]. It only returns on
// error: on success the process image is replaced.
func RunChild(args []string) error {
	if len(args) < 4 || args[2] != "--" {
		return errors.New("usage: __limit-exec <cpu-seconds> <as-bytes> -- <program> [args...]")
	}
	cpu, err1 := strconv.ParseUint(args[0], 10, 64)
	as, err2 := strconv.ParseUint(args[1], 10, 64)
	if err1 != nil || err2 != nil {
		return errors.New("bad limit")
	}
	argv := args[3:]
	// One OS thread, no new ones needed: RLIMIT_NPROC=1 makes clone() fail,
	// so nothing may start a thread between the limit and the exec.
	runtime.GOMAXPROCS(1)
	runtime.LockOSThread()
	if err := applyLimits(cpu, as); err != nil {
		return err
	}
	// A minimal environment: pdftotext needs nothing from the helper's.
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/nonexistent"}
	if err := syscall.Exec(argv[0], argv, env); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// exitChild is what main calls for ChildMode.
func ExitChild(args []string) {
	if err := RunChild(args); err != nil {
		fmt.Fprintln(os.Stderr, "pdftext: limit-exec failed")
		os.Exit(125)
	}
}
