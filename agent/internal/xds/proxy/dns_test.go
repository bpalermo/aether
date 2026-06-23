package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateDNSListeners(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	ls, err := GenerateDNSListeners(pod, 18053)
	require.NoError(t, err)
	require.Len(t, ls, 2)

	// UDP listener: netns-bound udp_proxy -> mesh_dns.
	udp := ls[0]
	assert.Equal(t, "dns_udp_p1", udp.GetName())
	assert.Equal(t, corev3.SocketAddress_UDP, udp.GetAddress().GetSocketAddress().GetProtocol())
	assert.Equal(t, uint32(18053), udp.GetAddress().GetSocketAddress().GetPortValue())
	assert.Equal(t, "/var/run/netns/p1", udp.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath())
	require.Len(t, udp.GetListenerFilters(), 1)
	var up udp_proxyv3.UdpProxyConfig
	require.NoError(t, udp.GetListenerFilters()[0].GetTypedConfig().UnmarshalTo(&up))
	assert.Equal(t, MeshDNSClusterName, up.GetCluster())

	// TCP listener: netns-bound tcp_proxy -> mesh_dns.
	tcp := ls[1]
	assert.Equal(t, "dns_tcp_p1", tcp.GetName())
	assert.Equal(t, corev3.SocketAddress_TCP, tcp.GetAddress().GetSocketAddress().GetProtocol())
	var tp tcp_proxyv3.TcpProxy
	require.NoError(t, tcp.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&tp))
	assert.Equal(t, MeshDNSClusterName, tp.GetCluster())
}

func TestNewMeshDNSCluster(t *testing.T) {
	c := NewMeshDNSCluster("10.0.0.5", 18054)
	assert.Equal(t, MeshDNSClusterName, c.GetName())
	ep := c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "10.0.0.5", ep.GetAddress())
	assert.Equal(t, uint32(18054), ep.GetPortValue())
}
