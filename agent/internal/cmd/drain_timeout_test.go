package cmd

import (
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxyConnectionDrainTimeoutFlag pins --proxy-connection-drain-timeout:
// registered on the node agent, defaulting to 2s (the value that makes upstream
// Envoy's proactive drain window non-empty under the chart's 10s drain time),
// and parsed as a Go duration.
func TestProxyConnectionDrainTimeoutFlag(t *testing.T) {
	f := rootCmd.Flags().Lookup("proxy-connection-drain-timeout")
	require.NotNil(t, f, "the chart passes --proxy-connection-drain-timeout; the agent must accept it")
	assert.Equal(t, proxy.DefaultConnectionDrainTimeout.String(), f.DefValue)
	assert.Equal(t, 2*time.Second, proxy.DefaultConnectionDrainTimeout)

	old := cfg.ProxyConnectionDrainTimeout
	t.Cleanup(func() { cfg.ProxyConnectionDrainTimeout = old })
	require.NoError(t, f.Value.Set("3s"))
	assert.Equal(t, 3*time.Second, cfg.ProxyConnectionDrainTimeout)
	require.Error(t, f.Value.Set("two"), "not a duration")
}

func TestValidateProxyConnectionDrainTimeout(t *testing.T) {
	for _, tc := range []struct {
		in time.Duration
		ok bool
	}{
		{proxy.DefaultConnectionDrainTimeout, true},
		{time.Millisecond, true},
		{0, false},
		{-time.Second, false},
	} {
		err := validateProxyConnectionDrainTimeout(tc.in)
		if tc.ok {
			assert.NoError(t, err, "%s", tc.in)
		} else {
			assert.Error(t, err, "%s: a non-positive drain timeout leaves the node HCMs at Envoy's 5s default", tc.in)
		}
	}
}
