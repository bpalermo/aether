package cache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The demand-scoped QUIC twin gates (issue #1020).
//
// Before #1020 the cache built one `quic:` twin per local ServiceAccount for
// every QUIC destination (then an allow-list, unconditional since #979), up
// front: on rev242, 118 twins fleet-wide of
// which 2 ever carried a request. Now every local ServiceAccount still gets a
// selection ARM on the destination's routes, but the twin behind it is built
// only once that source has dialled: its first request routes to the missing
// name, Envoy's on_demand filter asks for it over ODCDS, and ObserveQUICTwin
// validates, records (persisted, in ObservedUpstreams) and publishes it.

const quicDemandTD = "aether.internal"

var quicDemandSAs = []string{"source-a", "source-b", "source-c"}

// newQUICDemandCache is a node with three local ServiceAccounts, "demo/echo"
// in the dependency set (so QUIC-eligible: east-west QUIC is unconditional,
// #979) and "demo/other" registered but NOT a dependency of this node.
// storePath, when set, turns on persistence of the observed set there BEFORE
// the first snapshot, as the agent does at boot.
func newQUICDemandCache(t *testing.T, storePath string) *SnapshotCache {
	t.Helper()
	return newQUICDemandCacheWith(t, storePath, quicDemandSAs...)
}

// newQUICDemandCacheWith is newQUICDemandCache with only the given local
// ServiceAccounts on the node at load.
func newQUICDemandCacheWith(t *testing.T, storePath string, localSAs ...string) *SnapshotCache {
	t.Helper()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.observedFlushDebounce = time.Millisecond
	ctx := context.Background()
	if storePath != "" {
		c.EnableObservedUpstreamsStore(ctx, storePath)
		// Drain the debounced write before t.TempDir's cleanup removes the
		// directory (cleanups run LIFO; this one is registered after it), or a
		// late flush races RemoveAll ("directory not empty").
		t.Cleanup(c.FlushObservedUpstreams)
	}
	for _, sa := range localSAs {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: sa + "-0", Namespace: "demo", ServiceAccount: sa,
			NetworkNamespace: "/var/run/netns/cni-" + sa,
		}, quicDemandTD))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	c.SetCaptureAuthorities(map[string]string{"demo/echo": "echo.demo.svc.cluster.local", "demo/other": "other.demo.svc.cluster.local"})
	declareDeps(c, "demo/echo")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/echo":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
				"demo/other": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	// The pod set is complete (production: LoadListenersFromStorage).
	c.markLocalPodsSynced()
	require.NoError(t, c.generateSnapshot(ctx))
	return c
}

func echoTwin(c *SnapshotCache, sa string) string {
	return proxy.QUICClusterName("demo/echo", c.meshDomain, "demo/"+sa)
}

func spiffeOf(sa string) string { return "spiffe://" + quicDemandTD + "/ns/demo/sa/" + sa }

// quicState is what one snapshot says about east-west QUIC.
type quicState struct {
	snap cachev3.ResourceSnapshot
	// twins / twinCLAs are the `quic:` clusters and the load assignments
	// published under a `quic:` name.
	twins    []string
	twinCLAs []string
	// selections is every route carrying the selection plugin, keyed by
	// "<route table>/<vhost>".
	selections map[string]quicSelection
}

type quicSelection struct {
	arms    map[string]string
	noMatch string
}

func readQUICState(t *testing.T, c *SnapshotCache) quicState {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	st := quicState{snap: snap, selections: map[string]quicSelection{}}
	for name := range snap.GetResources(resourcev3.ClusterType) {
		if strings.HasPrefix(name, "quic:") {
			st.twins = append(st.twins, name)
		}
	}
	for name := range snap.GetResources(resourcev3.EndpointType) {
		if strings.HasPrefix(name, "quic:") {
			st.twinCLAs = append(st.twinCLAs, name)
		}
	}
	for table, res := range snap.GetResources(resourcev3.RouteType) {
		for _, vh := range res.(*routev3.RouteConfiguration).GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				if arms, noMatch, ok := proxy.QUICSelectionArms(r); ok {
					st.selections[table+"/"+vh.GetName()] = quicSelection{arms: arms, noMatch: noMatch}
				}
			}
		}
	}
	return st
}

// allArms is the arm set every echo route must carry: one per local SA,
// whether or not its twin is built.
func allArms(c *SnapshotCache, sas ...string) map[string]string {
	out := map[string]string{}
	for _, sa := range sas {
		out[spiffeOf(sa)] = echoTwin(c, sa)
	}
	return out
}

// requireSelections asserts the echo routes on BOTH route tables select with
// exactly want, falling back to the h2 cluster.
func requireSelections(t *testing.T, c *SnapshotCache, st quicState, want map[string]string) {
	t.Helper()
	echo := proxy.ServiceClusterName("demo/echo", c.meshDomain)
	require.GreaterOrEqual(t, len(st.selections), 2, "the echo vhost on BOTH out_http and cap_http must select")
	for where, sel := range st.selections {
		assert.NotContains(t, where, "other", "a destination outside the dependency set must not select")
		assert.Equal(t, want, sel.arms, "%s: arms", where)
		assert.Equal(t, echo, sel.noMatch, "%s: on_no_match must stay the h2 cluster", where)
	}
}

// (a) A QUIC-eligible destination with three local ServiceAccounts and NO
// observed pairs builds ZERO twins and publishes ZERO twin load assignments.
// The routes still carry one arm per ServiceAccount -- each naming a twin that
// is not in CDS, which is what sends a source's first request to ODCDS -- and
// on_no_match is the h2 cluster.
//
// Red on the pre-#1020 cache: it built all three twins up front.
func TestQUICDemandBuildsNoTwinBeforeAnyPairDialled(t *testing.T) {
	c := newQUICDemandCache(t, "")
	st := readQUICState(t, c)

	assert.Empty(t, st.twins, "no pair has dialled: no twin may exist (issue #1020)")
	assert.Empty(t, st.twinCLAs, "no twin, so no twin load assignment")
	requireSelections(t, c, st, allArms(c, quicDemandSAs...))
	for _, name := range allArms(c, quicDemandSAs...) {
		_, inCDS := st.snap.GetResources(resourcev3.ClusterType)[name]
		assert.False(t, inCDS, "arm target %s must be fetched on demand, not pre-built", name)
	}
	assert.Empty(t, c.QUICPairs())
}

// (b) An on-demand request for `quic:<echo>@demo/source-b` builds exactly
// that twin, and the snapshot that introduces it also carries its own load
// assignment (the #1008 rule: an on-demand twin is always a LATE twin) and
// the arm that selects it. Nothing is built for source-a or source-c.
func TestQUICDemandOnDemandRequestBuildsExactlyThatTwin(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	twinB := echoTwin(c, "source-b")

	decision, reason := c.ObserveQUICTwin(ctx, testQUICStream, twinB)
	require.Equal(t, QUICTwinAdded, decision, reason)

	var st quicState
	require.Eventually(t, func() bool {
		st = readQUICState(t, c)
		return len(st.twins) > 0
	}, 5*time.Second, eventuallyTick, "the admitted twin never reached the snapshot")

	assert.Equal(t, []string{twinB}, st.twins, "exactly the requested twin")
	assert.Equal(t, []string{twinB}, st.twinCLAs, "its load assignment rides the SAME snapshot as the twin")
	cl := st.snap.GetResources(resourcev3.ClusterType)[twinB].(*clusterv3.Cluster)
	assert.Equal(t, twinB, cl.GetEdsClusterConfig().GetServiceName(), "the twin subscribes to its own EDS name (#1008)")
	cla := st.snap.GetResources(resourcev3.EndpointType)[twinB].(*endpointv3.ClusterLoadAssignment)
	assert.Len(t, cla.GetEndpoints(), 1, "the twin's load assignment is the base's membership")
	requireSelections(t, c, st, allArms(c, quicDemandSAs...))
	assert.Equal(t, []string{twinB}, c.QUICPairs())

	// A repeat request (a second listener, a reconnecting stream) is a no-op.
	decision, _ = c.ObserveQUICTwin(ctx, testQUICStream, twinB)
	assert.Equal(t, QUICTwinKnown, decision)
}

// (c) The pair persists: a replaced agent that restores the observed set from
// local storage builds the twin in its FIRST snapshot, before any request.
func TestQUICDemandPairSurvivesAgentRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	d, reason := c.recordQUICPair(testQUICStream, echoTwin(c, "source-b"))
	require.Equal(t, QUICTwinAdded, d, reason)
	c.FlushObservedUpstreams()

	stored := readStore(t, path)
	require.Len(t, stored.GetQuicPairs(), 1, "the pair must be persisted")
	assert.Equal(t, "demo/echo", stored.GetQuicPairs()[0].GetService())
	assert.Equal(t, "demo/source-b", stored.GetQuicPairs()[0].GetSource())

	restarted := newQUICDemandCache(t, path)
	st := readQUICState(t, restarted)
	assert.Equal(t, []string{echoTwin(restarted, "source-b")}, st.twins, "the restored pair's twin must be in the first snapshot")
	assert.Equal(t, []string{echoTwin(restarted, "source-b")}, st.twinCLAs)
}

// (d) Pruning is on removal evidence only: the last pod of a source
// ServiceAccount leaving drops that source's twin, its arm and its persisted
// pair; the destination leaving the dependency set drops the rest.
func TestQUICDemandPrunesWhenSourceLeavesOrDestinationLeavesDependencySet(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	ctx := context.Background()
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")
	require.ElementsMatch(t, []string{echoTwin(c, "source-a"), echoTwin(c, "source-b")}, readQUICState(t, c).twins)

	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-source-b"))
	st := readQUICState(t, c)
	assert.Equal(t, []string{echoTwin(c, "source-a")}, st.twins, "source-b left the node: its twin must go")
	assert.Equal(t, []string{echoTwin(c, "source-a")}, st.twinCLAs)
	requireSelections(t, c, st, allArms(c, "source-a", "source-c"))
	assert.Equal(t, []string{echoTwin(c, "source-a")}, c.QUICPairs())
	c.FlushObservedUpstreams()
	require.Len(t, readStore(t, path).GetQuicPairs(), 1, "the pruned pair must leave the persisted set")

	declareDeps(c)
	require.NoError(t, c.generateSnapshot(ctx))
	st = readQUICState(t, c)
	assert.Empty(t, st.twins, "the destination left the dependency set: no twin may survive")
	assert.Empty(t, st.selections)
	assert.Empty(t, c.QUICPairs())
	c.FlushObservedUpstreams()
	assert.Empty(t, readStore(t, path).GetQuicPairs())
}

// A persisted pair whose source is not (yet) local is kept until the pod
// records are loaded -- the restore can run before them -- and pruned after.
func TestQUICDemandRestoredPairWaitsForThePodSetBeforePruning(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "demo/echo")
	gone := proxy.QUICClusterName("demo/echo", c.meshDomain, "demo/gone")
	c.quicPairs[quicPair{service: "demo/echo", source: "demo/gone"}] = time.Now()

	require.NoError(t, c.generateSnapshot(context.Background()))
	assert.Equal(t, []string{gone}, c.QUICPairs(), "before the pod set is known, absence is not evidence")

	c.markLocalPodsSynced()
	require.NoError(t, c.generateSnapshot(context.Background()))
	assert.Empty(t, c.QUICPairs(), "after it is, the pair's source has left")
}

// (e) Refusals: a destination outside the dependency set, a source that is not
// a local ServiceAccount, and names QUICClusterName would never produce.
// Nothing is recorded and no snapshot changes.
func TestQUICDemandRefusesWhatItCannotBuild(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	before := keysOf(readQUICState(t, c).snap.GetResources(resourcev3.ClusterType))

	for _, tc := range []struct {
		name, want string
	}{
		{proxy.QUICClusterName("demo/other", c.meshDomain, "demo/source-a"), QUICRefusedNotInDependency},
		{echoTwin(c, "stranger"), QUICRefusedSourceNotOnNode},
		{proxy.QUICClusterName("demo/echo", c.meshDomain, "other-ns/source-a"), QUICRefusedSourceNotOnNode},
		{"quic:" + proxy.ServiceClusterName("demo/echo", c.meshDomain) + ":8080@demo/source-a", QUICRefusedMalformed},
		{"quic:echo.demo.example.com@demo/source-a", QUICRefusedMalformed},
		{"quic:" + proxy.ServiceClusterName("demo/echo", c.meshDomain) + "@demo/source-a/x", QUICRefusedMalformed},
		{"quic:" + proxy.ServiceClusterName("demo/echo", c.meshDomain), QUICRefusedMalformed},
	} {
		decision, reason := c.ObserveQUICTwin(ctx, testQUICStream, tc.name)
		assert.Equal(t, QUICTwinRefused, decision, tc.name)
		assert.Equal(t, tc.want, reason, tc.name)
	}
	assert.Empty(t, c.QUICPairs(), "a refused name must record nothing")
	require.NoError(t, c.generateSnapshot(ctx))
	st := readQUICState(t, c)
	assert.Empty(t, st.twins)
	assert.ElementsMatch(t, before, keysOf(st.snap.GetResources(resourcev3.ClusterType)), "a refusal must not change CDS")
}

// (f) The #1033 migration: a PERSISTED pair that has no on-demand fetch within
// the window after the agent starts is pruned with its twin, one log line per
// node; a persisted pair that IS fetched in the window is kept. This is what
// drains the SAs x destinations fan-out the first #1032 deploy persisted on
// every talos node (observed_pairs == local_identities x 2).
//
// Red before #1033: no such prune existed, and both pairs survived for as
// long as their source had a pod on the node.
func TestQUICDemandPrunesPersistedPairsNotFetchedWithinTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b", "demo/source-c")
	c.FlushObservedUpstreams()
	require.Len(t, readStore(t, path).GetQuicPairs(), 3)

	restarted := newQUICDemandCache(t, path)
	ctx := context.Background()
	twinA, twinB, twinC := echoTwin(restarted, "source-a"), echoTwin(restarted, "source-b"), echoTwin(restarted, "source-c")
	all := []string{twinA, twinB, twinC}
	require.ElementsMatch(t, all, readQUICState(t, restarted).twins, "every restored twin is served from the first snapshot")

	// source-b's twin is fetched on demand in this process; source-c's is
	// re-subscribed on the proxy's fresh stream (a live on-demand
	// subscription: pruning it would strand it); source-a's is neither.
	decision, _ := restarted.ObserveQUICTwin(ctx, testQUICStream, twinB)
	require.Equal(t, QUICTwinKnown, decision)
	require.Zero(t, restarted.ResumeQUICSubscriptions(ctx, testQUICStream, []string{twinC}), "already restored: nothing new")

	window := DefaultQUICPairFetchWindow
	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(window - time.Second))
	assert.ElementsMatch(t, all, restarted.QUICPairs(), "inside the window nothing is pruned")

	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(window))
	assert.Equal(t, []string{twinB, twinC}, restarted.QUICPairs(), "the unfetched persisted pair is pruned; the fetched and subscribed ones kept")
	require.NoError(t, restarted.generateSnapshot(ctx))
	st := readQUICState(t, restarted)
	assert.ElementsMatch(t, []string{twinB, twinC}, st.twins, "the pruned pair's twin leaves CDS")
	assert.ElementsMatch(t, []string{twinB, twinC}, st.twinCLAs)
	requireSelections(t, restarted, st, allArms(restarted, quicDemandSAs...))
	restarted.FlushObservedUpstreams()
	stored := readStore(t, path).GetQuicPairs()
	require.Len(t, stored, 2, "the pruned pair leaves the persisted set")
	assert.Equal(t, "demo/source-b", stored[0].GetSource())
	assert.Equal(t, "demo/source-c", stored[1].GetSource())

	// A pruned pair that still has traffic is re-fetched: real first use.
	decision, _ = restarted.ObserveQUICTwin(ctx, testQUICStream, twinA)
	assert.Equal(t, QUICTwinAdded, decision)

	// Pairs first used in this process are never pruned by the window.
	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(10 * window))
	assert.ElementsMatch(t, all, restarted.QUICPairs())
}

// A zero window disables the prune.
func TestQUICDemandFetchWindowZeroDisablesThePrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	observeQUIC(t, c, "demo/echo", "demo/source-a")
	c.FlushObservedUpstreams()

	restarted := newQUICDemandCache(t, path)
	restarted.SetQUICPairFetchWindow(0)
	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(24 * time.Hour))
	assert.Equal(t, []string{echoTwin(restarted, "source-a")}, restarted.QUICPairs())
}
