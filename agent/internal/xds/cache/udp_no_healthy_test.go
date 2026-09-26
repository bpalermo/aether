package cache

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/stretchr/testify/assert"
)

// la builds a load assignment whose endpoints carry the given health statuses.
func la(statuses ...corev3.HealthStatus) *endpointv3.ClusterLoadAssignment {
	lbs := make([]*endpointv3.LbEndpoint, 0, len(statuses))
	for _, st := range statuses {
		lbs = append(lbs, &endpointv3.LbEndpoint{HealthStatus: st})
	}
	return &endpointv3.ClusterLoadAssignment{
		Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: lbs}},
	}
}

// TestUnroutableUDPEndpoints is the RED STATE for the #931 detector (#853: a
// gate that has never been shown to fire is not a gate).
//
// The all-UNHEALTHY row is the exact shape #931 shipped in, measured on a live
// cluster: one endpoint at the right address and port, marked UNHEALTHY because
// its app port had been TCP-probed, and every datagram to it discarded.
func TestUnroutableUDPEndpoints(t *testing.T) {
	tests := []struct {
		name string
		in   *endpointv3.ClusterLoadAssignment
		want int
	}{
		{
			name: "the #931 shape: the only endpoint is UNHEALTHY",
			in:   la(corev3.HealthStatus_UNHEALTHY),
			want: 1,
		},
		{
			name: "several endpoints, none routable",
			in: la(corev3.HealthStatus_UNHEALTHY, corev3.HealthStatus_DRAINING,
				corev3.HealthStatus_TIMEOUT),
			want: 3,
		},
		{
			// Envoy load balances to UNKNOWN -- it is the default for an endpoint
			// nobody has said anything about -- so this is NOT a blackhole.
			// Reading UNKNOWN as unhealthy would fire on every healthy service
			// whose endpoints carry no explicit status, which is most of them.
			name: "UNKNOWN is routable",
			in:   la(corev3.HealthStatus_UNKNOWN),
			want: 0,
		},
		{
			name: "HEALTHY is routable",
			in:   la(corev3.HealthStatus_HEALTHY),
			want: 0,
		},
		{
			// One survivor is enough: udp_proxy has a host, so nothing is lost
			// and nothing should be reported.
			name: "one healthy endpoint among unhealthy ones is not a blackhole",
			in: la(corev3.HealthStatus_UNHEALTHY, corev3.HealthStatus_HEALTHY,
				corev3.HealthStatus_DRAINING),
			want: 0,
		},
		{
			// A cold start, not a blackhole. Reporting here would make the
			// counter fire on every service between its cluster appearing and
			// its endpoints arriving, which is the normal path and would teach
			// everyone to ignore the signal.
			name: "no endpoints at all is a cold start",
			in:   la(),
			want: 0,
		},
		{
			name: "a nil load assignment reports nothing",
			in:   nil,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, unroutableUDPEndpoints(tt.in))
		})
	}
}

// TestUnroutableUDPEndpointsCountsAcrossLocalities pins that the scan is over
// every locality, not just the first: a service split across zones has one
// LocalityLbEndpoints per zone, and "none is routable" has to mean none anywhere.
func TestUnroutableUDPEndpointsCountsAcrossLocalities(t *testing.T) {
	multi := &endpointv3.ClusterLoadAssignment{
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{LbEndpoints: []*endpointv3.LbEndpoint{{HealthStatus: corev3.HealthStatus_UNHEALTHY}}},
			{LbEndpoints: []*endpointv3.LbEndpoint{{HealthStatus: corev3.HealthStatus_UNHEALTHY}}},
		},
	}
	assert.Equal(t, 2, unroutableUDPEndpoints(multi))

	// And a healthy endpoint in the SECOND locality still clears it.
	mixed := &endpointv3.ClusterLoadAssignment{
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{LbEndpoints: []*endpointv3.LbEndpoint{{HealthStatus: corev3.HealthStatus_UNHEALTHY}}},
			{LbEndpoints: []*endpointv3.LbEndpoint{{HealthStatus: corev3.HealthStatus_HEALTHY}}},
		},
	}
	assert.Equal(t, 0, unroutableUDPEndpoints(mixed),
		"a routable endpoint in any locality means udp_proxy has a host")
}
