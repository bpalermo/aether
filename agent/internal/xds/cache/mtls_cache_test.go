package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	filter_state_overridev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_mappers/filter_state_override/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	testPodSpiffeID = "spiffe://aether.internal/ns/aether-test/sa/echo"
	testPodNetns    = "/var/run/netns/cni-a"
)

// echoTestPod returns the local pod used by the mTLS cache tests: it maps
// testPodNetns to testPodSpiffeID and puts "aether-test/echo" in the node
// dependency set.
func echoTestPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: testPodNetns,
	}
}

// echoRegistry returns a mock registry serving "aether-test/echo" with a
// single endpoint in the given namespace.
func echoRegistry(endpointNamespace *string) *mockRegistry {
	return &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			ep := makeEndpoint("10.0.0.9", "cluster-1", "node-2", 18080)
			ep.KubernetesMetadata.Namespace = *endpointNamespace
			return map[string][]*registryv1.ServiceEndpoint{"aether-test/echo": {ep}}, nil
		},
	}
}

// snapshotEchoCluster fetches the emitted echo service cluster from the
// current snapshot.
func snapshotEchoCluster(t *testing.T, c *SnapshotCache, nodeName string) *clusterv3.Cluster {
	t.Helper()
	snap, err := c.GetSnapshot(nodeName)
	require.NoError(t, err)
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo.aether-test.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present in the snapshot")
	return echo
}

// pinnedSANs extracts the exact-match SAN URIs pinned on the cluster's upstream
// validation context. Since #842 a mesh cluster carries ONE transport socket
// (the waypoint variant carries two, which differ only in SNI), so there is one
// validation context to read.
func pinnedSANs(t *testing.T, cl *clusterv3.Cluster) []string {
	t.Helper()
	require.NotNil(t, cl.GetTransportSocket())
	utc := &tlsv3.UpstreamTlsContext{}
	require.NoError(t, cl.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))
	var got []string
	for _, sm := range utc.GetCommonTlsContext().GetCombinedValidationContext().GetDefaultValidationContext().GetMatchTypedSubjectAltNames() {
		got = append(got, sm.GetMatcher().GetExact())
	}
	return got
}

// certMapperDefault returns the secret name a connection with no source
// identity presents: the on-demand selector's mapper default_value.
func certMapperDefault(t *testing.T, cl *clusterv3.Cluster) string {
	t.Helper()
	require.NotNil(t, cl.GetTransportSocket())
	utc := &tlsv3.UpstreamTlsContext{}
	require.NoError(t, cl.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))
	sel := utc.GetCommonTlsContext().GetCustomTlsCertificateSelector()
	require.NotNil(t, sel, "mesh cluster must carry the per-connection certificate selector")

	onDemand := &on_demand_secretv3.Config{}
	require.NoError(t, sel.GetTypedConfig().UnmarshalTo(onDemand))
	mapper := &filter_state_overridev3.Config{}
	require.NoError(t, onDemand.GetCertificateMapper().GetTypedConfig().UnmarshalTo(mapper))
	return mapper.GetDefaultValue()
}

// TestCachedMTLSClusterMatchesInlineInjection is the byte-identity proof for
// issue #537: the cluster emitted from the precomputed entry cache must be
// proto-equal to what the previous snapshot path produced by cloning the base
// cluster and injecting the transport socket inline, for every composition
// mode (node, node+waypoint, edge).
func TestCachedMTLSClusterMatchesInlineInjection(t *testing.T) {
	ns := "default"
	sanURIs := []string{"spiffe://aether.internal/ns/default/sa/echo"}
	validationContextName := "spiffe://aether.internal"
	fqdn := "echo.aether-test.aether.internal"

	tests := []struct {
		name     string
		setup    func(t *testing.T, c *SnapshotCache)
		expected func() *clusterv3.Cluster
	}{
		{
			name: "node mode: per-source matcher",
			setup: func(t *testing.T, c *SnapshotCache) {
				require.NoError(t, c.AddPod(context.Background(), echoTestPod(), "aether.internal"))
				require.NoError(t, c.SetNodeIdentity(context.Background(), nodeIdentity))
			},
			expected: func() *clusterv3.Cluster {
				cl := proxy.NewServiceCluster(fqdn, "aether-test/echo", "aether-test/echo", proxy.SortSubsetKeys(nil))
				proxy.InjectUpstreamMTLS(cl, nodeIdentity, validationContextName, sanURIs, "18080", "")
				return cl
			},
		},
		{
			name: "node mode with waypoint: structured-SNI socket variants",
			setup: func(t *testing.T, c *SnapshotCache) {
				c.SetWaypointConfig(true, 18009)
				require.NoError(t, c.AddPod(context.Background(), echoTestPod(), "aether.internal"))
				require.NoError(t, c.SetNodeIdentity(context.Background(), nodeIdentity))
			},
			expected: func() *clusterv3.Cluster {
				cl := proxy.NewServiceCluster(fqdn, "aether-test/echo", "aether-test/echo", proxy.SortSubsetKeys(nil))
				proxy.InjectUpstreamMTLS(cl, nodeIdentity, validationContextName, sanURIs, "18080", "18080."+fqdn)
				return cl
			},
		},
		{
			name: "edge mode: single-identity transport socket",
			setup: func(t *testing.T, c *SnapshotCache) {
				c.SetEdgeMode(8080)
				c.SetEdgeIdentity(nodeIdentity, "aether.internal")
				c.SetStaticDependencies([]string{"aether-test/echo"})
			},
			expected: func() *clusterv3.Cluster {
				cl := proxy.NewServiceCluster(fqdn, "aether-test/echo", "aether-test/echo", proxy.SortSubsetKeys(nil))
				cl.TransportSocket = proxy.EdgeUpstreamTransportSocket(nodeIdentity, validationContextName, sanURIs, "18080")
				return cl
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setup(t, c)
			require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", echoRegistry(&ns)))

			got := snapshotEchoCluster(t, c, "node-1")
			want := tt.expected()
			assert.True(t, proto.Equal(want, got),
				"cached mTLS cluster must be proto-identical to the inline-injected cluster\nwant: %v\ngot:  %v", want, got)
		})
	}
}

// TestCachedMTLSClusterInvalidatedOnWorkloadChange used to verify the reverse
// of what it now asserts, and the inversion IS issue #842.
//
// It checked that adding or removing a local pod after the registry load
// rebuilt the cached cluster, because the per-source transport_socket_matcher
// had to track the local workload set — a stale cache would have silently
// downgraded the new pod's outbound mTLS to the node certificate. #815 made
// that set-valued (one entry per ServiceAccount) so pod churn WITHIN a
// ServiceAccount stopped mattering, but the first pod of a NEW ServiceAccount
// still rewrote every mesh cluster on the node, and every rewritten EDS cluster
// then re-warmed for the full 15 s EDS initial_fetch_timeout before its
// warming→active swap drained every upstream pool on that node.
//
// The certificate is chosen per connection now, so the cluster has no workload
// input at all: a pod arriving or leaving — of a new ServiceAccount or an
// existing one — must leave the emitted cluster BYTE-IDENTICAL, which is what
// lets Envoy's cluster hash gate drop the update entirely.
func TestCachedMTLSClusterInvalidatedOnWorkloadChange(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"

	require.NoError(t, c.AddPod(ctx, echoTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", echoRegistry(&ns)))

	bytesOf := func() []byte {
		out, err := proto.MarshalOptions{Deterministic: true}.Marshal(snapshotEchoCluster(t, c, "node-1"))
		require.NoError(t, err)
		return out
	}

	initial := bytesOf()
	// The one identity a mesh cluster still names is the NODE's — the mapper's
	// default_value, presented by connections that carry no source identity.
	assert.Equal(t, nodeIdentity, certMapperDefault(t, snapshotEchoCluster(t, c, "node-1")))
	assert.NotContains(t, string(initial), testPodSpiffeID,
		"a mesh cluster must not name any workload identity")

	// A pod of a DIFFERENT ServiceAccount lands after the load. Before #842
	// this was the expensive case: a new identity in the matcher's
	// exact_match_map, so every mesh cluster on the node re-warmed.
	other := &cniv1.CNIPod{
		Name:             "other-1",
		Namespace:        "aether-test",
		ServiceAccount:   "other",
		NetworkNamespace: "/var/run/netns/cni-b",
	}
	require.NoError(t, c.AddPod(ctx, other, "aether.internal"))
	assert.Equal(t, initial, bytesOf(),
		"a NEW ServiceAccount arriving must not change a mesh cluster's bytes (#842)")

	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-b"))
	assert.Equal(t, initial, bytesOf(),
		"the LAST pod of a ServiceAccount leaving must not change a mesh cluster's bytes (#842)")
}

// TestCachedMTLSClusterInvalidatedOnNodeIdentity verifies the node-SVID
// invalidation path: a cluster loaded BEFORE the node SVID is served is
// emitted bare (cleartext), and SetNodeIdentity afterwards must rebuild the
// cache so the next snapshot carries the injected transport socket.
func TestCachedMTLSClusterInvalidatedOnNodeIdentity(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"

	// As in production: the agent seeds the cache's trust domain at wiring time
	// (identity.NewTrustDomain(meshDomain), #740) — long before any cluster is
	// loaded. Upstream mTLS cannot be injected without one, because the
	// validation context is named "spiffe://<trust-domain>" (#815).
	c.setTrustDomain("aether.internal")

	declareDeps(c, "aether-test/echo")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", echoRegistry(&ns)))

	echo := snapshotEchoCluster(t, c, "node-1")
	assert.Nil(t, echo.GetTransportSocketMatcher(), "no matcher before the node SVID is served")
	assert.Nil(t, echo.GetTransportSocket(), "no transport socket before the node SVID is served")

	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	// No local workloads: the injection falls back to a single plain transport
	// socket presenting the node identity.
	echo = snapshotEchoCluster(t, c, "node-1")
	assert.NotNil(t, echo.GetTransportSocket(), "SetNodeIdentity after the load must rebuild the cached cluster")
}

// TestUpdateEdgeIdentityAfterStart is the edge's counterpart, and the cache half
// of issue #740: the edge no longer blocks startup on SPIRE, so its clusters are
// loaded BEFORE it has an identity and are emitted bare. UpdateEdgeIdentity, called
// when the SVID finally lands, must rebuild them and push a snapshot, so the edge
// Envoy picks the identity up on the next CDS update instead of on a restart.
func TestUpdateEdgeIdentityAfterStart(t *testing.T) {
	c := newTestCache("edge-1")
	ctx := context.Background()
	ns := "default"

	c.SetEdgeMode(8080)
	// The seeds resolveEdgeIdentity hands out while SPIRE is still coming up.
	c.SetEdgeIdentity("", "aether.internal")
	c.SetStaticDependencies([]string{"aether-test/echo"})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "edge-1", echoRegistry(&ns)))

	echo := snapshotEchoCluster(t, c, "edge-1")
	assert.Nil(t, echo.GetTransportSocket(), "no upstream mTLS before the edge SVID exists")

	require.NoError(t, c.UpdateEdgeIdentity(ctx, nodeIdentity, "aether.internal"))

	echo = snapshotEchoCluster(t, c, "edge-1")
	require.NotNil(t, echo.GetTransportSocket(), "a late edge identity must rebuild the cached cluster")

	// Idempotent: an unchanged identity (every SVID rotation that keeps the same
	// SPIFFE ID) is a no-op rather than another snapshot push.
	before := c.version.Load()
	require.NoError(t, c.UpdateEdgeIdentity(ctx, nodeIdentity, "aether.internal"))
	assert.Equal(t, before, c.version.Load(), "an unchanged identity must not bump the snapshot version")
}

// TestCachedMTLSClusterInvalidatedOnSANNamespaceChange verifies the
// sanNamespaces invalidation path: a registry reload whose endpoints moved to
// a different namespace must re-render the pinned server identities on the
// cached cluster (and never leave the stale SAN set in place).
func TestCachedMTLSClusterInvalidatedOnSANNamespaceChange(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"
	reg := echoRegistry(&ns)

	require.NoError(t, c.AddPod(ctx, echoTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	assert.Equal(t, []string{"spiffe://aether.internal/ns/default/sa/echo"},
		pinnedSANs(t, snapshotEchoCluster(t, c, "node-1")))

	// The service's endpoints move namespaces; the reload must re-render SANs.
	ns = "prod"
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	assert.Equal(t, []string{"spiffe://aether.internal/ns/prod/sa/echo"},
		pinnedSANs(t, snapshotEchoCluster(t, c, "node-1")))
}

// TestNodeProxyPerSourceClustersPoolPerDownstreamConnection is the regression
// guard that used to demand connection_pool_per_downstream_connection on every
// per-source cluster (issue #831) and now demands the arrangement that replaced
// it (issue #842). The two are mutually exclusive, and only one of them is a
// design.
//
// #831's mechanism was real: set_filter_state's default "envoy.string" factory
// builds a Router::StringAccessorImpl, which is NOT Envoy::Hashable, and
// CommonUpstreamTransportSocketFactory::hashKey folds a downstream shared
// filter-state object into the upstream pool hash ONLY for Hashable objects, so
// the identity that selects the client certificate contributed ZERO BYTES to
// the pool key. connection_pool_per_downstream_connection covered that by
// mixing the downstream connection id in instead — correct, but at the cost of
// one upstream connection and one full mTLS handshake per downstream
// connection, forever.
//
// #842 put the identity in the key where it belongs (envoy.hashable_string) and
// took the flag off. So the invariant is inverted, and its two halves must be
// checked TOGETHER — the flag's absence is safe only because the identity is
// hashable, and //test/mtlspool proves the pair end to end against a real
// Envoy. A cluster that had neither would be the #831 leak.
func TestNodeProxyPerSourceClustersPoolPerDownstreamConnection(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"

	require.NoError(t, c.AddPod(ctx, echoTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", echoRegistry(&ns)))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	checked := 0
	for name, res := range snap.GetResources(resourcev3.ClusterType) {
		cl, ok := res.(*clusterv3.Cluster)
		if !ok || cl.GetTransportSocket() == nil {
			continue
		}
		utc := &tlsv3.UpstreamTlsContext{}
		if cl.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc) != nil {
			continue
		}
		if utc.GetCommonTlsContext().GetCustomTlsCertificateSelector() == nil {
			continue // not a per-source mesh cluster (edge, probe, app)
		}
		checked++
		assert.Falsef(t, cl.GetConnectionPoolPerDownstreamConnection(),
			"cluster %q still keys pools by the downstream connection: since #842 the "+
				"source identity is in the pool key, and keeping the flag costs all "+
				"upstream h2 multiplexing across sources for nothing", name)
		assert.Zerof(t, utc.GetMaxSessionKeys().GetValue(),
			"cluster %q must keep max_session_keys 0: a client context supports a "+
				"custom certificate selector only with session resumption off, and a "+
				"resumed session would carry a certificate the selector did not choose", name)
	}
	require.NotZero(t, checked, "no per-source cluster in the snapshot: the guard checked nothing")
}

// TestMeshClusterNamesOnlyTheNodeIdentity: the ONLY SPIFFE ID a mesh cluster
// may contain is the mapper's default_value (the node SVID). Any workload
// identity in a cluster's bytes is per-node state, and per-node state in a
// cluster is what made every pod event re-hash every cluster (#815).
func TestMeshClusterNamesOnlyTheNodeIdentity(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	ns := "default"

	require.NoError(t, c.AddPod(ctx, echoTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", echoRegistry(&ns)))

	cl := snapshotEchoCluster(t, c, "node-1")
	b, err := proto.Marshal(cl)
	require.NoError(t, err)

	assert.Contains(t, string(b), nodeIdentity, "the node SVID is the mapper's default_value")
	assert.NotContains(t, string(b), testPodSpiffeID, "no local workload identity may appear")
	assert.NotContains(t, string(b), testPodNetns, "and certainly no netns path")
}
