package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCaptureChainsResolveAgainstCDS is proposal 037 Risk 1.
//
// A capture filter chain that names a cluster absent from the snapshot does not
// fail loudly. tcp_proxy has no ODCDS cold path, so Envoy simply kills the
// connection — no NACK, no stat, nothing that distinguishes it from a backend
// that is down. The per-port chains added for 037 name tcp:<fqdn>:<port>, so
// the cluster has to be produced from the same derived facts, in the same
// snapshot generation, as the chain that names it.
func TestCaptureChainsResolveAgainstCDS(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "client-0",
		Namespace:        "aether-test",
		ServiceAccount:   "client",
		NetworkNamespace: "/var/run/netns/cni-client",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/multi")

	// A service with a TCP primary AND a second raw-TCP port: the shape that
	// produces a per-port chain, and therefore a per-port cluster.
	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.0.40",
		ClusterName: "cluster-1",
		Port:        9000,
		Ports:       []uint32{9000, 5432},
		PortProtocols: map[uint32]registryv1.PortProtocol{
			9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
			5432: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "aether-test", PodName: "multi-0", NodeName: "node-2"},
	}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{"aether-test/multi": {ep}}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "aether-test/multi", ClusterIP: "10.96.0.80", PrimaryIsTCP: true}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	// The CDS set actually published.
	cds := map[string]struct{}{}
	for name := range snap.GetResources(resourcev3.ClusterType) {
		cds[name] = struct{}{}
	}
	require.NotEmpty(t, cds)

	// Every cluster any tcp_proxy names, across every listener.
	named := map[string]string{} // cluster -> chain that named it
	for _, res := range snap.GetResources(resourcev3.ListenerType) {
		l, ok := res.(*listenerv3.Listener)
		if !ok {
			continue
		}
		chains := append([]*listenerv3.FilterChain{}, l.GetFilterChains()...)
		if d := l.GetDefaultFilterChain(); d != nil {
			chains = append(chains, d)
		}
		for _, fc := range chains {
			for _, f := range fc.GetFilters() {
				tc := &tcp_proxyv3.TcpProxy{}
				if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(tc) != nil {
					continue
				}
				if single := tc.GetCluster(); single != "" {
					named[single] = fc.GetName()
				}
				for _, w := range tc.GetWeightedClusters().GetClusters() {
					named[w.GetName()] = fc.GetName()
				}
			}
		}
	}

	require.Contains(t, named, "tcp:multi.aether-test.aether.internal:5432",
		"the per-port chain must exist, or this test proves nothing")

	for cluster, chain := range named {
		// The ORIGINAL_DST passthrough cluster is static, not from CDS.
		if cluster == "passthrough_original_dst" {
			continue
		}
		assert.Contains(t, cds, cluster,
			"filter chain %q names cluster %q, which is not in the snapshot — tcp_proxy has no ODCDS cold path, so those connections die silently",
			chain, cluster)
	}

	// And the per-port cluster carries the port as SNI, which is how the
	// destination demuxes to the right loopback port. The floor must NOT (#306).
	pc, ok := snap.GetResources(resourcev3.ClusterType)["tcp:multi.aether-test.aether.internal:5432"].(*clusterv3.Cluster)
	require.True(t, ok)
	assert.Equal(t, "multi.aether-test.aether.internal:5432", pc.GetEdsClusterConfig().GetServiceName(),
		"the per-port cluster resolves its OWN load assignment, filtered to pods advertising that port as TCP")
}
