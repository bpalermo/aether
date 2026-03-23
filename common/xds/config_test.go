package xds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServerConfig_Defaults(t *testing.T) {
	tests := []struct {
		name     string
		expected *ServerConfig
	}{
		{
			name: "default network is tcp",
			expected: &ServerConfig{
				Network:         "tcp",
				Address:         ":50051",
				ShutdownTimeout: 30 * time.Second,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServerConfig()
			require.NotNil(t, got)
			assert.Equal(t, tt.expected.Network, got.Network)
			assert.Equal(t, tt.expected.Address, got.Address)
			assert.Equal(t, tt.expected.ShutdownTimeout, got.ShutdownTimeout)
		})
	}
}

func TestNewServerConfig_WithUDS(t *testing.T) {
	tests := []struct {
		name            string
		socketPath      string
		expectedNetwork string
		expectedAddress string
	}{
		{
			name:            "sets unix network and address",
			socketPath:      "/tmp/test.sock",
			expectedNetwork: "unix",
			expectedAddress: "/tmp/test.sock",
		},
		{
			name:            "accepts nested path",
			socketPath:      "/var/run/aether/xds.sock",
			expectedNetwork: "unix",
			expectedAddress: "/var/run/aether/xds.sock",
		},
		{
			name:            "accepts empty path",
			socketPath:      "",
			expectedNetwork: "unix",
			expectedAddress: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServerConfig(WithUDS(tt.socketPath))
			require.NotNil(t, got)
			assert.Equal(t, tt.expectedNetwork, got.Network)
			assert.Equal(t, tt.expectedAddress, got.Address)
			// ShutdownTimeout should remain at its default value.
			assert.Equal(t, defaultServerShutdownTimeout, got.ShutdownTimeout)
		})
	}
}

func TestNewServerConfig_MultipleOptions(t *testing.T) {
	tests := []struct {
		name            string
		opts            []ServerConfigOpt
		expectedNetwork string
		expectedAddress string
	}{
		{
			name: "last WithUDS wins when applied twice",
			opts: []ServerConfigOpt{
				WithUDS("/tmp/first.sock"),
				WithUDS("/tmp/second.sock"),
			},
			expectedNetwork: "unix",
			expectedAddress: "/tmp/second.sock",
		},
		{
			name: "WithUDS overrides default tcp",
			opts: []ServerConfigOpt{
				WithUDS("/tmp/override.sock"),
			},
			expectedNetwork: "unix",
			expectedAddress: "/tmp/override.sock",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServerConfig(tt.opts...)
			require.NotNil(t, got)
			assert.Equal(t, tt.expectedNetwork, got.Network)
			assert.Equal(t, tt.expectedAddress, got.Address)
		})
	}
}
