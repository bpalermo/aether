package proxy

import (
	"fmt"
	"regexp"
	"sync"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onDemandRegex returns the :authority regex of a catch-all's cluster_header
// route, and "" when it has none.
func onDemandRegex(vh *routev3.VirtualHost) string {
	for _, r := range vh.GetRoutes() {
		if r.GetRoute().GetClusterHeader() == "" {
			continue
		}
		for _, h := range r.GetMatch().GetHeaders() {
			if h.GetName() == ":authority" {
				return h.GetStringMatch().GetSafeRegex().GetRegex()
			}
		}
	}
	return ""
}

// TestCaptureCatchAll_OnDemandNamesOnlyTheDialledPort: in the table of dialled
// port P, the only authority that may NAME a cluster (cluster_header on
// :authority) is a mesh name that spells exactly ":P". Anything looser is
// #1682 through the cold path: "<svc>.<ns>.<mesh>:9090" dialled on 8080 would
// fetch and reach the cluster of 9090, and a port-less name would reach the
// default cluster whatever port was dialled.
func TestCaptureCatchAll_OnDemandNamesOnlyTheDialledPort(t *testing.T) {
	for _, mesh := range []string{"aether.internal", "mesh.example.com"} {
		vh := BuildCaptureCatchAllVirtualHost(mesh, 8080, false, true)
		raw := onDemandRegex(vh)
		require.NotEmpty(t, raw, "the table of a port has an on-demand route")
		re := regexp.MustCompile(raw)

		assert.True(t, re.MatchString("svc.ns."+mesh+":8080"), raw)
		for _, no := range []string{
			"svc.ns." + mesh,           // port-less: not in a real port's table
			"svc.ns." + mesh + ":9090", // another port
			"svc.ns." + mesh + ":80800",
			"svc.ns." + mesh + ":808",
			"svc.ns." + mesh + ":8080:8080",
			"svc.ns." + mesh + ":8080/",
			"svc." + mesh + ":8080",                 // one label: not a mesh name
			"a.svc.ns." + mesh + ":8080",            // three labels
			"svc.ns.svc.cluster.local:8080",         // a Kubernetes name is never named on demand
			"svc.ns:8080",                           //
			"x.svc.ns." + mesh + ".evil.test:8080",  // the mesh domain as an infix
			"svc.ns.evil-" + mesh + ":8080",         //
			"svc.ns." + mesh + ".evil.test:8080",    // the mesh domain as a prefix of another
			"SVC.NS." + mesh + ":8080",              // authorities are lower case on the wire; no case folding here
			"svc.ns." + mesh + ":8080\nx.y." + mesh, // no multi-line trick
		} {
			assert.False(t, re.MatchString(no), "%q must not name a cluster in the table of port 8080 (regex %s)", no, raw)
		}

		// The resolver agrees: the exact spelling resolves to itself, the
		// others fall to the passthrough.
		got, _ := resolveCatchAll(vh, "svc.ns."+mesh+":8080", "/")
		assert.Equal(t, "svc.ns."+mesh+":8080", got)
		for _, authority := range []string{"svc.ns." + mesh, "svc.ns." + mesh + ":9090"} {
			got, _ = resolveCatchAll(vh, authority, "/")
			assert.Equal(t, PassthroughClusterName, got, authority)
		}
	}

	// The dots of the mesh domain are literal: "aetherXinternal" is not it.
	re := regexp.MustCompile(onDemandRegex(BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, true)))
	assert.False(t, re.MatchString("svc.ns.aetherXinternal:8080"))
	assert.False(t, regexp.MustCompile(MeshAuthorityRegex("a.b", ":80")).MatchString("s.n.aXb:80"))
	assert.True(t, regexp.MustCompile(MeshAuthorityRegex("a.b", ":80")).MatchString("s.n.a.b:80"))
}

// TestCaptureCatchAll_LegacyTablesAlsoAcceptThePortlessName: in the two legacy
// tables (the mesh HTTP port, and port 80) a port-less mesh name is the legacy
// spelling of the service's default cluster and may name it too. Still no
// other port.
func TestCaptureCatchAll_LegacyTablesAlsoAcceptThePortlessName(t *testing.T) {
	for _, port := range []uint32{80, 18081} {
		vh := BuildCaptureCatchAllVirtualHost("aether.internal", port, true, true)
		re := regexp.MustCompile(onDemandRegex(vh))
		suffix := ":80"
		if port == 18081 {
			suffix = ":18081"
		}
		assert.True(t, re.MatchString("svc.ns.aether.internal"), "port %d", port)
		assert.True(t, re.MatchString("svc.ns.aether.internal"+suffix), "port %d", port)
		assert.False(t, re.MatchString("svc.ns.aether.internal:9090"), "port %d", port)
		assert.False(t, re.MatchString("svc.ns.aether.internal:"), "port %d", port)
		assert.False(t, re.MatchString("svc.ns.aether.internal"+suffix+"0"), "port %d", port)
	}
	// Negative control: the same port WITHOUT the legacy flag refuses the
	// port-less name.
	strict := regexp.MustCompile(onDemandRegex(BuildCaptureCatchAllVirtualHost("aether.internal", 80, false, true)))
	assert.False(t, strict.MatchString("svc.ns.aether.internal"))
	assert.True(t, strict.MatchString("svc.ns.aether.internal:80"))
}

// TestCaptureCatchAll_OtherTableNamesNoCluster: cap_http_other is HTTP on a
// port number no service declares as HTTP. No cluster can be named by such a
// port, so the table has no cluster_header route at all, whatever the flags.
func TestCaptureCatchAll_OtherTableNamesNoCluster(t *testing.T) {
	for _, portless := range []bool{false, true} {
		for _, redirectAll := range []bool{false, true} {
			vh := BuildCaptureCatchAllVirtualHost("aether.internal", 0, portless, redirectAll)
			assert.Empty(t, onDemandRegex(vh))
			for _, r := range vh.GetRoutes() {
				assert.Empty(t, r.GetRoute().GetClusterHeader(), "no route of cap_http_other may take its cluster from a header")
			}
			require.Len(t, vh.GetRoutes(), 2, "liveness + fallthrough")
			for _, authority := range []string{"svc.ns.aether.internal", "svc.ns.aether.internal:7777", "svc.ns.aether.internal:8080"} {
				got, status := resolveCatchAll(vh, authority, "/")
				if redirectAll {
					assert.Equal(t, PassthroughClusterName, got, authority)
				} else {
					assert.Equal(t, uint32(404), status, authority)
				}
			}
		}
	}
}

// TestCaptureCatchAll_Shape: the liveness route is first (it wins for its path
// whatever the authority), the fallthrough is last, and it is the passthrough
// with redirect-all and a hard 404 without.
func TestCaptureCatchAll_Shape(t *testing.T) {
	for _, port := range []uint32{0, 80, 8080} {
		for _, redirectAll := range []bool{false, true} {
			vh := BuildCaptureCatchAllVirtualHost("aether.internal", port, port == 80, redirectAll)
			assert.Equal(t, []string{"*"}, vh.GetDomains())
			routes := vh.GetRoutes()
			require.NotEmpty(t, routes)
			assert.Equal(t, MeshLivePath, routes[0].GetMatch().GetPath(), "liveness is an exact-path match and comes first")
			assert.Equal(t, uint32(200), routes[0].GetDirectResponse().GetStatus())
			// It wins even for an authority the on-demand route would take.
			_, status := resolveCatchAll(vh, "svc.ns.aether.internal:8080", MeshLivePath)
			assert.Equal(t, uint32(200), status)

			last := routes[len(routes)-1]
			assert.Empty(t, last.GetMatch().GetHeaders(), "the fallthrough matches every authority")
			if redirectAll {
				assert.Equal(t, PassthroughClusterName, last.GetRoute().GetCluster())
				assert.Nil(t, last.GetDirectResponse())
			} else {
				assert.Equal(t, uint32(404), last.GetDirectResponse().GetStatus())
				assert.Nil(t, last.GetRoute())
			}
		}
	}
}

// TestOutboundCatchAll_NamesAClusterOnlyFromAnAuthorityWithAPort: on the
// loopback (out_http) table nothing is dialled, so the Host header is the only
// address there is and a mesh authority with ANY port may name its cluster,
// "<fqdn>:<port>", which is a port's cluster. A mesh authority WITHOUT a port
// names nothing: it would spell the bare cluster name, which is no port's (a
// name without a port is port 80, and a service the node knows has a virtual
// host that says so). It falls through like a name that is no mesh name.
func TestOutboundCatchAll_NamesAClusterOnlyFromAnAuthorityWithAPort(t *testing.T) {
	rc := BuildOutboundRouteConfiguration(nil, "aether.internal", nil)
	require.Len(t, rc.GetVirtualHosts(), 1)
	vh := rc.GetVirtualHosts()[0]
	for _, authority := range []string{"svc.ns.aether.internal:8080", "svc.ns.aether.internal:9090", "svc.ns.aether.internal:80"} {
		got, _ := resolveCatchAll(vh, authority, "/")
		assert.Equal(t, authority, got)
	}
	got, status := resolveCatchAll(vh, "svc.ns.aether.internal", "/")
	assert.Empty(t, got, "a port-less mesh authority names no cluster")
	assert.Equal(t, uint32(404), status)
	_, status = resolveCatchAll(vh, "example.com", "/")
	assert.Equal(t, uint32(404), status, "the outbound table never passes through")
	_, status = resolveCatchAll(vh, "svc.ns.svc.cluster.local:8080", "/")
	assert.Equal(t, uint32(404), status, "and names no cluster for a Kubernetes name")

	// The proxy port's capture table has the same floor, with or without
	// passthrough: a port-less mesh authority names no cluster there either.
	for _, passthrough := range []bool{false, true} {
		rc := BuildProxyPortCaptureRouteConfiguration("cap_http_18081", nil, "aether.internal", passthrough, nil)
		require.Len(t, rc.GetVirtualHosts(), 1)
		vh := rc.GetVirtualHosts()[0]
		got, _ := resolveCatchAll(vh, "svc.ns.aether.internal:8080", "/")
		assert.Equal(t, "svc.ns.aether.internal:8080", got)
		got, _ = resolveCatchAll(vh, "svc.ns.aether.internal", "/")
		assert.NotEqual(t, "svc.ns.aether.internal", got, "passthrough=%v: a port-less mesh authority names the bare cluster", passthrough)
	}
}

// TestOnDemandCatchAll_NeverParksAnAuthorityThatNamesAMeshPort: 18081 and
// 18082 are ports of no service, so "<fqdn>:18081" names a cluster that can
// never exist. On the two tables whose on-demand route takes the port from the
// authority it is refused at once, for a name nothing is known of as well:
// never parked, so nothing that enters the node's scope meanwhile can answer
// it. Every other port is still offered on demand, and the liveness path
// still answers under any authority.
func TestOnDemandCatchAll_NeverParksAnAuthorityThatNamesAMeshPort(t *testing.T) {
	tables := map[string]*routev3.RouteConfiguration{
		"out_http":                    BuildOutboundRouteConfiguration(nil, "aether.internal", nil),
		"cap_http_18081":              BuildProxyPortCaptureRouteConfiguration("cap_http_18081", nil, "aether.internal", false, nil),
		"cap_http_18081, passthrough": BuildProxyPortCaptureRouteConfiguration("cap_http_18081", nil, "aether.internal", true, nil),
	}
	for name, rc := range tables {
		vh := rc.GetVirtualHosts()[len(rc.GetVirtualHosts())-1]
		for _, authority := range []string{"svc.ns.aether.internal:18081", "svc.ns.aether.internal:18082"} {
			cluster, status := resolveCatchAll(vh, authority, "/")
			assert.Emptyf(t, cluster, "%s: %s names a cluster", name, authority)
			assert.Equalf(t, uint32(421), status, "%s: %s", name, authority)
		}
		var refusal *routev3.Route
		for _, r := range vh.GetRoutes() {
			if r.GetDirectResponse().GetStatus() == 421 {
				refusal = r
			}
		}
		require.NotNil(t, refusal, name)
		assert.Equal(t, RefusalPortNotDeclared, refusal.GetResponseHeadersToAdd()[0].GetHeader().GetValue())
		assert.Equal(t, RefusalReservedPortBody, refusal.GetDirectResponse().GetBody().GetInlineString())

		for _, authority := range []string{"svc.ns.aether.internal:8080", "svc.ns.aether.internal:1808", "svc.ns.aether.internal:180811"} {
			cluster, _ := resolveCatchAll(vh, authority, "/")
			assert.Equalf(t, authority, cluster, "%s: another port is offered on demand", name)
		}
		_, status := resolveCatchAll(vh, "svc.ns.aether.internal:18081", MeshLivePath)
		assert.Equalf(t, uint32(200), status, "%s: the liveness path answers under any authority", name)
		// A name that is not a mesh name is not the mesh's to refuse.
		_, status = resolveCatchAll(vh, "example.com:18081", "/")
		assert.NotEqualf(t, uint32(421), status, "%s", name)
	}
}

// TestBuildRefusalVirtualHost: a refusal is a direct 421 with the reason in
// x-aether-error and the body given. It reaches no cluster, on any path.
func TestBuildRefusalVirtualHost(t *testing.T) {
	reasons := []string{RefusalNoHTTPPort, RefusalPortMismatch, RefusalPortNotHTTP, RefusalPortNotDeclared, RefusalPortRequired}
	assert.Equal(t, []string{"no-http-port", "port-mismatch", "port-not-http", "port-not-declared", "port-required"}, reasons,
		"the values are read by clients and by the e2e suite")
	for _, reason := range reasons {
		vh := BuildRefusalVirtualHost("svc|"+reason, []string{"svc.ns", "svc.ns:*"}, reason, "the body\n")
		assert.Equal(t, "svc|"+reason, vh.GetName())
		assert.Equal(t, []string{"svc.ns", "svc.ns:*"}, vh.GetDomains())
		require.Len(t, vh.GetRoutes(), 1)
		r := vh.GetRoutes()[0]
		assert.Equal(t, "/", r.GetMatch().GetPrefix(), "every path is refused")
		assert.Empty(t, r.GetMatch().GetHeaders())
		require.NotNil(t, r.GetDirectResponse())
		assert.Nil(t, r.GetRoute(), "a refusal reaches no cluster")
		assert.Equal(t, uint32(421), r.GetDirectResponse().GetStatus())
		assert.Equal(t, "the body\n", r.GetDirectResponse().GetBody().GetInlineString())
		require.Len(t, r.GetResponseHeadersToAdd(), 1)
		assert.Equal(t, "x-aether-error", r.GetResponseHeadersToAdd()[0].GetHeader().GetKey())
		assert.Equal(t, reason, r.GetResponseHeadersToAdd()[0].GetHeader().GetValue())
	}
	// The no-http-port builder is the same refusal with its own body.
	vh := BuildNoHTTPPortVirtualHost("redis.ns.mesh", []string{"redis.ns.mesh"}, "redis.ns.mesh:18082")
	assert.Equal(t, uint32(421), vh.GetRoutes()[0].GetDirectResponse().GetStatus())
	assert.Equal(t, RefusalNoHTTPPort, vh.GetRoutes()[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue())
	assert.Contains(t, vh.GetRoutes()[0].GetDirectResponse().GetBody().GetInlineString(), "redis.ns.mesh:18082")
}

// TestBuildServiceVirtualHost_NameIsNotTheCluster: in a per-port table a
// service has several virtual hosts, named apart, and port P is served by the
// cluster of port P. The virtual host's NAME must never be used as a cluster:
// a request no rule sends elsewhere goes to cluster.
func TestBuildServiceVirtualHost_NameIsNotTheCluster(t *testing.T) {
	const name, cluster = "svc.ns.mesh|9090", "svc.ns.mesh:9090"
	domains := []string{"svc.ns", "svc.ns:9090"}

	clustersOf := func(vh *routev3.VirtualHost) (out []string) {
		for _, r := range vh.GetRoutes() {
			if c := r.GetRoute().GetCluster(); c != "" {
				out = append(out, c)
			}
			for _, w := range r.GetRoute().GetWeightedClusters().GetClusters() {
				out = append(out, w.GetName())
			}
		}
		return out
	}

	plain := BuildServiceVirtualHost(name, cluster, domains, nil)
	assert.Equal(t, name, plain.GetName())
	assert.Equal(t, domains, plain.GetDomains())
	assert.Equal(t, []string{cluster}, clustersOf(plain))

	rules := []GammaRoute{
		{Matches: []GammaMatch{{Prefix: "/v2"}}, Backends: []GammaBackend{{Cluster: "v2.ns.mesh:9090", Weight: 1}}},
		{Matches: []GammaMatch{{Prefix: "/own"}}}, // no backend: the virtual host's own cluster
		{Matches: []GammaMatch{{Prefix: "/split"}}, Backends: []GammaBackend{{Cluster: "a.ns.mesh", Weight: 1}, {Cluster: "b.ns.mesh:9090", Weight: 3}}},
	}
	ruled := BuildServiceVirtualHost(name, cluster, domains, rules)
	assert.Equal(t, name, ruled.GetName())
	got := clustersOf(ruled)
	assert.ElementsMatch(t, []string{"v2.ns.mesh:9090", cluster, "a.ns.mesh", "b.ns.mesh:9090", cluster}, got)
	assert.NotContains(t, got, name, "the virtual host's name is not a cluster")
	last := ruled.GetRoutes()[len(ruled.GetRoutes())-1]
	assert.Equal(t, "/", last.GetMatch().GetPrefix())
	assert.Equal(t, cluster, last.GetRoute().GetCluster(), "what no rule matches goes to the port's own cluster")

	// The outbound builder is the same thing with name == cluster, unchanged.
	out := BuildOutboundServiceVirtualHost("svc.ns.mesh", []string{"svc.ns.mesh"}, rules)
	assert.Equal(t, "svc.ns.mesh", out.GetName())
	assert.Equal(t, "svc.ns.mesh", out.GetRoutes()[len(out.GetRoutes())-1].GetRoute().GetCluster())
}

// TestInboundChains_RawTCPDeclaredPortWithAnHTTPSecondary is the destination
// side of the mixed shape #1672 reported: `protocol: tcp`, port 9000, ports
// "9000,8080=h1". Every port has an explicit SNI chain of its own class
// (proposal 043, Phase 1); the two no-SNI chains stay for today's sources.
func TestInboundChains_RawTCPDeclaredPortWithAnHTTPSecondary(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name: "app-0", Namespace: "aether-test",
		Annotations: map[string]string{
			aetherannotations.AnnotationEndpointProtocol: aetherannotations.ProtocolTCP,
			aetherannotations.AnnotationEndpointPort:     "9000",
			aetherannotations.AnnotationEndpointPorts:    "9000,8080=h1",
		},
	}
	chains := buildInboundFilterChains(pod, "c", "v", "example.org", false, nil, nil)

	last := func(name string) string {
		c := chainByName(chains, name)
		require.NotNil(t, c, name)
		return c.GetFilters()[len(c.GetFilters())-1].GetName()
	}

	// 9000: a tcp_proxy chain selected by SNI "9000", to the 9000 app cluster.
	tcp := chainByName(chains, "in_tcp_aether-test_app-0_9000")
	require.NotNil(t, tcp)
	assert.Equal(t, []string{"9000"}, tcp.GetFilterChainMatch().GetServerNames())
	require.NotNil(t, chainTCPProxy(tcp))
	assert.Equal(t, AppClusterName(pod, 9000), chainTCPProxy(tcp).GetCluster())

	// 8080: an HCM chain selected by SNI "8080".
	assert.Equal(t, "envoy.http_connection_manager", last("in_aether-test_app-0_8080"))
	assert.Equal(t, []string{"8080"}, chainByName(chains, "in_aether-test_app-0_8080").GetFilterChainMatch().GetServerNames())

	// No HCM anywhere behind SNI "9000", and no tcp_proxy behind SNI "8080":
	// a port is one class.
	for _, c := range chains {
		sn := c.GetFilterChainMatch().GetServerNames()
		kind := c.GetFilters()[len(c.GetFilters())-1].GetName()
		if slices1(sn, "9000") {
			assert.Equal(t, "envoy.filters.network.tcp_proxy", kind, "chain %s", c.GetName())
		}
		if slices1(sn, "8080") {
			assert.Equal(t, "envoy.http_connection_manager", kind, "chain %s", c.GetName())
		}
	}
	assert.Nil(t, chainByName(chains, "in_aether-test_app-0_9000"), "the HCM chain of the raw-TCP port is gone")

	// The no-SNI chains stay: the default floor (to the declared port) and
	// the h2 chain.
	floor := chainByName(chains, "in_tcp_aether-test_app-0")
	require.NotNil(t, floor)
	assert.Nil(t, floor.GetFilterChainMatch(), "the floor is the default chain")
	assert.Equal(t, AppClusterName(pod, 9000), chainTCPProxy(floor).GetCluster())
	h2 := chainByName(chains, "in_aether-test_app-0")
	require.NotNil(t, h2)
	assert.Equal(t, []string{"h2"}, h2.GetFilterChainMatch().GetApplicationProtocols())
	assert.Empty(t, h2.GetFilterChainMatch().GetServerNames())

	require.Len(t, chains, 4)
	assert.Empty(t, duplicateMatches(chains))

	// Negative control: the same pod with 9000 declared HTTP has an HCM chain
	// for it and no tcp_proxy one.
	pod.Annotations[aetherannotations.AnnotationEndpointProtocol] = aetherannotations.ProtocolHTTP
	httpChains := buildInboundFilterChains(pod, "c", "v", "example.org", false, nil, nil)
	assert.Nil(t, chainByName(httpChains, "in_tcp_aether-test_app-0_9000"))
	assert.NotNil(t, chainByName(httpChains, "in_aether-test_app-0_9000"))
}

func slices1(s []string, v string) bool { return len(s) == 1 && s[0] == v }

// TestAppTCPPorts_IncludesTheDeclaredPort: every port declared raw TCP, the
// one in endpoint.aether.io/port included, and nothing else.
func TestAppTCPPorts_IncludesTheDeclaredPort(t *testing.T) {
	pod := func(annotations map[string]string) *cniv1.CNIPod {
		return &cniv1.CNIPod{Name: "p", Namespace: "n", Annotations: annotations}
	}
	keys := func(m map[uint16]struct{}) []uint16 {
		out := make([]uint16, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	assert.ElementsMatch(t, []uint16{9000}, keys(appTCPPorts(pod(map[string]string{
		aetherannotations.AnnotationEndpointProtocol: aetherannotations.ProtocolTCP,
		aetherannotations.AnnotationEndpointPort:     "9000",
		aetherannotations.AnnotationEndpointPorts:    "9000,8080=h1",
	}))))
	assert.ElementsMatch(t, []uint16{9000, 9001}, keys(appTCPPorts(pod(map[string]string{
		aetherannotations.AnnotationEndpointPort:  "9000",
		aetherannotations.AnnotationEndpointPorts: "9000=tcp,9001=tcp,8080",
	}))))
	assert.Empty(t, appTCPPorts(pod(map[string]string{
		aetherannotations.AnnotationEndpointPort:  "8080",
		aetherannotations.AnnotationEndpointPorts: "8080,9090=h2",
	})), "an HTTP-only pod has no raw-TCP port")
	assert.Empty(t, appTCPPorts(pod(map[string]string{
		aetherannotations.AnnotationEndpointProtocol: aetherannotations.ProtocolUDP,
		aetherannotations.AnnotationEndpointPort:     "5353",
	})), "a UDP port is not a raw-TCP port")
	assert.Empty(t, appTCPPorts(pod(map[string]string{
		aetherannotations.AnnotationEndpointPorts: "8080,9000=quic",
	})), "annotations that do not parse yield no raw-TCP port rather than a guess")
}

// TestNewAppDeliveryClusters_ProbeFollowsTheProbePortsClass: the delegated
// liveness probe is on the port in endpoint.aether.io/port and takes THAT
// port's class, which is the pod-level annotation only when the port carries
// no suffix of its own (endpointmeta.PortProtocols). A pod declaring
// `ports: "9000=tcp,8080"` with port 9000 and the default pod-level protocol
// has a raw-TCP probe port; it was probed with an HTTP GET, which a raw-TCP
// listener never answers, so the endpoint was never promoted. A sibling of
// #1672: the same question, "what does this port speak", answered from a
// pod-level field.
func TestNewAppDeliveryClusters_ProbeFollowsTheProbePortsClass(t *testing.T) {
	pod := func(annotations map[string]string) *cniv1.CNIPod {
		return &cniv1.CNIPod{
			Name: "p-0", Namespace: "aether-test", NetworkNamespace: "/var/run/netns/x",
			Annotations: annotations,
		}
	}
	isHTTPProbe := func(c *clusterv3.Cluster) bool {
		return c.GetHealthChecks()[0].GetHttpHealthCheck() != nil
	}

	// The suffix on the probe port decides, over the pod-level default.
	_, health := NewAppDeliveryClusters(pod(map[string]string{
		aetherannotations.AnnotationEndpointPort:  "9000",
		aetherannotations.AnnotationEndpointPorts: "9000=tcp,8080",
	}), "")
	require.NotNil(t, health)
	assert.False(t, isHTTPProbe(health), "the probe port is declared raw TCP: a TCP connect, not an HTTP GET")

	_, health = NewAppDeliveryClusters(pod(map[string]string{
		aetherannotations.AnnotationEndpointProtocol: aetherannotations.ProtocolTCP,
		aetherannotations.AnnotationEndpointPort:     "8080",
		aetherannotations.AnnotationEndpointPorts:    "8080=h1,9000",
	}), "")
	require.NotNil(t, health)
	assert.True(t, isHTTPProbe(health), "the probe port is declared HTTP although the pod-level default is tcp")

	// Both orders of a mixed pod whose probe port takes the pod-level protocol.
	_, health = NewAppDeliveryClusters(pod(map[string]string{
		aetherannotations.AnnotationEndpointProtocol: aetherannotations.ProtocolTCP,
		aetherannotations.AnnotationEndpointPort:     "9000",
		aetherannotations.AnnotationEndpointPorts:    "9000,8080=h1",
	}), "")
	assert.False(t, isHTTPProbe(health))
	_, health = NewAppDeliveryClusters(pod(map[string]string{
		aetherannotations.AnnotationEndpointPort:  "8080",
		aetherannotations.AnnotationEndpointPorts: "8080,9000=tcp",
	}), "")
	assert.True(t, isHTTPProbe(health))

	// Annotations that do not parse degrade to an HTTP probe, as before.
	_, health = NewAppDeliveryClusters(pod(map[string]string{
		aetherannotations.AnnotationEndpointPort:  "9000",
		aetherannotations.AnnotationEndpointPorts: "9000=quic",
	}), "")
	assert.True(t, isHTTPProbe(health))
}

// TestEdgeUpstreamTCPPortTransportSocket: the edge's per-port raw-TCP socket
// carries the port as SNI and no ALPN; with an empty SNI it is the floor's.
func TestEdgeUpstreamTCPPortTransportSocket(t *testing.T) {
	withPort := EdgeUpstreamTCPPortTransportSocket("spiffe://td/ns/a/sa/edge", "ROOTCA", []string{"spiffe://td/ns/b/sa/svc"}, "9000")
	assert.Contains(t, withPort.String(), `sni:"9000"`)
	assert.NotContains(t, withPort.String(), "alpn_protocols", "a raw-TCP socket advertises no ALPN, or the destination demuxes it to an HCM")

	floor := EdgeUpstreamTCPTransportSocket("spiffe://td/ns/a/sa/edge", "ROOTCA", []string{"spiffe://td/ns/b/sa/svc"})
	assert.NotContains(t, floor.String(), "sni:", "the floor carries no SNI (#306)")
	assert.Equal(t, floor.String(), EdgeUpstreamTCPPortTransportSocket("spiffe://td/ns/a/sa/edge", "ROOTCA", []string{"spiffe://td/ns/b/sa/svc"}, "").String())
}

// TestEveryTableClaimsADomainOnce: the proxy rejects a route table in which
// two virtual hosts claim one authority, and it rejects the WHOLE table. So
// every builder of a table drops the later claim, whatever handed it two: the
// first virtual host keeps the authority, the loser keeps its other domains
// (or goes, when it has none), the inputs are not written to, and the
// duplicate is reported.
func TestEveryTableClaimsADomainOnce(t *testing.T) {
	type dup struct{ table, domain, kept, dropped string }
	var mu sync.Mutex
	var reported []dup
	OnDuplicateDomain(func(table, domain, kept, dropped string) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, dup{table, domain, kept, dropped})
	})
	t.Cleanup(func() { OnDuplicateDomain(func(string, string, string, string) {}) })

	vh := func(name string, domains ...string) *routev3.VirtualHost {
		return BuildRefusalVirtualHost(name, domains, RefusalPortNotDeclared, "x")
	}
	in := func() []*routev3.VirtualHost {
		return []*routev3.VirtualHost{
			vh("a", "echo.demo.aether.internal:8080", "echo.demo.aether.internal"),
			vh("b", "echo.demo.aether.internal:8080", "echo.demo.aether.internal:9090"),
			vh("c", "echo.demo.aether.internal:8080"),
			vh("d", "other.demo.aether.internal:8080"),
		}
	}
	check := func(table string, rc *routev3.RouteConfiguration, given []*routev3.VirtualHost) {
		t.Helper()
		seen := map[string]string{}
		names := []string{}
		for _, v := range rc.GetVirtualHosts() {
			names = append(names, v.GetName())
			for _, d := range v.GetDomains() {
				require.Emptyf(t, seen[d], "%s: %q is claimed by %s and %s", table, d, seen[d], v.GetName())
				seen[d] = v.GetName()
			}
		}
		assert.Equalf(t, "a", seen["echo.demo.aether.internal:8080"], "%s: the first claim keeps the authority", table)
		assert.Equalf(t, "b", seen["echo.demo.aether.internal:9090"], "%s: the loser keeps its other domains", table)
		assert.NotContainsf(t, names, "c", "%s: a virtual host with no domain left is dropped", table)
		assert.Containsf(t, names, "d", "%s", table)
		assert.Equal(t, []string{"echo.demo.aether.internal:8080", "echo.demo.aether.internal:9090"}, given[1].GetDomains(), "%s: the input was written to", table)
	}
	given := in()
	check("out_http", BuildOutboundRouteConfiguration(given, "aether.internal", nil), given)
	given = in()
	check("cap_http_18081", BuildProxyPortCaptureRouteConfiguration("cap_http_18081", given, "aether.internal", true, nil), given)
	given = in()
	check("cap_http_8080", BuildCaptureRouteConfiguration("cap_http_8080", given, BuildCaptureCatchAllVirtualHost("aether.internal", 8080, false, true), nil), given)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, reported, 6, "two losers per table, three tables")
	assert.Contains(t, reported, dup{"out_http", "echo.demo.aether.internal:8080", "a", "b"})
	assert.Contains(t, reported, dup{"cap_http_8080", "echo.demo.aether.internal:8080", "a", "c"})

	// A table with no duplicate is handed back as it is.
	clean := []*routev3.VirtualHost{vh("a", "x:1"), vh("b", "x:2")}
	rc := BuildCaptureRouteConfiguration("cap_http_1", clean, nil, nil)
	assert.Same(t, clean[0], rc.GetVirtualHosts()[0])
}

// TestIndependentWeights: behind an invalid share the valid backends' weights
// are rescaled to a total that is coprime to a million (the proxy uses one
// random value for the share's route and for the pick), in their proportions.
func TestIndependentWeights(t *testing.T) {
	weights := func(bs []GammaBackend) []uint32 {
		out := make([]uint32, len(bs))
		for i, b := range bs {
			out[i] = b.Weight
		}
		return out
	}
	mk := func(ws ...uint32) []GammaBackend {
		out := make([]GammaBackend, len(ws))
		for i, w := range ws {
			out[i] = GammaBackend{Cluster: fmt.Sprintf("c%d", i), Weight: w}
		}
		return out
	}
	for _, in := range [][]uint32{{1, 1}, {500000, 500000}, {90, 10}, {1, 2, 3}, {7, 0, 3}, {999999, 1}, {1000000, 1000000, 1000000}} {
		got := weights(independentWeights(mk(in...)))
		var sum, inSum uint64
		for i := range in {
			sum += uint64(got[i])
			inSum += uint64(in[i])
		}
		require.Equalf(t, uint64(independentWeightTotal), sum, "%v -> %v", in, got)
		for i := range in {
			if in[i] == 0 {
				assert.Zerof(t, got[i], "%v -> %v: a backend with weight 0 receives nothing", in, got)
				continue
			}
			want := float64(in[i]) / float64(inSum)
			assert.InDeltaf(t, want, float64(got[i])/float64(sum), 2e-6, "%v -> %v: backend %d keeps its proportion", in, got, i)
		}
	}
	// 999983 has no factor in common with a million.
	for _, p := range []int{2, 5} {
		assert.NotZero(t, independentWeightTotal%p)
	}
	// No pick, no rescaling.
	assert.Equal(t, []uint32{7}, weights(independentWeights(mk(7))))
	assert.Equal(t, []uint32{7, 0}, weights(independentWeights(mk(7, 0))))
	assert.Empty(t, independentWeights(nil))

	// And only behind an invalid share: a rule whose backends are all valid
	// keeps the weights it was given.
	valid, share := splitInvalidBackends(mk(500000, 500000), 0)
	assert.Nil(t, share)
	assert.Equal(t, []uint32{500000, 500000}, weights(valid))
	withInvalid := mk(500000, 500000, 1000000)
	withInvalid[2].Invalid = true
	valid, share = splitInvalidBackends(withInvalid, 0)
	require.NotNil(t, share)
	assert.Equal(t, uint32(500000), share.GetDefaultValue().GetNumerator())
	assert.Equal(t, []uint32{499992, 499991}, weights(valid))
	assert.Equal(t, []uint32{500000, 500000, 1000000}, weights(withInvalid), "the rule's backends were written to")
}
