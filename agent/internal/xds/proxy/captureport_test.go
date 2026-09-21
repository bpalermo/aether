package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildCaptureTCPPortFilterChain covers the per-port TCP capture chain
// (proposal 037).
//
// The destination_port match is the whole mechanism: Envoy evaluates it ahead
// of prefix_ranges, and use_original_dst (already set on the capture listener)
// makes it the port the client actually dialed before the REDIRECT. A chain
// matching only the ClusterIP would outrank the HCM catch-all's
// application_protocols match and swallow the service's HTTP traffic — which
// is exactly why a service could not be both protocols before.
func TestBuildCaptureTCPPortFilterChain(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:mixed.aether-test.aether.internal",
		ClusterIP:   "10.96.0.60",
	}

	fc := buildCaptureTCPPortFilterChain(svc, 9000, "spiffe://example.org/ns/default/sa/client")
	require.NotNil(t, fc)

	assert.Equal(t, "cap_tcp_tcp:mixed.aether-test.aether.internal_9000", fc.GetName())

	m := fc.GetFilterChainMatch()
	require.NotNil(t, m)
	require.Len(t, m.GetPrefixRanges(), 1)
	assert.Equal(t, "10.96.0.60", m.GetPrefixRanges()[0].GetAddressPrefix())
	assert.Equal(t, uint32(32), m.GetPrefixRanges()[0].GetPrefixLen().GetValue())
	require.NotNil(t, m.GetDestinationPort(), "without the port match this chain swallows the VIP's HTTP traffic")
	assert.Equal(t, uint32(9000), m.GetDestinationPort().GetValue())

	// It must target the PORT's own cluster, not the floor cluster: the floor
	// forwards to the pod's PRIMARY port, so routing :9000 through it would
	// deliver that traffic to :8080.
	assert.Equal(t, "tcp:mixed.aether-test.aether.internal:9000",
		TCPPortClusterName(svc.ClusterName, 9000))
}

func TestBuildCaptureTCPPortFilterChain_Rejects(t *testing.T) {
	good := CaptureTCPService{ClusterName: "tcp:svc.ns.aether.internal", ClusterIP: "10.96.0.60"}
	tests := []struct {
		name string
		svc  CaptureTCPService
		port uint32
	}{
		{name: "no ClusterIP", svc: CaptureTCPService{ClusterName: good.ClusterName}, port: 9000},
		{name: "no ClusterName", svc: CaptureTCPService{ClusterIP: good.ClusterIP}, port: 9000},
		{name: "unparseable ClusterIP", svc: CaptureTCPService{ClusterName: good.ClusterName, ClusterIP: "not-an-ip"}, port: 9000},
		{name: "port zero", svc: good, port: 0},
		{name: "port out of range", svc: good, port: 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, buildCaptureTCPPortFilterChain(tt.svc, tt.port, "spiffe://example.org/ns/a/sa/b"),
				"a malformed input must yield no chain rather than a chain that matches nothing")
		})
	}
}
