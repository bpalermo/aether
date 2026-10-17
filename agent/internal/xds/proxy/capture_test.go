package proxy

import (
	"fmt"
	"strings"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"

	"aethermesh.dev/agent/internal/xds/config"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCaptureListener(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", Namespace: "default", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false, nil, false, nil)
	require.NoError(t, err)

	assert.Equal(t, "capture_default_p1", l.GetName())
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, "0.0.0.0", sa.GetAddress())
	assert.Equal(t, uint32(15001), sa.GetPortValue())
	assert.Equal(t, "/var/run/netns/p1", sa.GetNetworkNamespaceFilepath())
	// #1007: use_original_dst must be OFF. The field adds only the
	// restored-destination handoff, which looks the target listener up by
	// address string with no netns, so a diverted connection to
	// <endpoint>:18008 ran through whichever pod's inbound listener bound
	// 0.0.0.0:18008 last. The original_dst LISTENER FILTER (asserted first in
	// the list below) is what restores the destination.
	require.NotNil(t, l.GetUseOriginalDst(), "use_original_dst must be set explicitly")
	assert.False(t, l.GetUseOriginalDst().GetValue(), "use_original_dst must be false (#1007)")
	// proposal 038: the capture socket must be transparent so the CNI's
	// prerouting tproxy can assign a diverted packet to it. It pairs with the
	// original_dst listener filter -- transparent for delivery, original_dst
	// for localAddressRestored() on the passthrough cluster.
	assert.True(t, l.GetTransparent().GetValue(), "transparent set (038)")

	// Inspector stall guard: inconclusive first writes (<6B raw TCP, server-first
	// protocols) must CONTINUE to chain matching after 1s instead of being closed at
	// Envoy's 15s default (found via tcp-echo probes: p1..p99 failed, p100+ worked).
	assert.Equal(t, int64(1), l.GetListenerFiltersTimeout().GetSeconds(),
		"listener_filters_timeout must be capped at 1s")
	assert.True(t, l.GetContinueOnListenerFiltersTimeout(),
		"inconclusive sniffs must continue to chain matching, not close")

	// listener filters: original_dst, http_inspector, tls_inspector.
	require.Len(t, l.GetListenerFilters(), 3)
	assert.Equal(t, listenerFilterOriginalDstName, l.GetListenerFilters()[0].GetName())
	assert.Equal(t, listenerFilterHTTPInspectorName, l.GetListenerFilters()[1].GetName())
	assert.Equal(t, listenerFilterTLSInspectorName, l.GetListenerFilters()[2].GetName())

	// No TCP service: the one HTTP chain every listener has, cap_http (for
	// every port, the proxy port included), and cap_tcp_blackhole: scoped mode
	// catches raw TCP to the TCP mesh port that no service chain claimed,
	// before the HCM fabricates a 400 for it (proposal 037). No
	// DefaultFilterChain (withPassthrough=false).
	requireMatcherNamesChains(t, l)
	names := map[string]bool{}
	for _, fc := range l.GetFilterChains() {
		names[fc.GetName()] = true
	}
	assert.Equal(t, map[string]bool{CaptureHTTPChainName: true, "cap_tcp_blackhole": true}, names)
	assert.Nil(t, l.GetDefaultFilterChain(), "no DefaultFilterChain when withPassthrough=false")
	// The HTTP chain serves the route table of its own name.
	for _, name := range []string{CaptureHTTPChainName} {
		filters := chainByName(l.GetFilterChains(), name).GetFilters()
		var hcm http_connection_managerv3.HttpConnectionManager
		require.NoError(t, filters[len(filters)-1].GetTypedConfig().UnmarshalTo(&hcm))
		assert.Equal(t, name, hcm.GetRds().GetRouteConfigName())
	}
}

func TestGenerateCaptureListener_RequiresNetns(t *testing.T) {
	_, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1"}, "spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false, nil, false, nil)
	assert.Error(t, err)
}

// TestGenerateCaptureListener_WithTCPServices verifies that non-HTTP services produce
// per-ClusterIP TCP floor chains in addition to the global HCM catch-all.
func TestGenerateCaptureListener_WithTCPServices(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", Namespace: "default", NetworkNamespace: "/var/run/netns/p1"}
	tcpSvcs := []CaptureTCPService{
		{ClusterName: "tcp:svc-a.aether.internal", ClusterIP: "10.96.1.10", LegacyTCPPort: 9000},
		{ClusterName: "tcp:svc-b.aether.internal", ClusterIP: "10.96.1.20", LegacyTCPPort: 9000},
	}
	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false, tcpSvcs, false, nil)
	require.NoError(t, err)

	// Each service whose legacy target is raw TCP yields TWO chains
	// (proposals 037 and 043): the well-known :18082 spelling and its own
	// port. There is no third: the any-port chain (cap_tcp_anyport_<cluster>)
	// was removed in chart 3.0.0. Plus the HTTP chain and the scoped-mode
	// blackhole.
	requireMatcherNamesChains(t, l)
	names := map[string]bool{}
	for _, fc := range l.GetFilterChains() {
		names[fc.GetName()] = true
	}
	assert.True(t, names["cap_tcp_tcp:svc-a.aether.internal_18082"], "the :18082 spelling")
	assert.True(t, names["cap_tcp_tcp:svc-a.aether.internal_9000"], "the legacy target's own port")
	for name := range names {
		assert.NotContains(t, name, "anyport", "the any-port chain is gone: a raw-TCP service is reached on its own port and on :18082 only")
	}
	assert.True(t, names["cap_tcp_blackhole"])
	assert.True(t, names[CaptureHTTPChainName])
	assert.False(t, names["capture_default_p1"])
	require.Len(t, l.GetFilterChains(), 6, "2 per TCP service + cap_http + blackhole")

	// Each service's address reaches its own chains on its two ports, and
	// every one is a single tcp_proxy behind the source filter states.
	for ip, svc := range map[string]string{"10.96.1.10": "svc-a", "10.96.1.20": "svc-b"} {
		for _, p := range []uint32{9000, 18082} {
			name := sel(t, l, rawConn(ip, p))
			assert.Equal(t, fmt.Sprintf("cap_tcp_tcp:%s.aether.internal_%d", svc, p), name)
			fc := chainByName(l.GetFilterChains(), name)
			require.Len(t, fc.GetFilters(), 3, "netns + cert-mapper identity (#842; the #815 copy retired in #1165) + tcp_proxy")
			assert.Equal(t, "envoy.filters.network.tcp_proxy", fc.GetFilters()[2].GetName())
		}
		// Not HTTP on another port: scoped capture hands the port to cap_http.
		assert.Equal(t, CaptureHTTPChainName, sel(t, l, httpConn(ip, 8080)))
	}
}

// TestGenerateCaptureListener_InvalidTCPService verifies that TCP services with
// empty or unparseable ClusterIPs are silently skipped (no nil chains emitted).
func TestGenerateCaptureListener_InvalidTCPService(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	tcpSvcs := []CaptureTCPService{
		{ClusterName: "tcp:svc-a.aether.internal", ClusterIP: "", LegacyTCPPort: 9000, TCPPorts: []uint32{9100}},           // missing IP
		{ClusterName: "", ClusterIP: "10.96.1.10", LegacyTCPPort: 9000, TCPPorts: []uint32{9100}},                          // missing name
		{ClusterName: "tcp:svc-b.aether.internal", ClusterIP: "not-an-ip", LegacyTCPPort: 9000, TCPPorts: []uint32{9100}},  // bad IP
		{ClusterName: "tcp:svc-c.aether.internal", ClusterIP: "10.96.1.30", LegacyTCPPort: 9000, TCPPorts: []uint32{9100}}, // valid
	}
	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false, tcpSvcs, false, nil)
	require.NoError(t, err)

	// Only the valid service produces chains: its per-port chain, its :18082
	// spelling and its own port, plus the HTTP chain and the scoped-mode
	// blackhole.
	requireMatcherNamesChains(t, l)
	require.Len(t, l.GetFilterChains(), 5)
	for _, fc := range l.GetFilterChains() {
		if strings.HasPrefix(fc.GetName(), "cap_tcp_tcp:") {
			assert.Contains(t, fc.GetName(), "svc-c", "chain %s", fc.GetName())
		}
	}
	assert.Equal(t, []uint32{9000, 9100, 18082}, rawReach(t, l, "10.96.1.30"))
}

// TestGenerateCaptureListener_WithPassthrough verifies that withPassthrough=true adds
// a DefaultFilterChain routing to the passthrough_original_dst cluster, without adding
// an extra named chain (DefaultFilterChain is separate from filter_chains).
func TestGenerateCaptureListener_WithPassthrough(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false, nil, true, nil)
	require.NoError(t, err)

	// Named filter_chains: the one HTTP chain (no TCP services).
	requireMatcherNamesChains(t, l)
	require.Len(t, l.GetFilterChains(), 1, "the HTTP chain, passthrough is in DefaultFilterChain")

	// With passthrough, an HTTP chain takes only cleartext (transport_protocol
	// raw_buffer) so TLS egress (transport_protocol tls) falls to the
	// DefaultFilterChain passthrough instead of failing in the HTTP codec ...
	assert.Equal(t, "cap_passthrough", sel(t, l, tlsConn("10.96.9.9", 443, "example.com")),
		"TLS egress must fall through to the passthrough")
	// ... AND only detected HTTP, so cleartext NON-HTTP (raw TCP to a non-mesh
	// destination) also falls to the passthrough instead of being parsed as
	// HTTP and answered with a fake 400 (#460 e2e finding).
	assert.Equal(t, "cap_passthrough", sel(t, l, rawConn("10.96.9.9", 5432)),
		"raw TCP must pass through, not reach an HTTP chain")
	for _, app := range []string{"http/1.0", "http/1.1", "h2c"} {
		c := httpConn("10.96.9.9", 8080)
		c.App = []string{app}
		assert.Equal(t, CaptureHTTPChainName, sel(t, l, c), app)
	}

	// DefaultFilterChain must be set.
	require.NotNil(t, l.GetDefaultFilterChain(), "DefaultFilterChain must be set when withPassthrough=true")
	dfc := l.GetDefaultFilterChain()
	assert.Equal(t, "cap_passthrough", dfc.GetName())

	// DefaultFilterChain must have a single tcp_proxy filter routing to passthrough_original_dst.
	require.Len(t, dfc.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", dfc.GetFilters()[0].GetName())
	var tp tcp_proxyv3.TcpProxy
	require.NoError(t, dfc.GetFilters()[0].GetTypedConfig().UnmarshalTo(&tp))
	assert.Equal(t, PassthroughClusterName, tp.GetCluster())
	assert.Equal(t, "cap_passthrough", tp.GetStatPrefix())
}

// TestGenerateCaptureListener_WithPassthroughAndTCPServices verifies that named
// TCP floor chains coexist with the DefaultFilterChain passthrough when both are
// enabled. The named chains take precedence (Envoy evaluates them first).
func TestGenerateCaptureListener_WithPassthroughAndTCPServices(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"}
	tcpSvcs := []CaptureTCPService{
		{ClusterName: "tcp:svc-a.aether.internal", ClusterIP: "10.96.1.10", LegacyTCPPort: 9000},
	}
	l, err := GenerateCaptureListener(pod, "spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false, tcpSvcs, true, nil)
	require.NoError(t, err)

	// Named chains: the service's :18082 spelling, its own port, and the
	// HTTP chain. No blackhole here — redirect-all uses passthrough ->
	// kube-proxy REJECT, which is already attributable (proposal 037).
	requireMatcherNamesChains(t, l)
	require.Len(t, l.GetFilterChains(), 3)
	assert.Equal(t, "cap_passthrough", sel(t, l, rawConn("10.96.1.10", 7777)), "another port of the address: the passthrough")
	assert.Equal(t, "cap_tcp_tcp:svc-a.aether.internal_9000", sel(t, l, rawConn("10.96.1.10", 9000)))
	// DefaultFilterChain carries the passthrough.
	require.NotNil(t, l.GetDefaultFilterChain())
	assert.Equal(t, "cap_passthrough", l.GetDefaultFilterChain().GetName())
}

// TestNewPassthroughOriginalDstCluster verifies the passthrough cluster is an
// ORIGINAL_DST cluster with the correct name and no transport socket.
func TestNewPassthroughOriginalDstCluster(t *testing.T) {
	cl := NewPassthroughOriginalDstCluster()
	require.NotNil(t, cl)
	assert.Equal(t, PassthroughClusterName, cl.GetName())
	// Must be ORIGINAL_DST type (4).
	assert.Equal(t, int32(4), int32(cl.GetType()))
	// ORIGINAL_DST REQUIRES CLUSTER_PROVIDED LB — any other policy (the
	// ROUND_ROBIN default) makes Envoy reject the cluster and NACK CDS.
	assert.Equal(t, clusterv3.Cluster_CLUSTER_PROVIDED, cl.GetLbPolicy())
	// No transport socket — passthrough is plaintext.
	assert.Nil(t, cl.GetTransportSocket())

	// USE_DOWNSTREAM_PROTOCOL upstream options (issue #568): sniffed cleartext HTTP
	// (h2c OR http/1.1) transits the cap_http HCM to this cluster; the upstream dial
	// MUST mirror the downstream protocol or an h2c gRPC client to a non-mesh
	// h2-only server resets ("protocol error"). Assert the cluster carries the
	// use-downstream config with BOTH codecs populated.
	tepo := cl.GetTypedExtensionProtocolOptions()
	require.Contains(t, tepo, config.UpstreamHTTPProtocolOptionsKey,
		"passthrough must carry upstream HTTP protocol options to preserve h2c (#568)")
	var protoOpts httpv3.HttpProtocolOptions
	require.NoError(t, tepo[config.UpstreamHTTPProtocolOptionsKey].UnmarshalTo(&protoOpts))
	useDownstream, ok := protoOpts.GetUpstreamProtocolOptions().(*httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig)
	require.True(t, ok, "expected UseDownstreamProtocolConfig, got %T", protoOpts.GetUpstreamProtocolOptions())
	require.NotNil(t, useDownstream.UseDownstreamProtocolConfig)
	assert.NotNil(t, useDownstream.UseDownstreamProtocolConfig.GetHttpProtocolOptions(),
		"http/1.1 downstream must map to h1 upstream")
	assert.NotNil(t, useDownstream.UseDownstreamProtocolConfig.GetHttp2ProtocolOptions(),
		"h2c downstream must map to h2c upstream (the #568 fix)")

	// SO_MARK on the upstream sockets (proposal 022 M2-default): the CNI
	// redirect-all rule RETURNs this mark so the proxy's own forwarded egress is
	// not re-captured. Only socket_options (no source_address) so the bind/netns
	// are unchanged.
	opts := cl.GetUpstreamBindConfig().GetSocketOptions()
	require.Len(t, opts, 1)
	assert.Equal(t, int64(1), opts[0].GetLevel()) // SOL_SOCKET
	assert.Equal(t, int64(36), opts[0].GetName()) // SO_MARK
	assert.Equal(t, int64(meshconst.CapturePassthroughFwMark), opts[0].GetIntValue())
	assert.Equal(t, corev3.SocketOption_STATE_PREBIND, opts[0].GetState())
	assert.Nil(t, cl.GetUpstreamBindConfig().GetSourceAddress(), "no source_address — bind unchanged")
}

// TestBuildCapturePassthroughFilterChain verifies the DefaultFilterChain structure.
func TestBuildCapturePassthroughFilterChain(t *testing.T) {
	fc := BuildCapturePassthroughFilterChain()
	require.NotNil(t, fc)
	assert.Equal(t, "cap_passthrough", fc.GetName())
	// No FilterChainMatch: this is used as DefaultFilterChain, not a named chain.
	assert.Nil(t, fc.GetFilterChainMatch())
	// Single tcp_proxy filter.
	require.Len(t, fc.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", fc.GetFilters()[0].GetName())
	var tp tcp_proxyv3.TcpProxy
	require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(&tp))
	assert.Equal(t, PassthroughClusterName, tp.GetCluster())
}

func TestBuildCaptureRouteConfiguration(t *testing.T) {
	// cluster.local authority -> service cluster, reusing the outbound vhost builder.
	// The service key is the namespace-qualified "<ns>/<svc>" (020 Part 1), rendered
	// as the FQDN authority svc-1.aether-test.aether.internal.
	vh := BuildOutboundClusterVirtualHost(
		ServiceClusterName("aether-test/svc-1", "aether.internal"),
		[]string{"svc-1.aether-test.svc.cluster.local", "svc-1.aether-test.svc.cluster.local:18081"},
	)
	// The table of the mesh HTTP port (a legacy table, so a port-less mesh
	// authority may name its cluster too), scoped capture. One table per
	// dialled port since proposal 043; it was the single "cap_http".
	name := CaptureHTTPRouteName(meshconst.ProxyOutboundPort)
	rc := BuildCaptureRouteConfiguration(name, []*routev3.VirtualHost{vh},
		BuildCaptureCatchAllVirtualHost("aether.internal", meshconst.ProxyOutboundPort, true, false), nil)
	assert.Equal(t, "cap_http_18081", rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 2, "the service vhost + the on-demand catch-all")

	got := rc.GetVirtualHosts()[0]
	assert.Contains(t, got.GetDomains(), "svc-1.aether-test.svc.cluster.local:18081")
	assert.Equal(t, "svc-1.aether-test.aether.internal", got.GetRoutes()[0].GetRoute().GetCluster())

	// The catch-all recovers cold/off-node mesh services via ODCDS (ClusterHeader
	// on :authority) and 404s only non-mesh authorities — symmetric with outbound.
	catchAll := rc.GetVirtualHosts()[1]
	assert.Equal(t, []string{"*"}, catchAll.GetDomains())
	var hasOnDemand, has404 bool
	for _, r := range catchAll.GetRoutes() {
		if r.GetRoute().GetClusterHeader() == ":authority" {
			hasOnDemand = true
		}
		if r.GetDirectResponse().GetStatus() == 404 {
			has404 = true
		}
	}
	assert.True(t, hasOnDemand, "catch-all routes mesh authorities to ODCDS via :authority")
	assert.True(t, has404, "scoped-capture catch-all 404s non-mesh authorities")

	// The table owns its virtual_hosts slice, and a nil catch-all adds none.
	bare := BuildCaptureRouteConfiguration(name, []*routev3.VirtualHost{vh}, nil, nil)
	require.Len(t, bare.GetVirtualHosts(), 1)
}

// TestBuildCaptureRouteConfigurationRedirectAll verifies that with redirect-all
// capture the catch-all's final fallthrough routes to the ORIGINAL_DST passthrough
// (so a captured request to a real Kubernetes Service with no mesh authority and no
// dependency-set vhost still reaches its backend via kube-proxy) instead of 404'ing.
func TestBuildCaptureRouteConfigurationRedirectAll(t *testing.T) {
	rc := BuildCaptureRouteConfiguration(CaptureHTTPRouteName(8080), nil,
		BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, true), nil)
	require.Len(t, rc.GetVirtualHosts(), 1, "just the on-demand catch-all")
	catchAll := rc.GetVirtualHosts()[0]

	var hasOnDemand, hasPassthrough, has404 bool
	for _, r := range catchAll.GetRoutes() {
		if r.GetRoute().GetClusterHeader() == ":authority" {
			hasOnDemand = true
		}
		if r.GetRoute().GetCluster() == PassthroughClusterName {
			hasPassthrough = true
		}
		if r.GetDirectResponse().GetStatus() == 404 {
			has404 = true
		}
	}
	assert.True(t, hasOnDemand, "mesh authorities still resolve via ODCDS")
	assert.True(t, hasPassthrough, "redirect-all fallthrough routes to the ORIGINAL_DST passthrough")
	assert.False(t, has404, "redirect-all catch-all does not hard-404 non-mesh authorities")
}
