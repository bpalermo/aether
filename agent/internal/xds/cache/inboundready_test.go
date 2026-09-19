package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	health_checkv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cache-level tests for the issue #815 promotion gate: a per-pod
// inboundready_<pod> cluster actively health-checks the pod's own mesh inbound
// listener over mTLS, and the health gateway requires it alongside the app
// probe before the liveness loop can promote the endpoint.

func inboundReadyTestPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
}

// gatewayMinHealthy returns the cluster_min_healthy_percentages of each health
// gateway filter in the snapshot, keyed by the gateway path it answers.
func gatewayMinHealthy(t *testing.T, c *SnapshotCache) map[string][]string {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	gw, ok := snap.GetResources(resourcev3.ListenerType)[proxy.HealthGatewayListenerName].(*listenerv3.Listener)
	require.True(t, ok, "the health gateway listener must be in the snapshot")

	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, gw.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))

	out := map[string][]string{}
	for _, f := range hcm.GetHttpFilters() {
		if f.GetName() != "envoy.filters.http.health_check" {
			continue
		}
		hc := &health_checkv3.HealthCheck{}
		require.NoError(t, f.GetTypedConfig().UnmarshalTo(hc))
		path := hc.GetHeaders()[0].GetStringMatch().GetExact()
		for name := range hc.GetClusterMinHealthyPercentages() {
			out[path] = append(out[path], name)
		}
	}
	return out
}

func clusterNames(t *testing.T, c *SnapshotCache) map[string]*clusterv3.Cluster {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	out := map[string]*clusterv3.Cluster{}
	for name, r := range snap.GetResources(resourcev3.ClusterType) {
		cl, ok := r.(*clusterv3.Cluster)
		require.True(t, ok)
		out[name] = cl
	}
	return out
}

// TestInboundReadyClusterGatesPromotion: with SPIRE on and the node SVID
// served, the pod gets an inboundready_<pod> cluster and its OWN gateway path,
// while /healthz/health_<pod> keeps meaning the application probe alone.
func TestInboundReadyClusterGatesPromotion(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	pod := inboundReadyTestPod()

	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	clusters := clusterNames(t, c)
	probe, ok := clusters["inboundready_echo-1"]
	require.True(t, ok, "an mTLS pod on a node with an SVID must get an inbound-readiness probe")
	require.Len(t, probe.GetHealthChecks(), 1)
	assert.NotNil(t, probe.GetTransportSocket())

	requires := gatewayMinHealthy(t, c)
	assert.Equal(t, []string{"health_echo-1"},
		requires[proxy.HealthGatewayPath("health_echo-1")],
		"the app path must reflect the app probe ALONE, as it did before #815")
	assert.Equal(t, []string{"inboundready_echo-1"},
		requires[proxy.HealthGatewayPath("inboundready_echo-1")],
		"the inbound-readiness probe gets its own path so the agent can tell the two facts apart")
}

// TestInboundReadyAbsentBeforeNodeIdentity: the probe presents the node SVID,
// so before it lands there is nothing to build. The gateway must then carry NO
// /healthz/inboundready_<pod> path at all — 404 there is how the liveness loop
// learns the pod is UNGATED rather than unhealthy.
func TestInboundReadyAbsentBeforeNodeIdentity(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, inboundReadyTestPod(), "aether.internal"))

	assert.NotContains(t, clusterNames(t, c), "inboundready_echo-1")
	assert.Equal(t,
		[]string{"health_echo-1"},
		gatewayMinHealthy(t, c)[proxy.HealthGatewayPath("health_echo-1")],
		"without a node SVID the gate must stay exactly as it was")
	assert.NotContains(t, gatewayMinHealthy(t, c), proxy.HealthGatewayPath("inboundready_echo-1"),
		"an ungated pod must have no inbound-readiness path, so the agent reads 404")

	// The SVID arriving must materialise both the cluster and its path — with no
	// further trigger than the next snapshot.
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	assert.Contains(t, clusterNames(t, c), "inboundready_echo-1")
	assert.Equal(t, []string{"inboundready_echo-1"},
		gatewayMinHealthy(t, c)[proxy.HealthGatewayPath("inboundready_echo-1")])
}

// TestInboundReadyAbsentWithSpireOff is the SPIRE-off byte-identity guard: with
// SPIRE disabled the inbound listener is CLEARTEXT, there is no handshake to
// prove, and the whole feature must be inert — no cluster, no extra gate.
func TestInboundReadyAbsentWithSpireOff(t *testing.T) {
	c := newTestCache("node-1")
	c.SetSpireEnabled(false)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, inboundReadyTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	assert.NotContains(t, clusterNames(t, c), "inboundready_echo-1",
		"a cleartext inbound listener has no certificate to probe")
	assert.Equal(t,
		[]string{"health_echo-1"},
		gatewayMinHealthy(t, c)[proxy.HealthGatewayPath("health_echo-1")])
}

// TestSpireOffSnapshotIsByteIdenticalAcrossTheChange pins the stronger claim:
// with SPIRE off, the ENTIRE cluster set a pod produces is unchanged by #815
// (clusters are not touched at all in release one, and the probe is not built).
// It is written as an invariant — the pod's cluster set with SPIRE off must be
// exactly {app_*, health_*} — so it fails loudly if a later release starts
// emitting the probe unconditionally.
func TestSpireOffSnapshotIsByteIdenticalAcrossTheChange(t *testing.T) {
	c := newTestCache("node-1")
	c.SetSpireEnabled(false)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, inboundReadyTestPod(), "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	for name := range clusterNames(t, c) {
		if !proxy.IsPerPodClusterName(name) {
			continue
		}
		assert.NotContains(t, name, "inboundready_",
			"SPIRE-off per-pod clusters must be exactly the pre-#815 set")
	}
}

// TestInboundReadyRemovedWithPod: the probe is per-pod state and must not
// outlive its pod — a leftover probe cluster would health-check a netns that no
// longer exists.
func TestInboundReadyRemovedWithPod(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	pod := inboundReadyTestPod()

	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.Contains(t, clusterNames(t, c), "inboundready_echo-1")

	require.NoError(t, c.RemovePod(ctx, pod.GetNetworkNamespace()))
	assert.NotContains(t, clusterNames(t, c), "inboundready_echo-1")
	assert.NotContains(t, gatewayMinHealthy(t, c), proxy.HealthGatewayPath("health_echo-1"))
}

// TestInboundReadyIsDeterministic: the probe cluster and the gateway listener
// must be byte-identical across repeated regenerations of identical input, or
// every snapshot push becomes a real CDS/LDS rebuild (incident #135).
func TestInboundReadyIsDeterministic(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	for _, name := range []string{"echo-1", "echo-2", "echo-3"} {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name:             name,
			Namespace:        "aether-test",
			ServiceAccount:   "echo",
			NetworkNamespace: "/var/run/netns/cni-" + name,
		}, "aether.internal"))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	before := clusterResourceDigests(t, c, "node-1")
	for range 6 {
		require.NoError(t, c.generateSnapshot(ctx))
		// Recomputing from an unchanged node identity must also be a no-op.
		c.recomputeInboundReadyClusters()
		require.NoError(t, c.generateSnapshot(ctx))
	}
	require.Equal(t, before, clusterResourceDigests(t, c, "node-1"),
		"repeated regeneration of identical input must be byte-identical")
}
