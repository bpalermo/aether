package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	dns_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/dns_filter/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDNSFilterConfig(t *testing.T) {
	cfg := BuildDNSFilterConfig("p1",
		[]DNSVirtualDomain{{Domain: "*.aether.internal", Addresses: []string{"10.96.0.250"}}},
		[]string{"10.96.0.10", "10.96.0.11:5353"},
	)

	// inline DNS table: the wildcard mesh domain -> sentinel VIP
	domains := cfg.GetServerConfig().GetInlineDnsTable().GetVirtualDomains()
	require.Len(t, domains, 1)
	assert.Equal(t, "*.aether.internal", domains[0].GetName())
	assert.Equal(t, []string{"10.96.0.250"}, domains[0].GetEndpoint().GetAddressList().GetAddress())

	// upstream resolvers: bare host -> :53, explicit port preserved
	resolvers := cfg.GetClientConfig().GetUpstreamResolvers()
	require.Len(t, resolvers, 2)
	assert.Equal(t, uint32(53), resolvers[0].GetSocketAddress().GetPortValue())
	assert.Equal(t, "10.96.0.10", resolvers[0].GetSocketAddress().GetAddress())
	assert.Equal(t, uint32(5353), resolvers[1].GetSocketAddress().GetPortValue())
}

func TestGenerateDNSListener(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateDNSListener(pod, 15053,
		[]DNSVirtualDomain{{Domain: "*.aether.internal", Addresses: []string{"10.96.0.250"}}},
		[]string{"10.96.0.10"})
	require.NoError(t, err)

	assert.Equal(t, "dns_p1", l.GetName())
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, corev3.SocketAddress_UDP, sa.GetProtocol())
	assert.Equal(t, uint32(15053), sa.GetPortValue())
	assert.Equal(t, "/var/run/netns/p1", sa.GetNetworkNamespaceFilepath())

	require.Len(t, l.GetListenerFilters(), 1)
	assert.Equal(t, listenerFilterDNSName, l.GetListenerFilters()[0].GetName())
	var fc dns_filterv3.DnsFilterConfig
	require.NoError(t, l.GetListenerFilters()[0].GetTypedConfig().UnmarshalTo(&fc))
	require.Len(t, fc.GetServerConfig().GetInlineDnsTable().GetVirtualDomains(), 1)
}

func TestGenerateDNSListener_RequiresNetns(t *testing.T) {
	_, err := GenerateDNSListener(&cniv1.CNIPod{Name: "p1"}, 15053, nil, nil)
	assert.Error(t, err)
}
