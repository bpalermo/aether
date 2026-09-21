package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadClustersFromRegistry_ServiceUnderBothProtocols is the proposal 037
// Phase 1 milestone: a service that appears under BOTH protocol listings keeps
// its HTTP cluster and outbound vhost AND gets a TCP floor cluster, instead of
// the TCP pass clobbering the HTTP one.
//
// This is not a hypothetical. The registry key is <ns>/<serviceAccount>, and the
// protocol is a PER-POD annotation, so a ServiceAccount whose pods disagree —
// `endpoint.aether.io/protocol: tcp` on half a Deployment — puts the same service
// in both listings. That is true on etcd (the CNI registers each pod under its
// own protocol key) and, since #878, on the kubernetes backend too.
//
// Before this change both passes wrote c.clusters[serviceName] and the TCP pass
// ran second, so the h2 cluster, the outbound vhost and the GAMMA cap_http vhost
// all vanished and captured requests to the service 503'd with
// no_healthy_upstream — the #430 failure mode, reachable by annotation.
func TestLoadClustersFromRegistry_ServiceUnderBothProtocols(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "mixed-0",
		Namespace:        "aether-test",
		ServiceAccount:   "mixed",
		NetworkNamespace: "/var/run/netns/cni-mixed",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/mixed")

	// The SAME service key under both protocols, with DIFFERENT endpoints —
	// which is what a split-annotation ServiceAccount actually produces.
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"aether-test/mixed": {makeEndpoint("10.0.0.20", "cluster-1", "node-2", 9000)},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/mixed": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "aether-test/mixed", ClusterIP: "10.96.0.60"}})

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	const fqdn = "mixed.aether-test.aether.internal"

	// 1. The HTTP cluster survives the TCP pass. This is the assertion that
	//    fails against the pre-Phase-1 code.
	httpCluster, ok := clusters[fqdn].(*clusterv3.Cluster)
	require.True(t, ok, "the h2 cluster must survive a service that is also listed under TCP")
	assert.Equal(t, "aether-test/mixed", httpCluster.GetEdsClusterConfig().GetServiceName())

	// 2. The TCP floor cluster is there too.
	tcpCluster, ok := clusters["tcp:"+fqdn].(*clusterv3.Cluster)
	require.True(t, ok, "the tcp floor cluster must be built for the TCP listing")
	assert.Equal(t, "aether-test/mixed", tcpCluster.GetEdsClusterConfig().GetServiceName(),
		"both clusters resolve the same bare-name EDS resource")

	// 3. The outbound vhost survives. Losing it is what turned client traffic
	//    into route-table misses (404) rather than retriable 503s.
	rc, ok := snap.GetResources(resourcev3.RouteType)[proxy.OutboundHTTPRouteName].(*routev3.RouteConfiguration)
	require.True(t, ok, "out_http must be published")
	var vhostFound bool
	for _, vh := range rc.GetVirtualHosts() {
		if vh.GetName() == fqdn {
			vhostFound = true
			break
		}
	}
	assert.True(t, vhostFound, "the service's outbound vhost must survive the TCP pass")

	// 4. EXACTLY ONE EDS resource named for the service. Both entries reference
	//    the bare name; if both also PUBLISHED it, go-control-plane's snapshot
	//    consistency check rejects the duplicate (or one silently wins). The
	//    HTTP entry owns it; the TCP entry carries loadAssignment == nil.
	endpointsRes := snap.GetResources(resourcev3.EndpointType)
	cla, ok := endpointsRes["aether-test/mixed"].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok, "the bare-name EDS resource must be published exactly once")
	assert.Equal(t, "aether-test/mixed", cla.GetClusterName())

	// The owner is the HTTP entry, so the published CLA carries the HTTP
	// endpoints. The TCP floor resolves through the same resource — which is the
	// pre-existing shared-EDS behaviour this phase does not change.
	c.clusterMu.RLock()
	tcpEntry, hasTCP := c.clusters[proxy.TCPClusterName("aether-test/mixed", c.meshDomain)]
	httpEntry, hasHTTP := c.clusters["aether-test/mixed"]
	c.clusterMu.RUnlock()
	require.True(t, hasTCP, "the TCP entry is keyed by its Envoy cluster name")
	require.True(t, hasHTTP, "the HTTP entry keeps the bare service key")
	assert.Nil(t, tcpEntry.loadAssignment,
		"the TCP entry must not publish a second CLA under the bare name")
	assert.NotNil(t, httpEntry.loadAssignment,
		"the HTTP entry owns the bare-name CLA")
}

// TestLoadClustersFromRegistry_TCPOnlyServiceOwnsBareCLA is the other half of the
// ownership rule: with no HTTP entry, the TCP floor entry must publish the
// bare-name load assignment itself, or the floor cluster's EDS never resolves and
// tcp_proxy — which has no ODCDS cold path — silently drops the connection.
func TestLoadClustersFromRegistry_TCPOnlyServiceOwnsBareCLA(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "tcponly-0",
		Namespace:        "aether-test",
		ServiceAccount:   "tcponly",
		NetworkNamespace: "/var/run/netns/cni-tcponly",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/tcponly")

	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"aether-test/tcponly": {makeEndpoint("10.0.0.30", "cluster-1", "node-2", 9000)},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "aether-test/tcponly", ClusterIP: "10.96.0.70"}})

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	cla, ok := snap.GetResources(resourcev3.EndpointType)["aether-test/tcponly"].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok, "a TCP-only service must still publish its bare-name EDS resource")
	require.Len(t, cla.GetEndpoints(), 1)

	c.clusterMu.RLock()
	_, bareKeyed := c.clusters["aether-test/tcponly"]
	tcpEntry, hasTCP := c.clusters[proxy.TCPClusterName("aether-test/tcponly", c.meshDomain)]
	c.clusterMu.RUnlock()

	assert.False(t, bareKeyed,
		"a TCP entry is no longer stored under the bare service name (proposal 037 design (a))")
	require.True(t, hasTCP)
	assert.NotNil(t, tcpEntry.loadAssignment, "with no HTTP entry the TCP entry owns the bare-name CLA")
	assert.Equal(t, "aether-test/tcponly", tcpEntry.loadAssignment.GetClusterName(),
		"the CLA is named for the SERVICE, not for the tcp: cluster key")
}
