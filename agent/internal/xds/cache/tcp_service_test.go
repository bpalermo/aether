package cache

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/capture"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tcpTransportSocketFilterStateExtName is the correct Envoy extension name for
// the transport-socket-specific FilterStateInput, exported here for assertions.
const tcpTransportSocketFilterStateExtName = "envoy.matching.inputs.transport_socket_filter_state"

// TestLoadClustersFromRegistry_TCPCluster verifies that a PROTOCOL_TCP service
// produces a "tcp:<svc>.<domain>" floor cluster whose EDS resolves to the
// service's registry endpoints — the data path the transparent-capture TCP floor
// routes a raw mTLS passthrough onto. The HTTP "<svc>.<domain>" cluster is NOT
// emitted for a TCP service (its data path is the floor cluster).
func TestLoadClustersFromRegistry_TCPCluster(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	// A local pod establishes the netns -> SPIFFE ID mapping; SetNodeIdentity
	// gates the upstream mTLS injection captureTCPClusters depends on.
	pod := &cniv1.CNIPod{
		Name:             "echo-tcp-0",
		Namespace:        "aether-test",
		ServiceAccount:   "echo-tcp",
		NetworkNamespace: "/var/run/netns/cni-tcp",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "echo-tcp")

	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"echo-tcp": {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 9000)},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}

	// The capture reconciler classified echo-tcp as a TCP service (annotation
	// "tcp" on its mesh Service) and projected its ClusterIP.
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "echo-tcp", ClusterIP: "10.96.0.50"}})

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	clusters := snap.GetResources(resourcev3.ClusterType)
	// The TCP floor cluster is present, named tcp:<svc>.<domain>.
	tcpCluster, ok := clusters["tcp:echo-tcp.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "tcp floor cluster must be present")
	assert.Equal(t, "echo-tcp", tcpCluster.GetEdsClusterConfig().GetServiceName(),
		"tcp cluster EDS resource is the bare service name")
	// With a local pod present, the per-source mTLS transport socket is injected
	// via the matcher (not a single TransportSocket).
	require.NotNil(t, tcpCluster.GetTransportSocketMatcher(), "tcp floor cluster must carry the per-source mTLS matcher")

	// The transport socket matcher must use the transport-socket-specific
	// FilterStateInput extension, NOT the generic network filter_state input.
	// Using "envoy.matching.inputs.filter_state" in a cluster transport_socket_matcher
	// causes Envoy to silently return nullopt (wrong MatchingData type), so the
	// exact-match never fires and every upstream connection falls to OnNoMatch,
	// presenting the node identity instead of the per-source pod cert. For tcp_proxy
	// upstreams with SAN pinning this causes TLS handshake failures (client cert SAN
	// mismatch against the service's expected namespace), manifesting as upstream_cx_total=0.
	inputName := tcpCluster.GetTransportSocketMatcher().GetMatcherTree().GetInput().GetName()
	assert.Equal(t, tcpTransportSocketFilterStateExtName, inputName,
		"TCP floor cluster transport_socket_matcher must use envoy.matching.inputs.transport_socket_filter_state")

	// No HTTP cluster/vhost for a TCP-only service.
	_, hasHTTP := clusters["echo-tcp.aether.internal"]
	assert.False(t, hasHTTP, "a TCP service must not also emit an HTTP cluster")

	// EDS for the bare service name carries the endpoint (the tcp: cluster
	// references it by ServiceName).
	endpoints := snap.GetResources(resourcev3.EndpointType)
	cla, ok := endpoints["echo-tcp"].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok, "bare-name EDS load assignment must be present for the tcp cluster")
	require.NotEmpty(t, cla.GetEndpoints(), "tcp cluster EDS must carry the registry endpoint")
	require.NotEmpty(t, cla.GetEndpoints()[0].GetLbEndpoints())
	addr := cla.GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "10.0.0.9", addr.GetAddress())
}
