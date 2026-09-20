package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	// Registry/dependency/capture keys are namespace-qualified "<ns>/<svc>"
	// (020 Part 1): aether-test/echo-tcp.
	declareDeps(c, "aether-test/echo-tcp")

	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"aether-test/echo-tcp": {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 9000)},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}

	// The capture reconciler classified echo-tcp as a TCP service (annotation
	// "tcp" on its mesh Service) and projected its ClusterIP.
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "aether-test/echo-tcp", ClusterIP: "10.96.0.50"}})

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	clusters := snap.GetResources(resourcev3.ClusterType)
	// The TCP floor cluster is present, named tcp:<svc>.<ns>.<domain>.
	tcpCluster, ok := clusters["tcp:echo-tcp.aether-test.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "tcp floor cluster must be present")
	assert.Equal(t, "aether-test/echo-tcp", tcpCluster.GetEdsClusterConfig().GetServiceName(),
		"tcp cluster EDS resource is the namespace-qualified service key")
	// Per-source mTLS is one socket with a per-connection certificate selector
	// (#842); the per-identity matcher and socket list are gone. The TCP floor
	// follows the HTTP path exactly — it differs only in ALPN and SNI.
	assert.Nil(t, tcpCluster.GetTransportSocketMatcher(), "the per-identity matcher is gone (#842)")
	assert.Empty(t, tcpCluster.GetTransportSocketMatches())
	require.NotNil(t, tcpCluster.GetTransportSocket(), "tcp floor cluster must carry the mesh upstream socket")

	utc := &tlsv3.UpstreamTlsContext{}
	require.NoError(t, tcpCluster.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))
	require.NotNil(t, utc.GetCommonTlsContext().GetCustomTlsCertificateSelector(),
		"tcp floor cluster must carry the per-connection certificate selector")
	assert.Empty(t, utc.GetCommonTlsContext().GetAlpnProtocols(),
		"the TCP floor sends NO ALPN so the destination inbound demuxes to its default chain")

	// SAN pinning regression (020 Part 1): the transport socket must pin the
	// peer identity with the BARE ServiceAccount name in the sa/ segment —
	// "spiffe://<td>/ns/<endpoint-ns>/sa/echo-tcp" — NOT the namespace-qualified
	// key ("sa/aether-test/echo-tcp"), which never matches an SVID and fails every
	// TCP-floor handshake with fail_verify_san (observed on talos, 2026-07-02).
	combined := utc.GetCommonTlsContext().GetCombinedValidationContext()
	require.NotNil(t, combined, "the tcp floor socket pins the server identity")
	var got []string
	for _, sm := range combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames() {
		got = append(got, sm.GetMatcher().GetExact())
	}
	assert.Equal(t, []string{"spiffe://aether.internal/ns/default/sa/echo-tcp"}, got,
		"SAN pin uses the bare SA name")

	// No HTTP cluster/vhost for a TCP-only service.
	_, hasHTTP := clusters["echo-tcp.aether-test.aether.internal"]
	assert.False(t, hasHTTP, "a TCP service must not also emit an HTTP cluster")

	// EDS for the service key carries the endpoint (the tcp: cluster references it
	// by ServiceName).
	endpoints := snap.GetResources(resourcev3.EndpointType)
	cla, ok := endpoints["aether-test/echo-tcp"].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok, "service-key EDS load assignment must be present for the tcp cluster")
	require.NotEmpty(t, cla.GetEndpoints(), "tcp cluster EDS must carry the registry endpoint")
	require.NotEmpty(t, cla.GetEndpoints()[0].GetLbEndpoints())
	addr := cla.GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "10.0.0.9", addr.GetAddress())
}

// TestCaptureTCPServices_JoinDependencySet verifies every TCP mesh service joins the
// node dependency set on its own — WITHOUT any local pod declaring it as an upstream.
// The capture listener emits a per-VIP floor chain for each TCP service
// unconditionally, and tcp_proxy has no ODCDS cold path: if the tcp: cluster were
// demand-gated out, the chain would match and every connection would die silently
// (the #460 e2e trap). Chain and cluster must always ship together.
func TestCaptureTCPServices_JoinDependencySet(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)

	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: "aether-test/tcp-echo", ClusterIP: "10.96.0.50"},
		{ServiceName: "team-b/redis", ClusterIP: "10.96.0.51"},
	})

	deps := c.DependencySet()
	assert.Contains(t, deps, "aether-test/tcp-echo", "TCP mesh service must be in scope without a declaring pod")
	assert.Contains(t, deps, "team-b/redis")

	// Removal drops them again (level-based replace).
	c.SetCaptureTCPServices(nil)
	deps = c.DependencySet()
	assert.NotContains(t, deps, "aether-test/tcp-echo")
	assert.NotContains(t, deps, "team-b/redis")
}
