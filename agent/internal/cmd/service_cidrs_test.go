package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServiceCIDRsFlag pins --service-cidrs (proposal 044): registered on the
// agent, empty by default (discover the range from ServiceCIDR objects), and
// split on commas the way the chart renders agent.serviceCIDRs.
func TestServiceCIDRsFlag(t *testing.T) {
	f := rootCmd.Flags().Lookup("service-cidrs")
	require.NotNil(t, f, "the chart passes --service-cidrs; the agent must accept it")
	assert.Equal(t, "[]", f.DefValue)

	old := cfg.ServiceCIDRs
	t.Cleanup(func() { cfg.ServiceCIDRs = old })
	cfg.ServiceCIDRs = nil
	require.NoError(t, f.Value.Set("10.96.0.0/12,10.0.0.0/24"))
	assert.Equal(t, []string{"10.96.0.0/12", "10.0.0.0/24"}, cfg.ServiceCIDRs)
}

// A --service-cidrs entry that is not a CIDR stops the agent before it starts
// anything, rather than run it with a range nobody asked for.
func TestValidateAgentFlags_ServiceCIDRs(t *testing.T) {
	old := cfg.ServiceCIDRs
	t.Cleanup(func() { cfg.ServiceCIDRs = old })

	for _, tc := range []struct {
		in []string
		ok bool
	}{
		{nil, true},
		{[]string{"10.96.0.0/12"}, true},
		{[]string{"10.96.0.0/12", "fd00::/108"}, true},
		{[]string{"10.96.0.0"}, false},
		{[]string{"10.96.0.0/12", "ten"}, false},
	} {
		cfg.ServiceCIDRs = tc.in
		err := validateAgentFlags()
		if tc.ok {
			assert.NoError(t, err, "%v", tc.in)
		} else {
			assert.ErrorContains(t, err, "--service-cidrs", "%v", tc.in)
		}
	}
}
