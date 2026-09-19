package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
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

// TestLocalPodChurnRewritesEveryServiceCluster characterises the blast radius of
// one local pod's lifecycle on the node's CDS set.
//
// Every mesh service cluster carries a per-source upstream-mTLS matcher whose
// exact_match_map is keyed by the SOURCE POD'S NETNS PATH
// (proxy.InjectUpstreamMTLS ← recomputeMTLSClusters ← setLocalWorkload /
// removeLocalWorkload). So one pod arriving or leaving changes a field of EVERY
// service cluster the node holds — not just the clusters that pod talks to, and
// not just the clusters of that pod's own service. Under delta-xDS that is a
// genuine content change on all of them, so Envoy is told every cluster changed
// and rebuilds each one.
//
// The test asserts the two halves that are invariants regardless of how the
// matcher is built: the churn is total (every service cluster is affected, so
// nobody should reason about it as a local effect), and it is exactly reversible
// (the DEL restores the pre-ADD bytes, which is what makes a create/destroy pair
// a round trip rather than a ratchet).
func TestLocalPodChurnRewritesEveryServiceCluster(t *testing.T) {
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

	before := clusterResourceDigests(t, c, "node-1")

	// A pod of an UNRELATED service, which this node's existing clusters have
	// no routing relationship with.
	newcomer := &cniv1.CNIPod{
		Name:             "svc-5-1",
		Namespace:        "aether-test",
		ServiceAccount:   "svc-5",
		NetworkNamespace: "/var/run/netns/cni-b",
	}
	require.NoError(t, c.AddPod(ctx, newcomer, "aether.internal"))
	afterAdd := clusterResourceDigests(t, c, "node-1")

	// The RESIDENT pod's own per-pod clusters must be untouched by a stranger
	// arriving. That includes inboundready_<pod> (issue #815), which is
	// node-identity + pod scoped and deliberately NOT run through
	// InjectUpstreamMTLS' per-source matcher: adding one pod must add exactly
	// one cluster, not perturb the other pods'.
	for name, digest := range before {
		if !proxy.IsPerPodClusterName(name) {
			continue
		}
		require.Equalf(t, digest, afterAdd[name],
			"per-pod cluster %s changed bytes when an unrelated pod was added; "+
				"per-pod clusters must not embed node-wide state", name)
	}
	require.Contains(t, afterAdd, "inboundready_"+newcomer.GetName(),
		"the new pod must bring its own inbound-readiness probe cluster")
	require.NotContains(t, before, "inboundready_"+newcomer.GetName())

	// Every service cluster — the bare FQDN and each per-port alias — must have
	// been rewritten. The per-pod app_/health_/inboundready_ STATIC clusters
	// carry no per-source upstream mTLS and are deliberately not asserted on.
	serviceClusters := 0
	for name, digest := range before {
		if !isServiceClusterName(name) {
			continue
		}
		serviceClusters++
		require.NotEqualf(t, digest, afterAdd[name],
			"cluster %s kept its bytes across an unrelated pod ADD; if the per-source "+
				"transport-socket matcher no longer embeds the local pod set, this "+
				"characterisation is stale and the CDS-churn note on it should be revisited", name)
	}
	require.GreaterOrEqual(t, serviceClusters, 3, "the fixture must emit several service clusters")

	// The pod leaving must restore the byte-for-byte pre-ADD state: a
	// create/destroy pair is a round trip, so repeated churn cannot ratchet the
	// cluster set into an ever-growing shape.
	require.NoError(t, c.RemovePod(ctx, newcomer.GetNetworkNamespace()))
	afterDel := clusterResourceDigests(t, c, "node-1")
	for name, digest := range before {
		require.Equalf(t, digest, afterDel[name],
			"cluster %s did not return to its pre-ADD bytes after the pod DEL", name)
	}
	require.NotContains(t, afterDel, "inboundready_"+newcomer.GetName(),
		"the departing pod's inbound-readiness probe must go with it")
}

// isServiceClusterName reports whether a cluster name is a registry-derived mesh
// service cluster (the ones that carry the per-source mTLS matcher), as opposed
// to a per-pod app_/health_/inboundready_ delivery or probe cluster.
func isServiceClusterName(name string) bool {
	return !proxy.IsPerPodClusterName(name)
}
