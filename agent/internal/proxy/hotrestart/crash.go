package hotrestart

import (
	"errors"
	"os/exec"
	"syscall"
)

// isCrashSignal reports whether an *exec.Cmd Wait error is a fatal-signal crash
// (SIGSEGV/SIGABRT/SIGBUS/SIGILL) rather than a clean non-zero exit. A base-id
// bind collision exits non-zero (errno 98) WITHOUT a signal; a genuine Envoy
// crash is signaled. Distinguishing them lets the supervisor give up on a
// crashing Envoy fast instead of burning the (much larger) bind-collision retry
// budget and looping silently for minutes (talos worker-01, 2026-06-19: a CDS
// referencing a gone netns crashed every epoch and masqueraded as a collision).
func isCrashSignal(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	switch ws.Signal() {
	case syscall.SIGSEGV, syscall.SIGABRT, syscall.SIGBUS, syscall.SIGILL:
		return true
	}
	return false
}
