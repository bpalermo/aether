package proxy

import (
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildWaypointTunnelChain(t *testing.T) {
	fc := BuildWaypointTunnelChain("echo.default.aether.internal")
	// SNI suffix match so any structured <port>.echo.default... lands here.
	assert.Equal(t, []string{"*.echo.default.aether.internal"}, fc.GetFilterChainMatch().GetServerNames())
	assert.Equal(t, "tls", fc.GetFilterChainMatch().GetTransportProtocol())
	require.Len(t, fc.GetFilters(), 1, "a single tcp_proxy filter (raw passthrough)")
	assert.Equal(t, "ew_echo.default.aether.internal", fc.GetName())
}

func TestBuildWaypointTunnelListener(t *testing.T) {
	assert.Nil(t, BuildWaypointTunnelListener(18009, nil), "no chains -> no listener")

	ln := BuildWaypointTunnelListener(18009, []*listenerv3.FilterChain{BuildWaypointTunnelChain("echo.default.aether.internal")})
	require.NotNil(t, ln)
	assert.Equal(t, WaypointTunnelListenerName, ln.GetName())
	// Host netns: a plain socket address, NO network-namespace filepath.
	sa := ln.GetAddress().GetSocketAddress()
	assert.Equal(t, "0.0.0.0", sa.GetAddress())
	assert.Equal(t, uint32(18009), sa.GetPortValue())
	assert.Empty(t, sa.GetNetworkNamespaceFilepath(), "tunnel binds the host netns")
	require.Len(t, ln.GetListenerFilters(), 1, "tls_inspector reads the SNI")
}

func TestBuildWaypointIngressCluster(t *testing.T) {
	c := BuildWaypointIngressCluster("echo.default.aether.internal", []string{"10.244.1.5", "10.244.2.7"})
	assert.Equal(t, "ew_ingress_echo.default.aether.internal", c.GetName())
	assert.Nil(t, c.GetTransportSocket(), "raw passthrough — the bytes are already the source's mTLS")
	eps := c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()
	require.Len(t, eps, 2)
	for _, ep := range eps {
		assert.Equal(t, uint32(defaultInboundPort), ep.GetEndpoint().GetAddress().GetSocketAddress().GetPortValue(),
			"pods are dialed at the mesh inbound port")
	}
	_ = waypointTunnelChainName // symmetry helper
}
