package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1672: a service whose PRIMARY port is raw TCP and which also serves an
// HTTP port (`protocol: tcp`, port 9000, ports "9000,8080=h1"). Its pod is
// registered under both protocols with the same row, so the service is in the
// HTTP listing AND the TCP listing.

const (
	tcpFirstSvc  = "aether-test/tcpfirst"
	tcpFirstFQDN = "tcpfirst.aether-test.aether.internal"
	tcpFirstVIP  = "10.96.0.90"
)

// mixedRow is one registry row of a pod serving both protocols.
func mixedRow(ip string, primary uint32, classes map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	ep := makeEndpoint(ip, "cluster-1", "node-2", primary)
	ep.PortProtocols = classes
	ep.Ports = nil
	for _, p := range []uint32{8080, 9000, 9100} {
		if _, ok := classes[p]; ok {
			ep.Ports = append(ep.Ports, p)
		}
	}
	return ep
}

func tcpFirstRow(ip string) *registryv1.ServiceEndpoint {
	return mixedRow(ip, 9000, map[uint32]registryv1.PortProtocol{9000: ppTCP, 8080: ppHTTP})
}

// bothListings serves the same rows under the HTTP and the TCP listing, which
// is what a pod registered once per protocol looks like to the agent.
func bothListings(rows map[string][]*registryv1.ServiceEndpoint) *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_UDP {
				return map[string][]*registryv1.ServiceEndpoint{}, nil
			}
			return rows, nil
		},
	}
}

// newMixedCache is a capture-enabled cache with one local client pod, a node
// identity, and svc declared as an upstream and delivered by the mesh-Service
// reconciler with annotatedTCP as the registrar-stamped primary class.
func newMixedCache(t *testing.T, svc, authority, vip string, annotatedTCP bool) *SnapshotCache {
	t.Helper()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	ctx := context.Background()
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "client-0", Namespace: "aether-test",
		ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client",
	}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, svc)
	c.SetCaptureAuthorities(map[string]string{svc: authority})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: svc, ClusterIP: vip, PrimaryIsTCP: annotatedTCP}})
	return c
}

// captureChains returns the capture listener's per-VIP chains for vip: the
// destination_port-qualified ones by port, and the names of the unqualified
// ones (which match the VIP on every port nothing else claims).
func captureChains(t *testing.T, c *SnapshotCache, vip string) (qualified map[uint32]string, portless []string) {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	qualified = map[uint32]string{}
	found := false
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		l, ok := res.(*listenerv3.Listener)
		if !ok || len(name) < 8 || name[:8] != "capture_" {
			continue
		}
		found = true
		for _, fc := range l.GetFilterChains() {
			m := fc.GetFilterChainMatch()
			var forVIP bool
			for _, r := range m.GetPrefixRanges() {
				forVIP = forVIP || r.GetAddressPrefix() == vip
			}
			if !forVIP {
				continue
			}
			if m.GetDestinationPort() == nil {
				portless = append(portless, fc.GetName())
				continue
			}
			qualified[m.GetDestinationPort().GetValue()] = fc.GetName()
		}
	}
	require.True(t, found, "the client pod has a capture listener")
	return qualified, portless
}

// TestTCPPrimaryWithHTTPPort_FloorChainsFromEndpoints is the measured failure:
// the mesh Service said "http" (the registrar labelled it by the first listing
// it appeared in), the agent took that as the primary's class and built no
// floor, and raw TCP to :18082 and :9000 was never answered.
//
// The class of the primary is now read from the endpoints the cache holds, so
// the floor is built whatever the annotation says -- which is also what makes
// a new agent correct behind a registrar that still stamps the old label.
func TestTCPPrimaryWithHTTPPort_FloorChainsFromEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name         string
		annotatedTCP bool
	}{
		{"mesh Service labelled http (a registrar from before the fix)", false},
		{"mesh Service labelled tcp", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newMixedCache(t, tcpFirstSvc, "tcpfirst.aether-test.svc.cluster.local", tcpFirstVIP, tt.annotatedTCP)
			reg := bothListings(map[string][]*registryv1.ServiceEndpoint{tcpFirstSvc: {tcpFirstRow("10.0.0.16")}})
			require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg))

			qualified, portless := captureChains(t, c, tcpFirstVIP)
			assert.Equal(t, map[uint32]string{
				18082: "cap_tcp_tcp:" + tcpFirstFQDN + "_18082",
				9000:  "cap_tcp_tcp:" + tcpFirstFQDN + "_9000",
			}, qualified, "raw TCP on the TCP mesh port and on the primary port; nothing claims the HTTP port")
			assert.Empty(t, portless,
				"no any-port shim on a VIP that serves HTTP: it would take <VIP>:8080 from the HCM chain")

			clusters := snapshotClusterNames(t, c)
			require.Contains(t, clusters, "tcp:"+tcpFirstFQDN, "the chain's cluster rides the same snapshot")
			require.Contains(t, clusters, "tcp:"+tcpFirstFQDN+":9000")
		})
	}
}

// TestTCPPrimaryWithHTTPPort_HTTPClusters covers the HTTP half of the same
// service. The HTTP pass used to treat the primary port as the default HTTP
// port because the service was in the HTTP listing: it built <fqdn> with
// SNI=9000, so `http://<svc>/` and :18081 reached the raw-TCP port and
// answered 502.
func TestTCPPrimaryWithHTTPPort_HTTPClusters(t *testing.T) {
	c := newMixedCache(t, tcpFirstSvc, "tcpfirst.aether-test.svc.cluster.local", tcpFirstVIP, false)
	reg := bothListings(map[string][]*registryv1.ServiceEndpoint{tcpFirstSvc: {tcpFirstRow("10.0.0.16")}})
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg))

	clusters := snapshotClusterNames(t, c)
	assert.Contains(t, clusters, tcpFirstFQDN+":8080", "the HTTP port keeps its per-port cluster")
	assert.NotContains(t, clusters, tcpFirstFQDN,
		"no default HTTP cluster: the name would address the primary port, which is raw TCP")
	assert.NotContains(t, clusters, tcpFirstFQDN+":9000", "no HTTP cluster for the raw-TCP port")
	assert.NotContains(t, clusters, tcpFirstFQDN+":18081", "and no mesh-port alias of a default cluster that does not exist")

	c.clusterMu.RLock()
	_, bare := c.clusters[tcpFirstSvc]
	floor, hasFloor := c.clusters[proxy.TCPClusterName(tcpFirstSvc, c.meshDomain)]
	c.clusterMu.RUnlock()
	assert.False(t, bare)
	require.True(t, hasFloor)
	require.NotNil(t, floor.loadAssignment, "with no default HTTP entry the floor owns the bare-name load assignment")
	assert.Equal(t, tcpFirstSvc, floor.loadAssignment.GetClusterName())

	// `http://<svc>/` and :18081 name the primary port, over HTTP. This primary
	// is raw TCP, so they name nothing: refused at once and by name, with both
	// working spellings in the body.
	vh := captureVhost(t, c, "node-1", tcpFirstFQDN)
	require.NotNil(t, vh, "the service keeps a cap_http virtual host")
	assert.ElementsMatch(t, []string{
		"tcpfirst.aether-test.svc.cluster.local", "tcpfirst.aether-test.svc.cluster.local:18081",
		tcpFirstFQDN, tcpFirstFQDN + ":18081",
	}, vh.GetDomains(), "the portless and mesh-port authorities only: <fqdn>:8080 is not refused")
	require.Len(t, vh.GetRoutes(), 1)
	dr := vh.GetRoutes()[0].GetDirectResponse()
	require.NotNil(t, dr)
	assert.Equal(t, uint32(421), dr.GetStatus())
	hdrs := vh.GetRoutes()[0].GetResponseHeadersToAdd()
	require.Len(t, hdrs, 1)
	assert.Equal(t, proxy.NoHTTPPortErrorHeader, hdrs[0].GetHeader().GetKey())
	assert.Equal(t, "primary-not-http", hdrs[0].GetHeader().GetValue())
	body := dr.GetBody().GetInlineString()
	assert.Contains(t, body, tcpFirstFQDN+":8080", "names where HTTP is served")
	assert.Contains(t, body, tcpFirstFQDN+":18082", "and how to reach the primary")
	assert.Contains(t, body, tcpFirstFQDN+":9000")
}

// TestHTTPPrimaryWithTCPPort_Unchanged pins the mirror shape, the one the
// nightly suite gates: an HTTP primary with a raw-TCP secondary keeps its
// default HTTP cluster and gets one qualified chain for the TCP port -- and
// does so even when the annotation says tcp, because the endpoints decide.
func TestHTTPPrimaryWithTCPPort_Unchanged(t *testing.T) {
	const svc, fqdn, vip = "aether-test/mixed", "mixed.aether-test.aether.internal", "10.96.0.91"
	for _, annotatedTCP := range []bool{false, true} {
		c := newMixedCache(t, svc, "mixed.aether-test.svc.cluster.local", vip, annotatedTCP)
		row := mixedRow("10.0.0.14", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP})
		require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1",
			bothListings(map[string][]*registryv1.ServiceEndpoint{svc: {row}})))

		qualified, portless := captureChains(t, c, vip)
		assert.Equal(t, map[uint32]string{9000: "cap_tcp_tcp:" + fqdn + "_9000"}, qualified,
			"the TCP port only: :18082 is the primary as raw TCP, and this primary is HTTP")
		assert.Empty(t, portless)

		clusters := snapshotClusterNames(t, c)
		require.Contains(t, clusters, fqdn, "the default HTTP cluster")
		assert.Contains(t, clusters, fqdn+":18081")
		assert.Contains(t, clusters, "tcp:"+fqdn+":9000")
		assert.NotContains(t, clusters, fqdn+":9000",
			"a port declared raw TCP gets no HTTP cluster: per-port membership is by (port, protocol)")

		vh := captureVhost(t, c, "node-1", fqdn)
		require.NotNil(t, vh)
		require.NotEmpty(t, vh.GetRoutes())
		assert.Nil(t, vh.GetRoutes()[0].GetDirectResponse(), "an ordinary virtual host, not a refusal")
	}
}

// TestPureTCPService_KeepsAnyPortShim: a service that is raw TCP throughout
// keeps all three parts of its floor, before and after the cache holds its
// endpoints. Before, the annotation is all there is, and a restarted agent
// must not serve a listener without the chain a running proxy has connections
// on (#1094).
func TestPureTCPService_KeepsAnyPortShim(t *testing.T) {
	const svc, fqdn, vip = "aether-test/echo-tcp", "echo-tcp.aether-test.aether.internal", "10.96.0.92"
	c := newMixedCache(t, svc, "echo-tcp.aether-test.svc.cluster.local", vip, true)

	// A load that returns nothing for the service: the snapshot is built and
	// the cache still holds no endpoint of it.
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1",
		bothListings(map[string][]*registryv1.ServiceEndpoint{})))
	_, portless := captureChains(t, c, vip)
	assert.Equal(t, []string{"cap_tcp_anyport_tcp:" + fqdn}, portless, "from the annotation alone, as before")

	rows := map[string][]*registryv1.ServiceEndpoint{svc: {mixedRow("10.0.0.30", 9000, map[uint32]registryv1.PortProtocol{9000: ppTCP})}}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return rows, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg))

	qualified, portless := captureChains(t, c, vip)
	assert.Equal(t, []string{"cap_tcp_anyport_tcp:" + fqdn}, portless)
	assert.Equal(t, map[uint32]string{
		18082: "cap_tcp_tcp:" + fqdn + "_18082",
		9000:  "cap_tcp_tcp:" + fqdn + "_9000",
	}, qualified)
}

// TestCaptureDerivation covers the map the capture chains are decided from.
func TestCaptureDerivation(t *testing.T) {
	tcpFirst := tcpFirstRow("10.0.0.1")
	httpFirst := mixedRow("10.0.0.2", 8080, map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP})
	pureTCP := mixedRow("10.0.0.3", 9000, map[uint32]registryv1.PortProtocol{9000: ppTCP, 9100: ppTCP})
	legacyTCP := makeEndpoint("10.0.0.4", "cluster-1", "node-2", 9000)

	got := deriveCaptureServices(
		map[string][]*registryv1.ServiceEndpoint{"ns/tcpfirst": {tcpFirst}, "ns/httpfirst": {httpFirst}},
		map[string][]*registryv1.ServiceEndpoint{"ns/tcpfirst": {tcpFirst}, "ns/httpfirst": {httpFirst}, "ns/puretcp": {pureTCP}, "ns/legacy": {legacyTCP}, "ns/empty": {}},
		nil,
	)
	assert.Equal(t, map[string]captureDerivation{
		"ns/tcpfirst":  {primaryIsTCP: true, primaryPort: 9000, servesHTTP: true},
		"ns/httpfirst": {tcpPorts: []uint32{9000}, servesHTTP: true},
		"ns/puretcp":   {primaryIsTCP: true, primaryPort: 9000, tcpPorts: []uint32{9100}},
		"ns/legacy":    {primaryIsTCP: true, primaryPort: 9000},
	}, got, "a service with no endpoint is absent: nothing is known about it")

	// An old kubernetes-backend registrar lists a mixed pod under its pod-level
	// protocol only. The row still says it serves an HTTP port.
	got = deriveCaptureServices(nil, map[string][]*registryv1.ServiceEndpoint{"ns/tcpfirst": {tcpFirst}}, nil)
	assert.True(t, got["ns/tcpfirst"].servesHTTP)
}

// TestCaptureEntryResolution: what an entry does with and without a derivation.
func TestCaptureEntryResolution(t *testing.T) {
	// Nothing known about the service yet: the annotation, exactly as before.
	e := resolveCaptureEntry("ns/svc", "10.96.0.1", true, captureDerivation{}, false)
	assert.True(t, e.primaryIsTCP)
	assert.False(t, e.servesHTTP)
	e = resolveCaptureEntry("ns/svc", "10.96.0.1", false, captureDerivation{}, false)
	assert.False(t, e.primaryIsTCP)

	// Known: the endpoints decide, whatever the annotation says.
	d := captureDerivation{primaryIsTCP: true, primaryPort: 9000, servesHTTP: true}
	e = resolveCaptureEntry("ns/svc", "10.96.0.1", false, d, true)
	assert.True(t, e.primaryIsTCP)
	assert.True(t, e.servesHTTP)
	assert.Equal(t, uint32(9000), e.primaryPort)
	e = resolveCaptureEntry("ns/svc", "10.96.0.1", true, captureDerivation{tcpPorts: []uint32{9000}, servesHTTP: true}, true)
	assert.False(t, e.primaryIsTCP)
	assert.False(t, e.servesHTTP, "servesHTTP only says something about a TCP-primary entry; keeping it false elsewhere keeps an HTTP service's entry from changing")
	assert.Equal(t, []uint32{9000}, e.tcpPorts)

	// The change detector sees the shim being withheld, and nothing else.
	a := []captureTCPEntry{resolveCaptureEntry("ns/svc", "10.96.0.1", true, captureDerivation{primaryIsTCP: true, primaryPort: 9000}, true)}
	b := []captureTCPEntry{resolveCaptureEntry("ns/svc", "10.96.0.1", true, captureDerivation{primaryIsTCP: true, primaryPort: 9000, servesHTTP: true}, true)}
	assert.False(t, equalTCPEntries(a, b), "gaining an HTTP port removes the any-port shim: the listeners must be rebuilt")
	assert.True(t, equalTCPEntries(a, []captureTCPEntry{resolveCaptureEntry("ns/svc", "10.96.0.1", false, captureDerivation{primaryIsTCP: true, primaryPort: 9000}, true)}),
		"the annotation flipping under a known service changes nothing, so a registrar roll rebuilds no listener")
}
