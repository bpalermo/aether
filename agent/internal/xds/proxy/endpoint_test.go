package proxy

import (
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClusterLoadAssignment(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
	}{
		{name: "standard", serviceName: "my-service"},
		{name: "empty", serviceName: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cla := NewClusterLoadAssignment(tt.serviceName)
			require.NotNil(t, cla)
			assert.Equal(t, tt.serviceName, cla.GetClusterName())
			assert.Empty(t, cla.GetEndpoints())
		})
	}
}

// TestSortLocalityLbEndpoints verifies endpoints sort by address so CLAs
// rebuilt from maps or unstable registry listings hash identically when the
// endpoint set is unchanged (EDS resource order is part of the version hash).
func TestSortLocalityLbEndpoints(t *testing.T) {
	mk := func(ip string) *registryv1.ServiceEndpoint {
		return &registryv1.ServiceEndpoint{Ip: ip}
	}
	eps := []*endpointv3.LocalityLbEndpoints{
		ServiceLocalityLbEndpointFromRegistryEndpoint(mk("10.244.4.9"), "", "", WaypointRewrite{}),
		ServiceLocalityLbEndpointFromRegistryEndpoint(mk("10.244.1.7"), "", "", WaypointRewrite{}),
		ServiceLocalityLbEndpointFromRegistryEndpoint(mk("10.244.3.2"), "", "", WaypointRewrite{}),
	}

	SortLocalityLbEndpoints(eps)

	got := make([]string, 0, len(eps))
	for _, e := range eps {
		got = append(got, e.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress().GetAddress())
	}
	assert.Equal(t, []string{"10.244.1.7", "10.244.3.2", "10.244.4.9"}, got)

	// Empty and nil entries must not panic and sort first.
	mixed := []*endpointv3.LocalityLbEndpoints{eps[2], {}, eps[0]}
	SortLocalityLbEndpoints(mixed)
	assert.Empty(t, mixed[0].GetLbEndpoints())
}
