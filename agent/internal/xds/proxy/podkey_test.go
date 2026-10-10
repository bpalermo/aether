package proxy

import (
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1584: every per-pod xDS name tells two pods apart when the pods have
// the same NAME in different namespaces (two StatefulSets called "web", each
// with a "web-0", scheduled on one node).
//
// These tests are the inventory of what is derived from a pod. Before the fix
// every name below was the pod name alone and the two pods shared it.

const (
	sameNameTrustDomain = "aether.internal"
	sameNameMeshDomain  = "mesh.internal"
)

func sameNamePodA() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "web-0",
		Namespace:        "ns-a",
		ServiceAccount:   "web",
		NetworkNamespace: "/var/run/netns/cni-aaaa",
		Uid:              "11111111-1111-1111-1111-111111111111",
	}
}

func sameNamePodB() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "web-0",
		Namespace:        "ns-b",
		ServiceAccount:   "web",
		NetworkNamespace: "/var/run/netns/cni-bbbb",
		Uid:              "22222222-2222-2222-2222-222222222222",
	}
}

// TestPodResourceKey pins the spelling and that it cannot be ambiguous: the
// separator is legal in neither a namespace nor a pod name, so pods that
// differ in either never produce one key, including the pairs a '-' separator
// would confuse.
func TestPodResourceKey(t *testing.T) {
	assert.Equal(t, "ns-a_web-0", PodResourceKey(sameNamePodA()))
	assert.NotEqual(t, PodResourceKey(sameNamePodA()), PodResourceKey(sameNamePodB()))
	assert.NotEqual(t,
		PodResourceKey(&cniv1.CNIPod{Namespace: "a-b", Name: "c"}),
		PodResourceKey(&cniv1.CNIPod{Namespace: "a", Name: "b-c"}))

	key := PodResourceKey(&cniv1.CNIPod{Namespace: "team-a", Name: "web-0.shard"})
	ns, name, ok := strings.Cut(key, "_")
	require.True(t, ok)
	assert.Equal(t, "team-a", ns, "the first underscore ends the namespace")
	assert.Equal(t, "web-0.shard", name)
}

// TestPerPodResourceNamesCarryTheNamespace is the name inventory: every xDS
// RESOURCE name (the key go-control-plane indexes a snapshot by, and the key
// Envoy holds a listener or cluster under) that is derived from a pod, and the
// health gateway paths the agent's liveness loop reads.
func TestPerPodResourceNamesCarryTheNamespace(t *testing.T) {
	a, b := sameNamePodA(), sameNamePodB()

	names := []struct {
		kind string
		a, b string
		want string
	}{
		{"LDS outbound HTTP listener", OutboundListenerName(a), OutboundListenerName(b), "outbound_http_ns-a_web-0"},
		{"LDS inbound mTLS listener", InboundListenerName(a), InboundListenerName(b), "inbound_ns-a_web-0"},
		{"LDS inbound QUIC listener", InboundQUICListenerName(a), InboundQUICListenerName(b), "inbound_ns-a_web-0_h3"},
		{"LDS TCP capture listener", CaptureListenerName(a), CaptureListenerName(b), "capture_ns-a_web-0"},
		{"LDS UDP capture listener", CaptureUDPListenerName(a), CaptureUDPListenerName(b), "capture_udp_ns-a_web-0"},
		{"CDS app delivery cluster", AppClusterName(a, 8080), AppClusterName(b, 8080), "app_ns-a_web-0_8080"},
		{"CDS app health-probe cluster", HealthProbeClusterName(a), HealthProbeClusterName(b), "health_ns-a_web-0"},
		{"CDS inbound-readiness cluster", InboundReadyClusterName(a), InboundReadyClusterName(b), "inboundready_ns-a_web-0"},
		{
			"health gateway path (app probe)",
			HealthGatewayPath(HealthProbeClusterName(a)), HealthGatewayPath(HealthProbeClusterName(b)),
			"/healthz/health_ns-a_web-0",
		},
		{
			"health gateway path (inbound readiness)",
			HealthGatewayPath(InboundReadyClusterName(a)), HealthGatewayPath(InboundReadyClusterName(b)),
			"/healthz/inboundready_ns-a_web-0",
		},
	}
	for _, tc := range names {
		assert.Equalf(t, tc.want, tc.a, "%s (ns-a/web-0)", tc.kind)
		assert.NotEqualf(t, tc.a, tc.b, "%s: ns-a/web-0 and ns-b/web-0 must not share a name", tc.kind)
		assert.Truef(t, strings.Contains(tc.b, "ns-b_web-0"), "%s (ns-b/web-0) = %q", tc.kind, tc.b)
	}

	// The per-pod cluster names are still recognised as per-pod: the on-demand
	// CDS observer and the pin report skip them by prefix.
	for _, name := range []string{AppClusterName(a, 8080), HealthProbeClusterName(a), InboundReadyClusterName(a)} {
		assert.Truef(t, IsPerPodClusterName(name), "%s", name)
	}
}

// TestSameNamedPodsShareNoGeneratedName builds both pods' resources with the
// real generators: no listener name, cluster name or stat prefix of one pod is
// one of the other's, each resource is bound into its own pod's network
// namespace and presents its own pod's certificate, and each inbound listener
// routes to its OWN pod's app cluster.
func TestSameNamedPodsShareNoGeneratedName(t *testing.T) {
	a, b := sameNamePodA(), sameNamePodB()

	inA, outA, appA, healthA, err := GenerateListenersFromRegistryPod(a, sameNameTrustDomain, sameNameMeshDomain, false, false, nil, nil, "")
	require.NoError(t, err)
	inB, outB, appB, healthB, err := GenerateListenersFromRegistryPod(b, sameNameTrustDomain, sameNameMeshDomain, false, false, nil, nil, "")
	require.NoError(t, err)

	for _, pair := range [][2]*listenerv3.Listener{{inA, inB}, {outA, outB}} {
		la, lb := pair[0], pair[1]
		assert.NotEqual(t, la.GetName(), lb.GetName())
		assert.Contains(t, la.GetStatPrefix(), "ns-a_web-0", "the per-pod stat prefix carries the namespace")
		assert.Contains(t, lb.GetStatPrefix(), "ns-b_web-0")
		assert.Equal(t, a.GetNetworkNamespace(), la.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath())
		assert.Equal(t, b.GetNetworkNamespace(), lb.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath())
	}
	assert.Equal(t, "inbound_ns-a_web-0", inA.GetStatPrefix())
	assert.Equal(t, "out_http_ns-a_web-0", outA.GetStatPrefix())

	// The server certificate each inbound listener presents is its own pod's.
	assert.Equal(t, "spiffe://aether.internal/ns/ns-a/sa/web", inboundServerCert(t, inA))
	assert.Equal(t, "spiffe://aether.internal/ns/ns-b/sa/web", inboundServerCert(t, inB))

	// The per-pod clusters: their own names, their own netns.
	require.Len(t, appA, 1)
	require.Len(t, appB, 1)
	for _, pair := range [][2]*clusterv3.Cluster{{appA[0], appB[0]}, {healthA, healthB}} {
		ca, cb := pair[0], pair[1]
		assert.NotEqual(t, ca.GetName(), cb.GetName())
		assert.Equal(t, a.GetNetworkNamespace(), ca.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath())
		assert.Equal(t, b.GetNetworkNamespace(), cb.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath())
	}

	// And an inbound listener reaches its application BY NAME: pod A's routes
	// name pod A's delivery cluster and nothing of pod B's.
	assert.Contains(t, inA.String(), appA[0].GetName())
	assert.NotContains(t, inA.String(), appB[0].GetName())
	assert.Contains(t, inB.String(), appB[0].GetName())
	assert.NotContains(t, inB.String(), appA[0].GetName())
	assert.NotContains(t, inA.String(), healthB.GetName(), "nor pod B's health-probe cluster on its readiness path")
}

// TestSameNamedPodsOtherListenersShareNoName covers the listeners
// GenerateListenersFromRegistryPod does not build: QUIC inbound, TCP capture
// and UDP capture.
func TestSameNamedPodsOtherListenersShareNoName(t *testing.T) {
	a, b := sameNamePodA(), sameNamePodB()

	routes := map[string][]L4Backend{"ns-a/dns": {{Service: "ns-a/dns", Cluster: "udp:dns.ns-a.mesh.internal", Weight: 1}}}
	vips := map[string]string{"ns-a/dns": "10.96.0.53"}
	udpA, err := GenerateUDPCaptureListener(a, 18082, routes, vips)
	require.NoError(t, err)
	udpB, err := GenerateUDPCaptureListener(b, 18082, routes, vips)
	require.NoError(t, err)
	require.NotNil(t, udpA)
	require.NotNil(t, udpB)
	assert.Equal(t, "capture_udp_ns-a_web-0", udpA.GetName())
	assert.Equal(t, "capture_udp_ns-a_web-0", udpA.GetStatPrefix())
	assert.Equal(t, "capture_udp_ns-b_web-0", udpB.GetName())
	assert.Contains(t, udpA.String(), `stat_prefix:"capture_udp_ns-a_web-0"`, "the udp_proxy stat prefix too")
	assert.Equal(t, a.GetNetworkNamespace(), udpA.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath())
	assert.Equal(t, b.GetNetworkNamespace(), udpB.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath())

	capA, err := GenerateCaptureListener(a, SpiffeIDFromPod(a, sameNameTrustDomain), 15001, sameNameTrustDomain, false, nil, false, nil)
	require.NoError(t, err)
	capB, err := GenerateCaptureListener(b, SpiffeIDFromPod(b, sameNameTrustDomain), 15001, sameNameTrustDomain, false, nil, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "capture_ns-a_web-0", capA.GetName())
	assert.Equal(t, "capture_ns-a_web-0", capA.GetStatPrefix())
	assert.Equal(t, "capture_ns-b_web-0", capB.GetName())
	assert.Equal(t, "capture_ns-b_web-0", capB.GetStatPrefix())
}

// inboundServerCert is the SDS name of the certificate the listener's first
// TLS-terminating chain presents.
func inboundServerCert(t *testing.T, l *listenerv3.Listener) string {
	t.Helper()
	for _, fc := range l.GetFilterChains() {
		if fc.GetTransportSocket() == nil {
			continue
		}
		ctx := &tlsv3.DownstreamTlsContext{}
		require.NoError(t, fc.GetTransportSocket().GetTypedConfig().UnmarshalTo(ctx))
		certs := ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()
		require.NotEmpty(t, certs)
		return certs[0].GetName()
	}
	t.Fatalf("listener %s has no TLS chain", l.GetName())
	return ""
}
