package cmd

import (
	"testing"

	"github.com/bpalermo/aether/agent/constants"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAgentConfig(t *testing.T) {
	c := NewAgentConfig()

	require.NotNil(t, c)
	assert.False(t, c.Debug)
	assert.Equal(t, constants.DefaultProxyID, c.ProxyServiceNodeID)
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
			name:     "proxy service node ID uses default",
			got:      c.ProxyServiceNodeID,
			expected: constants.DefaultProxyID,
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
	c.ProxyServiceNodeID = "custom-proxy-id"
	c.RegistrarAddress = "custom-registrar:9443"

	assert.True(t, c.Debug)
	assert.Equal(t, "custom-proxy-id", c.ProxyServiceNodeID)
	assert.Equal(t, "custom-registrar:9443", c.RegistrarAddress)
}

func TestAgentConfig_SubConfigsAreIndependent(t *testing.T) {
	cfg1 := NewAgentConfig()
	cfg2 := NewAgentConfig()

	assert.NotSame(t, cfg1.CNIServerConfig, cfg2.CNIServerConfig)
}

// TestCaptureRedirectAllFlag verifies the --capture-redirect-all flag is
// registered on the agent root command, defaults to false, and binds to the
// package cfg.CaptureRedirectAll field (which root.go threads into
// SnapshotCache.SetCaptureRedirectAll). This is the agent half of the proposal
// 022 M2a spike: without the flag the cache never adds the passthrough chain.
func TestCaptureRedirectAllFlag(t *testing.T) {
	cmd := GetCommand()

	f := cmd.Flags().Lookup("capture-redirect-all")
	require.NotNil(t, f, "expected --capture-redirect-all flag to be registered")
	assert.Equal(t, "false", f.DefValue, "flag must default to false")

	// Restore the package global the flag is bound to after the test mutates it.
	t.Cleanup(func() { cfg.CaptureRedirectAll = false })

	require.NoError(t, cmd.Flags().Set("capture-redirect-all", "true"))
	assert.True(t, cfg.CaptureRedirectAll, "setting the flag must drive cfg.CaptureRedirectAll")
}
