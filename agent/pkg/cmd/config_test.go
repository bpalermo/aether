package cmd

import (
	"testing"

	cniServer "github.com/bpalermo/aether/agent/pkg/cni/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/install"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAgentConfig(t *testing.T) {
	c := NewAgentConfig()

	require.NotNil(t, c)
	assert.False(t, c.Debug)
	assert.Equal(t, constants.DefaultClusterName, c.ClusterName)
	assert.Equal(t, constants.DefaultProxyID, c.ProxyServiceNodeID)
	assert.NotNil(t, c.InstallConfig)
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
			name:     "cluster name uses default",
			got:      c.ClusterName,
			expected: constants.DefaultClusterName,
		},
		{
			name:     "proxy service node ID uses default",
			got:      c.ProxyServiceNodeID,
			expected: constants.DefaultProxyID,
		},
		{
			name:     "install config is initialized",
			got:      c.InstallConfig,
			expected: install.NewInstallerConfig(),
		},
		{
			name:     "CNI server config is initialized",
			got:      c.CNIServerConfig,
			expected: cniServer.NewCNIServerConfig(),
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

	// Test that fields can be modified
	c.Debug = true
	c.ClusterName = "production-cluster"
	c.ProxyServiceNodeID = "custom-proxy-id"

	assert.True(t, c.Debug)
	assert.Equal(t, "production-cluster", c.ClusterName)
	assert.Equal(t, "custom-proxy-id", c.ProxyServiceNodeID)
}

func TestAgentConfig_SubConfigsAreIndependent(t *testing.T) {
	cfg1 := NewAgentConfig()
	cfg2 := NewAgentConfig()

	// Verify that configs are independent instances
	assert.NotSame(t, cfg1.InstallConfig, cfg2.InstallConfig)
	assert.NotSame(t, cfg1.CNIServerConfig, cfg2.CNIServerConfig)
}
