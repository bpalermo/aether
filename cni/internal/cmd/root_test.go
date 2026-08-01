package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRetiredFlagsGone pins proposal 031 (round 2): the per-pod capture
// redirect is unconditional for managed pods — the netconf gate and the flag
// that wrote it must not come back. Per-pod capture.aether.io/* annotations
// remain the opt-out.
func TestRetiredFlagsGone(t *testing.T) {
	cmd := GetCommand()
	for _, name := range []string{"transparent-capture"} {
		assert.Nil(t, cmd.Flags().Lookup(name), "flag --%s was retired and must not be re-registered", name)
	}
}
