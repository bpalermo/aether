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

// TestOTLPPinDefaultsOn pins issue #950: the plugin runs under the HOST's
// resolver, so an unpinned cluster Service name exports nothing. Pinning must be
// the default, not an opt-in an operator has to discover.
func TestOTLPPinDefaultsOn(t *testing.T) {
	flag := GetCommand().Flags().Lookup("otlp-pin-endpoint")
	if assert.NotNil(t, flag, "--otlp-pin-endpoint must be registered") {
		assert.Equal(t, "true", flag.DefValue)
	}
}
