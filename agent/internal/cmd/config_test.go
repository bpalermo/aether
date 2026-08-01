package cmd

import (
	"testing"

	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAgentConfig(t *testing.T) {
	c := NewAgentConfig()

	require.NotNil(t, c)
	assert.False(t, c.Debug)
	assert.NotNil(t, c.CNIServerConfig)
}

func TestAgentConfig_DefaultValues(t *testing.T) {
	c := NewAgentConfig()

	tests := []struct {
		name     string
		got      interface{}
		expected interface{}
	}{
		{
			name:     "debug is false by default",
			got:      c.Debug,
			expected: false,
		},
		{
			name:     "CNI server config is initialized",
			got:      c.CNIServerConfig,
			expected: cniServer.NewCNIServerConfig(),
		},
		{
			name:     "registrar address uses default",
			got:      c.RegistrarAddress,
			expected: "aether-registrar.aether-system.svc:443",
		},
		{
			name:     "SPIRE is enabled by default",
			got:      c.SpireEnabled,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.got)
		})
	}
}

func TestAgentConfig_ConfigurableFields(t *testing.T) {
	c := NewAgentConfig()

	c.Debug = true
	c.NodeName = "custom-node"
	c.RegistrarAddress = "custom-registrar:9443"

	assert.True(t, c.Debug)
	assert.Equal(t, "custom-node", c.NodeName)
	assert.Equal(t, "custom-registrar:9443", c.RegistrarAddress)
}

func TestAgentConfig_SubConfigsAreIndependent(t *testing.T) {
	cfg1 := NewAgentConfig()
	cfg2 := NewAgentConfig()

	assert.NotSame(t, cfg1.CNIServerConfig, cfg2.CNIServerConfig)
}

// TestRetiredFlagsGone pins proposal 031: the retired feature flags must NOT
// come back — capture, the dormant redirect-all passthrough chain, and L4
// routing are unconditional, the tunnel port is a constant, the xDS identity
// is the node name (no separate proxy-id), and the startup-taint remover runs
// unconditionally.
func TestRetiredFlagsGone(t *testing.T) {
	cmd := GetCommand()
	for _, name := range []string{
		"transparent-capture", "capture-redirect-all", "l4-routes", "east-west-tunnel-port",
		"proxy-id", "remove-startup-taint",
	} {
		assert.Nil(t, cmd.Flags().Lookup(name), "flag --%s was retired by proposal 031 and must not be re-registered", name)
	}
}

// TestRetiredEdgeFlagsGone pins the edge-side retirements (031 round 2): the
// readiness port is the fixed constant 18021, per-Gateway addressing is
// unconditional (Phase 1 remains only as the EdgeServiceName-empty /
// port-allocation-failure fallback), and the edge's empty local store lives at
// the fixed pod-local path.
func TestRetiredEdgeFlagsGone(t *testing.T) {
	for _, name := range []string{"edge-readiness-port", "edge-per-gateway-addressing", "mounted-registry-dir", "proxy-id"} {
		assert.Nil(t, edgeCmd.Flags().Lookup(name), "edge flag --%s was retired and must not be re-registered", name)
	}
}

// TestGammaDefaultOn pins proposal 031's end state: gamma is a default-on kill
// switch (CRD detection makes present-CRDs = on the right default).
func TestGammaDefaultOn(t *testing.T) {
	f := GetCommand().Flags().Lookup("gamma")
	require.NotNil(t, f)
	assert.Equal(t, "true", f.DefValue)
}
