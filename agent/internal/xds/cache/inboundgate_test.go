package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// main-worker-05, 2026-09-19: the node served its node SVID at 17:31:00.051Z
// and still emitted NO inboundready_* cluster for its whole agent lifetime,
// while its health_* clusters exported normally. Nothing said so.
//
// The structural cause is that #819 hung the probe recompute off exactly two
// one-shot events — a bulk listener load, and SetNodeIdentity, which the SPIRE
// bridge calls once ever (`if firstServe`). Any ordering in which both fire
// while a precondition is still missing leaves the gate absent permanently:
// there is no third trigger. These tests pin the two orderings and the
// convergence that now makes the ordering irrelevant.

// TestInboundGateConvergesAfterMissedTriggers is the main-worker-05 regression.
// Both of the old triggers fire while a precondition is missing — the node SVID
// lands before any listener exists, and the listener load happens with the node
// identity already set but through a path that does not recompute — and the
// gate must STILL be programmed on the next snapshot.
func TestInboundGateConvergesAfterMissedTriggers(t *testing.T) {
	allowAnyNetns(t)
	c := newTestCache("node-1")
	ctx := context.Background()

	// 1. The node SVID arrives before the cache holds a single listener. The
	//    recompute walks an empty map: nothing to gate, and this is the ONLY
	//    SetNodeIdentity call the bridge will ever make.
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	// 2. Pods appear afterwards, through a path that does not recompute the
	//    probes itself.
	pod := raceTestPod("svc-1", "/var/run/netns/cni-a")
	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(ctx, types.ContainerID("c-1"), pod))
	require.NoError(t, c.LoadListenersFromStorage(ctx, store, raceTrustDomain))

	assert.Contains(t, clusterNames(t, c), proxy.InboundReadyClusterName(pod),
		"the gate must converge on the next snapshot, whichever trigger was missed")
	assert.Equal(t,
		[]string{proxy.InboundReadyClusterName(pod)},
		gatewayMinHealthy(t, c)[proxy.HealthGatewayPath(proxy.InboundReadyClusterName(pod))])
}

// TestInboundGateConvergesWhenTrustDomainArrivesLast is the same defect seen
// from the trust-domain side: the node SVID is served while the cache's trust
// domain is still empty, so inboundReadyClusterFor refuses. Nothing re-ran it
// in #819. It must now be programmed as soon as the trust domain lands.
func TestInboundGateConvergesWhenTrustDomainArrivesLast(t *testing.T) {
	allowAnyNetns(t)
	c := newTestCache("node-1")
	ctx := context.Background()

	pod := raceTestPod("svc-1", "/var/run/netns/cni-a")
	require.NoError(t, c.AddPod(ctx, pod, raceTrustDomain))
	c.trustDomain.Store("") // the window, re-opened
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NotContains(t, clusterNames(t, c), proxy.InboundReadyClusterName(pod),
		"with no trust domain there is no identity to pin, so no probe")

	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	assert.Contains(t, clusterNames(t, c), proxy.InboundReadyClusterName(pod),
		"the trust domain arriving must materialise the gate with no further trigger")
}

// TestInboundGateRecomputeIsAStableNoOp: the recompute runs on every snapshot,
// so it must be free in the steady state — an entry already rendered from the
// current identity is not rebuilt, and the emitted bytes never change (the
// #135 delta-xDS invariant that TestSnapshotRegenerationIsANoOpPush guards).
func TestInboundGateRecomputeIsAStableNoOp(t *testing.T) {
	allowAnyNetns(t)
	c := newTestCache("node-1")
	ctx := context.Background()

	pod := raceTestPod("svc-1", "/var/run/netns/cni-a")
	require.NoError(t, c.AddPod(ctx, pod, raceTrustDomain))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	before := clusterNames(t, c)[proxy.InboundReadyClusterName(pod)]
	require.NotNil(t, before)

	gated, ungated := c.recomputeInboundReadyClusters()
	assert.Equal(t, 1, gated)
	assert.Zero(t, ungated)

	after := clusterNames(t, c)[proxy.InboundReadyClusterName(pod)]
	assert.Same(t, before, after, "an entry already current must not be re-rendered")
}

// TestInboundGateMissingReasons: every precondition the gate can wait on has a
// name, because "gate absent" with no reason is what cost a whole node's
// observability on 2026-09-19.
func TestInboundGateMissingReasons(t *testing.T) {
	pod := raceTestPod("svc-1", "/var/run/netns/cni-a")
	full := inboundReadyIdentity{nodeSpiffeID: nodeIdentity, trustDomain: raceTrustDomain}

	spireOff := newTestCache("node-1")
	spireOff.SetSpireEnabled(false)
	assert.Contains(t, spireOff.inboundGateMissing(pod, full), "SPIRE disabled")

	c := newTestCache("node-1")
	assert.Contains(t, c.inboundGateMissing(pod, inboundReadyIdentity{nodeSpiffeID: nodeIdentity}), "trust domain")
	assert.Contains(t, c.inboundGateMissing(pod, inboundReadyIdentity{trustDomain: raceTrustDomain}), "node SVID")
	assert.Contains(t, c.inboundGateMissing(raceTestPod("svc-1", ""), full), "network namespace")
	assert.Empty(t, c.inboundGateMissing(pod, full), "with every precondition met the gate must report nothing missing")
}
