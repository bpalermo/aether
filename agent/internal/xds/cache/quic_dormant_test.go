package cache

import (
	"context"
	"path/filepath"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1036 gates, at the cache: a pair whose twin the proxy fetched on demand
// is never forgotten. Envoy's ODCDS manager keeps one subscription per name
// for the life of the process: when the agent removes a twin it fetched on
// demand, Envoy drops the cluster and keeps the subscription, and every later
// on-demand request for the name is "already subscribed, skipping" -- 503 NC
// at the on_demand timeout, until the proxy restarts. So when the source's
// last pod leaves the node (or the destination leaves the dependency set) the pair goes
// DORMANT: the twin leaves the snapshot as before, and it is republished --
// with no request, the subscription is still open -- the moment the pair is
// valid again.
//
// Red on the #1035 cache (pairs forgotten on removal evidence): after the
// source returns no twin is published, and in production nothing ever asks.

func addSourcePod(t *testing.T, c *SnapshotCache, sa, netns string) {
	t.Helper()
	require.NoError(t, c.AddPod(context.Background(), &cniv1.CNIPod{
		Name: sa + "-1", Namespace: "demo", ServiceAccount: sa,
		NetworkNamespace: netns,
	}, quicDemandTD))
}

// Source leaves -> dormant -> source returns -> republished by the very
// snapshot the returning pod's AddPod publishes, with no request.
func TestQUICDormantPairRepublishedWhenSourceReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	ctx := context.Background()
	twinA := echoTwin(c, "source-a")
	observeQUIC(t, c, "demo/echo", "demo/source-a")
	require.Equal(t, []string{twinA}, readQUICState(t, c).twins)

	// A's last pod leaves (a Deployment roll: the old pod goes first).
	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-source-a"))
	st := readQUICState(t, c)
	assert.Empty(t, st.twins, "source-a left the node: its twin leaves the snapshot")
	assert.Empty(t, st.twinCLAs)
	assert.Empty(t, c.QUICPairs())
	assert.Equal(t, []string{twinA}, c.DormantQUICPairs(), "the proxy holds a subscription for the twin: the pair is dormant, not forgotten")
	c.FlushObservedUpstreams()
	stored := readStore(t, path)
	assert.Empty(t, stored.GetQuicPairs())
	require.Len(t, stored.GetDormantQuicPairs(), 1, "the dormant pair is persisted")
	assert.Equal(t, "demo/source-a", stored.GetDormantQuicPairs()[0].GetSource())

	// A pod of the same ServiceAccount lands on the node again. Nothing
	// requests the twin (Envoy would skip it: already subscribed).
	addSourcePod(t, c, "source-a", "/var/run/netns/cni-source-a-new")
	st = readQUICState(t, c)
	assert.Equal(t, []string{twinA}, st.twins, "the returning source's twin must be republished with no request (issue #1036)")
	assert.Equal(t, []string{twinA}, st.twinCLAs, "with its own load assignment, in the same snapshot")
	requireSelections(t, c, st, allArms(c, quicDemandSAs...))
	assert.Equal(t, []string{twinA}, c.QUICPairs())
	assert.Empty(t, c.DormantQUICPairs())
	c.FlushObservedUpstreams()
	stored = readStore(t, path)
	assert.Len(t, stored.GetQuicPairs(), 1)
	assert.Empty(t, stored.GetDormantQuicPairs())
}

// Destination leaves the dependency set -> dormant -> returns -> republished.
func TestQUICDormantPairRepublishedWhenDestinationReturns(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	twinA, twinB := echoTwin(c, "source-a"), echoTwin(c, "source-b")
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")

	declareDeps(c)
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, readQUICState(t, c).twins, "out of the dependency set: no twin")
	assert.Equal(t, []string{twinA, twinB}, c.DormantQUICPairs())

	declareDeps(c, "demo/echo")
	require.NoError(t, c.generateSnapshot(ctx))
	st := readQUICState(t, c)
	assert.ElementsMatch(t, []string{twinA, twinB}, st.twins, "back in the dependency set: both twins republished with no request")
	assert.ElementsMatch(t, []string{twinA, twinB}, st.twinCLAs)
	assert.Empty(t, c.DormantQUICPairs())
}

// A pair the proxy holds NO subscription for (restored from storage, never
// fetched or re-subscribed in this process) is forgotten as before: the proxy
// drops such a cluster outright and its next request re-fetches.
func TestQUICDormantOnlyForSubscribedPairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	observeQUIC(t, c, "demo/echo", "demo/source-a")
	c.FlushObservedUpstreams()

	restarted := newQUICDemandCache(t, path)
	require.Equal(t, []string{echoTwin(restarted, "source-a")}, restarted.QUICPairs())
	require.NoError(t, restarted.RemovePod(context.Background(), "/var/run/netns/cni-source-a"))
	assert.Empty(t, restarted.QUICPairs())
	assert.Empty(t, restarted.DormantQUICPairs(), "no subscription behind it: forgotten")
}

// Dormant pairs survive an agent restart. A restored dormant pair whose source
// is on the node at load is republished in the first snapshot after the pod
// set is known, before any request; one whose source is still away stays
// dormant until the proxy's fresh stream re-states its subscriptions, which
// keeps it if named and prunes it if not.
func TestQUICDormantPairsPersistAcrossAgentRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	ctx := context.Background()
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b", "demo/source-c")
	for _, sa := range []string{"source-a", "source-b", "source-c"} {
		require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-"+sa))
	}
	require.Len(t, c.DormantQUICPairs(), 3)
	c.FlushObservedUpstreams()
	require.Len(t, readStore(t, path).GetDormantQuicPairs(), 3)

	// The restarted agent loads with source-a back on the node, b and c away.
	restarted := newQUICDemandCacheWith(t, path, "source-a")
	twinA, twinB, twinC := echoTwin(restarted, "source-a"), echoTwin(restarted, "source-b"), echoTwin(restarted, "source-c")
	st := readQUICState(t, restarted)
	assert.Equal(t, []string{twinA}, st.twins, "a restored dormant pair whose source is present is republished before any request")
	assert.Equal(t, []string{twinA}, restarted.QUICPairs())
	assert.Equal(t, []string{twinB, twinC}, restarted.DormantQUICPairs(), "absent sources stay dormant across the restart")

	// The proxy's fresh stream re-subscribes b's twin (and a's), not c's.
	restarted.RestateQUICSubscriptions(ctx, testQUICStream, []string{twinA, twinB})
	assert.Equal(t, []string{twinB}, restarted.DormantQUICPairs(), "c's subscription is gone: pruned; b's is held: kept")
	restarted.FlushObservedUpstreams()
	stored := readStore(t, path)
	require.Len(t, stored.GetDormantQuicPairs(), 1)
	assert.Equal(t, "demo/source-b", stored.GetDormantQuicPairs()[0].GetSource())

	// b returns: republished.
	addSourcePod(t, restarted, "source-b", "/var/run/netns/cni-source-b-new")
	assert.ElementsMatch(t, []string{twinA, twinB}, readQUICState(t, restarted).twins)
}

// A proxy generation change (hot restart): the child's fresh stream holds no
// on-demand subscription, so its first CDS request names none. The draining
// parent still holds its subscriptions until it exits, so the dormant pairs it
// vouches for stay dormant while its stream is live (issue #1052), and are
// pruned when that stream ends. After that, a removed twin is forgotten (the
// child holds it only through the wildcard) until a request subscribes it
// again.
func TestQUICDormantPairsPrunedOnProxyGenerationChange(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")
	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-source-a"))
	require.Equal(t, []string{echoTwin(c, "source-a")}, c.DormantQUICPairs())

	const child int64 = testQUICStream + 1
	assert.Zero(t, c.RestateQUICSubscriptions(ctx, child, nil))
	assert.Equal(t, []string{echoTwin(c, "source-a")}, c.DormantQUICPairs(),
		"the parent generation's stream is still live and holds the subscription: still dormant")
	assert.Equal(t, 1, c.CloseQUICStream(ctx, testQUICStream))
	assert.Empty(t, c.DormantQUICPairs(), "the parent exited; the new generation holds no subscription: dormant pairs are pruned")

	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-source-b"))
	assert.Empty(t, c.DormantQUICPairs(), "b's twin is not subscribed by the new generation: forgotten")

	addSourcePod(t, c, "source-a", "/var/run/netns/cni-source-a-new")
	assert.Empty(t, readQUICState(t, c).twins, "a pruned dormant pair is not republished: its next request fetches it")
}

// A first-use request the agent refuses still opens a subscription Envoy keeps:
// the pair is parked dormant and published once it becomes valid, so only the
// refused request 503s, not every one after it.
func TestQUICRefusedFirstUseIsParkedDormant(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	late := echoTwin(c, "late")

	decision, reason := c.ObserveQUICTwin(ctx, testQUICStream, late)
	require.Equal(t, QUICTwinRefused, decision)
	require.Equal(t, QUICRefusedSourceNotOnNode, reason)
	assert.Empty(t, c.QUICPairs())
	assert.Equal(t, []string{late}, c.DormantQUICPairs())

	addSourcePod(t, c, "late", "/var/run/netns/cni-late")
	assert.Equal(t, []string{late}, readQUICState(t, c).twins, "the subscribed twin is published when its source arrives")
}
