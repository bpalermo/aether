package cmd

import (
	"testing"
	"time"

	xdsconfig "aethermesh.dev/agent/internal/xds/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEastWestQUICIdleTimeoutFlag pins the --east-west-quic-idle-timeout flag
// (aether#1054): registered on the agent, defaulting to the 8s twin idle, and
// parsed as a Go duration.
func TestEastWestQUICIdleTimeoutFlag(t *testing.T) {
	f := rootCmd.Flags().Lookup("east-west-quic-idle-timeout")
	require.NotNil(t, f, "the chart passes --east-west-quic-idle-timeout; the agent must accept it")
	assert.Equal(t, xdsconfig.DefaultQUICTwinIdleTimeout.String(), f.DefValue)
	assert.Equal(t, 8*time.Second, xdsconfig.DefaultQUICTwinIdleTimeout)

	// The flag is bound to cfg.EastWestQUICIdleTimeout; restore it after.
	old := cfg.EastWestQUICIdleTimeout
	t.Cleanup(func() { cfg.EastWestQUICIdleTimeout = old })
	require.NoError(t, f.Value.Set("5s"))
	assert.Equal(t, 5*time.Second, cfg.EastWestQUICIdleTimeout)
	require.Error(t, f.Value.Set("eight"), "not a duration")
}

func TestValidateEastWestQUICIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		in time.Duration
		ok bool
	}{
		{xdsconfig.DefaultQUICTwinIdleTimeout, true},
		{time.Millisecond, true},
		{0, false},
		{-time.Second, false},
	} {
		err := validateEastWestQUICIdleTimeout(tc.in)
		if tc.ok {
			assert.NoError(t, err, "%s", tc.in)
		} else {
			assert.Error(t, err, "%s: a non-positive idle timeout renders Envoy's 1h default on the twins", tc.in)
		}
	}
}
