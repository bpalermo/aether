package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// unmarshalTCPProxy extracts the TcpProxy from a network filter.
func unmarshalTCPProxy(t *testing.T, f *listenerv3.Filter) *tcp_proxyv3.TcpProxy {
	t.Helper()
	require.Equal(t, "envoy.filters.network.tcp_proxy", f.Name)
	typed := f.GetTypedConfig()
	require.NotNil(t, typed)
	var tcp tcp_proxyv3.TcpProxy
	require.NoError(t, proto.Unmarshal(typed.Value, &tcp))
	return &tcp
}

// TestBuildCaptureTCPRouteFilterChain_Passthrough verifies that when no rules are
// provided, the result falls back to the passthrough floor chain (single cluster).
func TestBuildCaptureTCPRouteFilterChain_Passthrough(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:svc-a.aether.internal",
		ClusterIP:   "10.0.0.10",
	}
	chain := BuildCaptureTCPRouteFilterChain(svc, nil)
	require.NotNil(t, chain)
	assert.Equal(t, "cap_tcp_tcp:svc-a.aether.internal", chain.Name)
	// Should have two filters: set_filter_state + tcp_proxy
	require.Len(t, chain.Filters, 2)
	tcp := unmarshalTCPProxy(t, chain.Filters[1])
	// Single-cluster form
	assert.Equal(t, "tcp:svc-a.aether.internal", tcp.GetCluster())
}

// TestBuildCaptureTCPRouteFilterChain_SingleBackend verifies the single-backend
// case emits the TcpProxy_Cluster form (not weighted_clusters).
func TestBuildCaptureTCPRouteFilterChain_SingleBackend(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:svc-a.aether.internal",
		ClusterIP:   "10.0.0.10",
	}
	rules := []L4ServiceRoute{
		{Backends: []L4Backend{{Service: "svc-a", Cluster: "tcp:svc-a.aether.internal", Weight: 1}}},
	}
	chain := BuildCaptureTCPRouteFilterChain(svc, rules)
	require.NotNil(t, chain)
	require.Len(t, chain.Filters, 2)
	tcp := unmarshalTCPProxy(t, chain.Filters[1])
	assert.Equal(t, "tcp:svc-a.aether.internal", tcp.GetCluster())
}

// TestBuildCaptureTCPRouteFilterChain_WeightedBackends verifies that multiple
// backends become a TcpProxy_WeightedClusters with correct weights.
func TestBuildCaptureTCPRouteFilterChain_WeightedBackends(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:svc-b.aether.internal",
		ClusterIP:   "10.0.0.20",
	}
	rules := []L4ServiceRoute{
		{Backends: []L4Backend{
			{Service: "svc-b-v1", Cluster: "tcp:svc-b-v1.aether.internal", Weight: 80},
			{Service: "svc-b-v2", Cluster: "tcp:svc-b-v2.aether.internal", Weight: 20},
		}},
	}
	chain := BuildCaptureTCPRouteFilterChain(svc, rules)
	require.NotNil(t, chain)
	// prefix_ranges should match the ClusterIP/32
	require.NotNil(t, chain.FilterChainMatch)
	require.Len(t, chain.FilterChainMatch.PrefixRanges, 1)
	assert.Equal(t, "10.0.0.20", chain.FilterChainMatch.PrefixRanges[0].AddressPrefix)
	assert.Equal(t, uint32(32), chain.FilterChainMatch.PrefixRanges[0].PrefixLen.GetValue())

	require.Len(t, chain.Filters, 2)
	tcp := unmarshalTCPProxy(t, chain.Filters[1])
	wc := tcp.GetWeightedClusters()
	require.NotNil(t, wc, "expected weighted_clusters form")
	require.Len(t, wc.Clusters, 2)
	assert.Equal(t, "tcp:svc-b-v1.aether.internal", wc.Clusters[0].Name)
	assert.Equal(t, uint32(80), wc.Clusters[0].Weight)
	assert.Equal(t, "tcp:svc-b-v2.aether.internal", wc.Clusters[1].Name)
	assert.Equal(t, uint32(20), wc.Clusters[1].Weight)
}

// TestBuildCaptureTCPRouteFilterChain_DuplicateClustersWeightMerged verifies
// that duplicate clusters across rules have their weights summed.
func TestBuildCaptureTCPRouteFilterChain_DuplicateClustersWeightMerged(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:svc-c.aether.internal",
		ClusterIP:   "10.0.0.30",
	}
	rules := []L4ServiceRoute{
		{Backends: []L4Backend{
			{Cluster: "tcp:svc-c-v1.aether.internal", Weight: 50},
			{Cluster: "tcp:svc-c-v2.aether.internal", Weight: 50},
		}},
		{Backends: []L4Backend{
			{Cluster: "tcp:svc-c-v1.aether.internal", Weight: 10},
		}},
	}
	chain := BuildCaptureTCPRouteFilterChain(svc, rules)
	require.NotNil(t, chain)
	tcp := unmarshalTCPProxy(t, chain.Filters[1])
	wc := tcp.GetWeightedClusters()
	require.NotNil(t, wc)
	// v1: 50+10=60, v2: 50
	byName := make(map[string]uint32)
	for _, c := range wc.Clusters {
		byName[c.Name] = c.Weight
	}
	assert.Equal(t, uint32(60), byName["tcp:svc-c-v1.aether.internal"])
	assert.Equal(t, uint32(50), byName["tcp:svc-c-v2.aether.internal"])
}

// TestBuildCaptureTCPRouteFilterChain_InvalidIP returns nil for invalid IP.
func TestBuildCaptureTCPRouteFilterChain_InvalidIP(t *testing.T) {
	svc := CaptureTCPService{ClusterName: "tcp:svc-x.aether.internal", ClusterIP: "not-an-ip"}
	chain := BuildCaptureTCPRouteFilterChain(svc, nil)
	assert.Nil(t, chain)
}

// TestBuildCaptureTLSRouteFilterChains_Basic verifies that TLSRoute rules produce
// per-SNI filter chains with the correct server_names + ClusterIP prefix_ranges.
func TestBuildCaptureTLSRouteFilterChains_Basic(t *testing.T) {
	svc := CaptureTCPService{
		ClusterName: "tcp:svc-d.aether.internal",
		ClusterIP:   "10.0.1.10",
	}
	rules := []L4ServiceRoute{
		{
			SNIHostnames: []string{"a.example.com", "b.example.com"},
			Backends:     []L4Backend{{Cluster: "tcp:svc-d-v1.aether.internal", Weight: 1}},
		},
		{
			SNIHostnames: []string{"c.example.com"},
			Backends: []L4Backend{
				{Cluster: "tcp:svc-d-v1.aether.internal", Weight: 70},
				{Cluster: "tcp:svc-d-v2.aether.internal", Weight: 30},
			},
		},
	}
	chains := BuildCaptureTLSRouteFilterChains(svc, rules)
	require.Len(t, chains, 2)

	// Chain 0: two SNI names → single backend
	c0 := chains[0]
	require.NotNil(t, c0.FilterChainMatch)
	assert.ElementsMatch(t, []string{"a.example.com", "b.example.com"}, c0.FilterChainMatch.ServerNames)
	require.Len(t, c0.FilterChainMatch.PrefixRanges, 1)
	assert.Equal(t, "10.0.1.10", c0.FilterChainMatch.PrefixRanges[0].AddressPrefix)
	tcp0 := unmarshalTCPProxy(t, c0.Filters[1])
	assert.Equal(t, "tcp:svc-d-v1.aether.internal", tcp0.GetCluster())

	// Chain 1: one SNI → weighted backends
	c1 := chains[1]
	assert.Equal(t, []string{"c.example.com"}, c1.FilterChainMatch.ServerNames)
	tcp1 := unmarshalTCPProxy(t, c1.Filters[1])
	wc := tcp1.GetWeightedClusters()
	require.NotNil(t, wc)
	require.Len(t, wc.Clusters, 2)
	assert.Equal(t, uint32(70), wc.Clusters[0].Weight)
	assert.Equal(t, uint32(30), wc.Clusters[1].Weight)
}

// TestBuildCaptureTLSRouteFilterChains_EmptyRuleSkipped verifies that rules with
// no SNI hostnames or no backends are skipped without panicking.
func TestBuildCaptureTLSRouteFilterChains_EmptyRuleSkipped(t *testing.T) {
	svc := CaptureTCPService{ClusterName: "tcp:svc-e.aether.internal", ClusterIP: "10.0.1.20"}
	rules := []L4ServiceRoute{
		{SNIHostnames: []string{}, Backends: []L4Backend{{Cluster: "tcp:svc-e.aether.internal", Weight: 1}}},
		{SNIHostnames: []string{"x.example.com"}, Backends: nil},
		{SNIHostnames: []string{"y.example.com"}, Backends: []L4Backend{{Cluster: "tcp:svc-e-v2.aether.internal", Weight: 1}}},
	}
	chains := BuildCaptureTLSRouteFilterChains(svc, rules)
	require.Len(t, chains, 1, "only the rule with both SNI and backends should produce a chain")
	assert.Equal(t, []string{"y.example.com"}, chains[0].FilterChainMatch.ServerNames)
}

// TestGenerateUDPCaptureListener_NoRoutes returns nil when udpRoutes is empty.
func TestGenerateUDPCaptureListener_NoRoutes(t *testing.T) {
	l, err := GenerateUDPCaptureListener("pod-1", "/proc/1/ns/net", 18082, nil)
	require.NoError(t, err)
	assert.Nil(t, l, "should return nil when no routes are provided")
}

// TestGenerateUDPCaptureListener_WithRoutes verifies a UDP listener is generated
// with the correct UDP socket address and udp_proxy filter.
func TestGenerateUDPCaptureListener_WithRoutes(t *testing.T) {
	routes := map[string][]L4Backend{
		"svc-f": {{Service: "svc-f", Cluster: "tcp:svc-f.aether.internal", Weight: 1}},
	}
	l, err := GenerateUDPCaptureListener("pod-2", "/proc/2/ns/net", 18082, routes)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "capture_udp_pod-2", l.Name)
	// Protocol must be UDP
	sa := l.GetAddress().GetSocketAddress()
	require.NotNil(t, sa)
	assert.Equal(t, corev3.SocketAddress_UDP, sa.Protocol)
	assert.Equal(t, uint32(18082), sa.GetPortValue())
	// A connection-less UDP listener carries NO filter chains; udp_proxy is a
	// listener filter (Envoy rejects filter_chains on a UDP listener).
	assert.Empty(t, l.FilterChains, "UDP listener must have no filter chains")
	require.Len(t, l.ListenerFilters, 1)
	assert.Equal(t, "envoy.filters.udp_listener.udp_proxy", l.ListenerFilters[0].Name)
}

// TestL4RulesToWeightedClusters_DefaultWeight verifies weight=0 becomes 1.
func TestL4RulesToWeightedClusters_DefaultWeight(t *testing.T) {
	rules := []L4ServiceRoute{
		{Backends: []L4Backend{
			{Cluster: "tcp:svc-g.aether.internal", Weight: 0},
		}},
	}
	clusters := l4RulesToWeightedClusters(rules)
	require.Len(t, clusters, 1)
	assert.Equal(t, uint32(1), clusters[0].Weight, "weight 0 should be treated as 1")
}
