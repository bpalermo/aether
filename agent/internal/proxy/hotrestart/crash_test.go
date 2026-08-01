package hotrestart

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCrashSignal(t *testing.T) {
	// Killed by a fatal signal (SIGSEGV) -> crash.
	assert.True(t, isCrashSignal(exec.Command("sh", "-c", "kill -SEGV $$").Run()),
		"a SIGSEGV-terminated process is a crash")

	// Clean non-zero exit (a base-id bind collision exits errno 98, no signal).
	assert.False(t, isCrashSignal(exec.Command("sh", "-c", "exit 1").Run()),
		"a non-zero exit without a signal is not a crash")

	// Success and non-exec errors are not crashes.
	assert.False(t, isCrashSignal(exec.Command("sh", "-c", "exit 0").Run()))
	assert.False(t, isCrashSignal(errors.New("boom")))
	assert.False(t, isCrashSignal(nil))
}
