package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// clusterResourceDigests fingerprints each emitted cluster by the bytes
// go-control-plane hashes for delta-xDS (MarshalResource → HashResource). Two
// snapshots agreeing here are a no-op push; disagreeing means Envoy is told the
// cluster changed and rebuilds it for real.
func clusterResourceDigests(t *testing.T, c *SnapshotCache, node string) map[string]string {
	t.Helper()
	snap, err := c.GetSnapshot(node)
	require.NoError(t, err)

	out := make(map[string]string)
	for name, r := range snap.GetResources(resourcev3.ClusterType) {
		b, err := cachev3.MarshalResource(r)
		require.NoError(t, err)
		sum := sha256.Sum256(b)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out
}

// TestSnapshotRegenerationIsANoOpPush pins the delta-xDS no-op invariant:
// regenerating the snapshot with no input change must produce BYTE-IDENTICAL
// resources, so the push costs Envoy nothing.
//
// This is the steady-state sibling of TestSnapshotDeterministic_ShuffledInputOrder
// (which pins order-independence across independent builds). The agent pushes a
// full snapshot per event — one per CNI ADD/DEL, per SVID arrival, per registry
// reload — and a soak measures ~9 of them per node per minute during a
// scale-up. Every one of those is harmless only for as long as unchanged config
// hashes unchanged: a resource that re-hashes on a no-op regeneration turns each
// push into a real rebuild on the data plane (incident #135 — see ordering.go).
func TestSnapshotRegenerationIsANoOpPush(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/echo", "aether-test/svc-1")

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/echo":  {makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080)},
				"aether-test/svc-1": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	before := clusterResourceDigests(t, c, "node-1")
	require.NotEmpty(t, before, "the fixture must emit clusters or this test proves nothing")

	require.NoError(t, c.generateSnapshot(ctx))
	require.NoError(t, c.generateSnapshot(ctx))

	after := clusterResourceDigests(t, c, "node-1")
	require.Equal(t, before, after,
		"regenerating the snapshot with unchanged inputs must be byte-identical: "+
			"a re-hashing resource makes every snapshot push a real CDS rebuild "+
			"(see agent/internal/xds/cache/ordering.go)")
}

// churnFixture builds a node holding one resident pod of ServiceAccount "echo"
// and three registry services, and returns the cache plus a live context.
func churnFixture(t *testing.T) (*SnapshotCache, context.Context) {
	t.Helper()
	c := newTestCache("node-1")
	ctx := context.Background()

	resident := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, resident, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/echo", "aether-test/svc-1", "aether-test/svc-2")

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/echo":  {makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080)},
				"aether-test/svc-1": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 18080)},
				"aether-test/svc-2": {makeEndpoint("10.0.0.11", "cluster-1", "node-2", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	return c, ctx
}

// requireServiceClustersEqual asserts every service cluster present in both
// digest maps is byte-identical, and that neither gained or lost one.
func requireServiceClustersEqual(t *testing.T, before, after map[string]string, why string) {
	t.Helper()
	n := 0
	for name, digest := range before {
		if !isServiceClusterName(name) {
			continue
		}
		n++
		require.Equalf(t, digest, after[name], "service cluster %s changed bytes: %s", name, why)
	}
	for name := range after {
		if !isServiceClusterName(name) {
			continue
		}
		require.Containsf(t, before, name, "service cluster %s appeared: %s", name, why)
	}
	require.GreaterOrEqual(t, n, 3, "the fixture must emit several service clusters")
}

// TestServiceClusterBytesStableWithinServiceAccount is the release-two
// invariant of issue #815, and the exact inverse of the characterisation it
// replaced (TestLocalPodChurnRewritesEveryServiceCluster).
//
// Every mesh service cluster carries a per-source upstream-mTLS matcher. It used
// to be keyed by the source pod's NETNS PATH, which is unique per pod, so one
// pod arriving or leaving rewrote a field of EVERY service cluster on the node;
// Envoy then rebuilt each one, each rebuilt EDS cluster warmed for the full 15 s
// EDS initial_fetch_timeout, and the warming→active swap drained every upstream
// connection pool on the node (20–33 clusters × 15 s per pod ADD, measured).
//
// The matcher is now keyed by the source SPIFFE ID — per ServiceAccount — so:
//
//   - a pod ADD/DEL of a ServiceAccount that ALREADY has a pod on the node
//     changes ZERO service-cluster bytes, which is what makes Envoy's
//     blockUpdate hash gate say "unchanged" and warm nothing;
//   - only the FIRST pod of a ServiceAccount arriving, or the LAST one leaving,
//     changes them — and that change is exactly reversible.
func TestServiceClusterBytesStableWithinServiceAccount(t *testing.T) {
	c, ctx := churnFixture(t)
	before := clusterResourceDigests(t, c, "node-1")
	require.NotEmpty(t, before)

	// A SECOND pod of the RESIDENT pod's ServiceAccount. The identity set is
	// unchanged, so no service cluster may move a byte.
	sibling := &cniv1.CNIPod{
		Name:             "echo-2",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-b",
	}
	require.NoError(t, c.AddPod(ctx, sibling, "aether.internal"))
	afterSiblingAdd := clusterResourceDigests(t, c, "node-1")
	requireServiceClustersEqual(t, before, afterSiblingAdd,
		"a second pod of a ServiceAccount already present on the node must not "+
			"perturb any cluster — that is the whole of issue #815 release two")

	// The sibling still brings its OWN per-pod clusters; adding a cluster is not
	// rewriting the existing ones, and Envoy warms only the new one.
	require.Contains(t, afterSiblingAdd, "inboundready_"+sibling.GetName())
	require.NotContains(t, before, "inboundready_"+sibling.GetName())
	for name, digest := range before {
		if !proxy.IsPerPodClusterName(name) {
			continue
		}
		require.Equalf(t, digest, afterSiblingAdd[name],
			"per-pod cluster %s changed bytes when a sibling pod was added; "+
				"per-pod clusters must not embed node-wide state", name)
	}

	// ...and removing it is equally free.
	require.NoError(t, c.RemovePod(ctx, sibling.GetNetworkNamespace()))
	afterSiblingDel := clusterResourceDigests(t, c, "node-1")
	requireServiceClustersEqual(t, before, afterSiblingDel,
		"removing a pod whose ServiceAccount still has another pod on the node must be free")
	require.NotContains(t, afterSiblingDel, "inboundready_"+sibling.GetName())
	require.Equal(t, before, afterSiblingDel, "the whole CDS set must be back to the pre-ADD bytes")
}

// TestServiceClusterBytesChangeOnFirstAndLastPodOfServiceAccount pins the other
// half: the matcher is still per-ServiceAccount state, so the FIRST pod of a new
// ServiceAccount and the LAST pod of a departing one DO rewrite every service
// cluster — reversibly. These are the only pod events that still cost a
// node-wide re-warm, and quantifying them is the point of the test.
func TestServiceClusterBytesChangeOnFirstAndLastPodOfServiceAccount(t *testing.T) {
	c, ctx := churnFixture(t)
	before := clusterResourceDigests(t, c, "node-1")

	// A pod of a ServiceAccount with no other pod on this node.
	newcomer := &cniv1.CNIPod{
		Name:             "svc-5-1",
		Namespace:        "aether-test",
		ServiceAccount:   "svc-5",
		NetworkNamespace: "/var/run/netns/cni-c",
	}
	require.NoError(t, c.AddPod(ctx, newcomer, "aether.internal"))
	afterAdd := clusterResourceDigests(t, c, "node-1")

	serviceClusters := 0
	for name, digest := range before {
		if !isServiceClusterName(name) {
			continue
		}
		serviceClusters++
		require.NotEqualf(t, digest, afterAdd[name],
			"cluster %s kept its bytes although a NEW source identity arrived; the "+
				"matcher must gain an entry for it or that pod's egress would fall to "+
				"on_no_match and present the node identity", name)
	}
	require.GreaterOrEqual(t, serviceClusters, 3)

	// Per-pod clusters of the resident pod stay untouched even here.
	for name, digest := range before {
		if !proxy.IsPerPodClusterName(name) {
			continue
		}
		require.Equalf(t, digest, afterAdd[name], "per-pod cluster %s changed bytes", name)
	}

	// The LAST pod of that ServiceAccount leaving restores the byte-for-byte
	// pre-ADD state: a create/destroy pair is a round trip, so repeated churn
	// cannot ratchet the cluster set into an ever-growing shape.
	require.NoError(t, c.RemovePod(ctx, newcomer.GetNetworkNamespace()))
	afterDel := clusterResourceDigests(t, c, "node-1")
	require.Equal(t, before, afterDel, "the DEL must restore the pre-ADD bytes exactly")
}

// TestServiceClusterBytesUnaffectedByPodChurnWithSpireOff: with no node SVID —
// which is what SPIRE-disabled looks like to this cache — no per-source matcher
// is injected at all (TestServiceClusterNoMTLSWithoutNodeIdentity), so release
// two must be a literal no-op there. Pod churn changes no service-cluster byte
// before or after the change, and no cluster mentions an identity or a netns.
func TestServiceClusterBytesUnaffectedByPodChurnWithSpireOff(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	declareDeps(c, "aether-test/echo", "aether-test/svc-1", "aether-test/svc-2")

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/echo":  {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 18080)},
				"aether-test/svc-1": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 18080)},
				"aether-test/svc-2": {makeEndpoint("10.0.0.11", "cluster-1", "node-2", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	before := clusterResourceDigests(t, c, "node-1")

	// NO SetNodeIdentity: nothing to present, so nothing to select.
	pod := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	requireServiceClustersEqual(t, before, clusterResourceDigests(t, c, "node-1"),
		"with SPIRE off a pod ADD must not touch a service cluster")

	require.NoError(t, c.RemovePod(ctx, pod.GetNetworkNamespace()))
	requireServiceClustersEqual(t, before, clusterResourceDigests(t, c, "node-1"),
		"with SPIRE off a pod DEL must not touch a service cluster")

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	for name, r := range snap.GetResources(resourcev3.ClusterType) {
		if !isServiceClusterName(name) {
			continue
		}
		cl, ok := r.(*clusterv3.Cluster)
		require.True(t, ok)
		require.Nilf(t, cl.GetTransportSocketMatcher(), "cluster %s must carry no matcher with SPIRE off", name)
		require.Emptyf(t, cl.GetTransportSocketMatches(), "cluster %s must carry no matches with SPIRE off", name)
	}
}

// TestNoNetnsPathInServiceClusterBytes is the release-two boundary on the cache
// side: no source pod's netns path may appear ANYWHERE in a service cluster's
// serialized bytes.
//
// The scan is over the raw delta-xDS wire bytes, which is deliberate: an
// embedded google.protobuf.Any carries its payload as plain nested serialized
// bytes, so a substring scan reaches inside every typed_config (the matcher
// input's FilterStateInput, each action's TransportSocketNameAction, the
// UpstreamTlsContexts) without having to know the nesting. A structural
// assertion on the matcher alone would miss a netns leaking into a socket name,
// an SNI or a metadata value.
func TestNoNetnsPathInServiceClusterBytes(t *testing.T) {
	c, ctx := churnFixture(t)

	// A second ServiceAccount, so the matcher has more than one entry.
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name:             "svc-5-1",
		Namespace:        "aether-test",
		ServiceAccount:   "svc-5",
		NetworkNamespace: "/var/run/netns/cni-c",
	}, "aether.internal"))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	needles := []string{"/var/run/netns/cni-a", "/var/run/netns/cni-c", "/var/run/netns/"}
	matchers := 0
	for name, r := range snap.GetResources(resourcev3.ClusterType) {
		if !isServiceClusterName(name) {
			// Per-pod app_/health_/inboundready_ clusters legitimately dial
			// inside the pod's netns and carry the path by design.
			continue
		}
		cl, ok := r.(*clusterv3.Cluster)
		require.True(t, ok)
		if cl.GetTransportSocketMatcher() != nil {
			matchers++
		}
		b, err := cachev3.MarshalResource(r)
		require.NoError(t, err)
		for _, needle := range needles {
			require.NotContainsf(t, string(b), needle,
				"service cluster %s still embeds a netns path (%q): the per-source "+
					"matcher must be keyed by source SPIFFE ID (issue #815 release two)", name, needle)
		}
	}
	require.GreaterOrEqual(t, matchers, 3,
		"the fixture must emit service clusters that actually carry the matcher")
}

// isServiceClusterName reports whether a cluster name is a registry-derived mesh
// service cluster (the ones that carry the per-source mTLS matcher), as opposed
// to a per-pod app_/health_/inboundready_ delivery or probe cluster.
func isServiceClusterName(name string) bool {
	return !proxy.IsPerPodClusterName(name)
}
