package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPodNDots pins the ndots derivation: the injected dnsConfig ndots is the
// mesh domain's label count, so it can never drift from the domain (the
// retired --pod-ndots flag could).
func TestPodNDots(t *testing.T) {
	assert.Equal(t, "2", podNDots("aether.internal"))
	assert.Equal(t, "3", podNDots("mesh.corp.example"))
	assert.Equal(t, "1", podNDots("internal"))
}

// TestRetiredFlagsGone pins the --pod-ndots retirement: ndots is derived from
// --mesh-domain and the standalone flag must not come back.
func TestRetiredFlagsGone(t *testing.T) {
	cmd := GetCommand()
	assert.Nil(t, cmd.Flags().Lookup("pod-ndots"), "flag --pod-ndots was retired (derived from --mesh-domain) and must not be re-registered")
	assert.NotNil(t, cmd.Flags().Lookup("mesh-domain"))
}
