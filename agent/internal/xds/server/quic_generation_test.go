package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1052 gate, at the observer: after an agent restart that lands mid proxy
// hot restart, two proxy generations reconnect and each re-states its
// subscriptions on its own fresh stream, in either order. Each subtest runs the
// agent's real observer callbacks against a real snapshot cache that reloads
// its dormant pairs from the persisted ledger, then ends the old generation's
// stream and restarts the agent once more on the same store: no dormant pair
// may survive that no live generation holds a subscription for.

// newGenerationNode is a node with two local ServiceAccounts, "demo/echo" in
// its dependency set (so QUIC-eligible, #979), persisting its observed set at path (enabled before any pod
// lands, as the agent does at boot).
func newGenerationNode(ctx context.Context, t *testing.T, path string) *cache.SnapshotCache {
	t.Helper()
	c := cache.NewSnapshotCache(restateNode, slog.New(slog.DiscardHandler))
	c.RestoreDependency(ctx, "demo/echo")
	c.EnableObservedUpstreamsStore(ctx, path)
	t.Cleanup(c.FlushObservedUpstreams)
	for i := range 2 {
		sa := fmt.Sprintf("sa-%02d", i)
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: sa + "-0", Namespace: "demo", ServiceAccount: sa,
			NetworkNamespace: "/var/run/netns/cni-" + sa,
		}, restateDomain))
	}
	return c
}

func firstCDS(names ...string) *discoveryv3.DeltaDiscoveryRequest {
	return &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: append([]string{"*"}, names...),
	}
}

func TestOnDemandObserver_OverlappingProxyGenerationsAfterAgentRestart(t *testing.T) {
	awayA := proxy.QUICClusterName("demo/echo", restateDomain, "demo/away-a")
	awayB := proxy.QUICClusterName("demo/echo", restateDomain, "demo/away-b")
	const (
		oldGen int64 = 1 // the proxy generation before the hot restart
		child  int64 = 2 // the new generation's stream to the restarted agent
		parent int64 = 3 // the old generation reconnecting to the restarted agent
	)
	node := &corev3.Node{Id: restateNode}

	for _, tc := range []struct {
		name  string
		order []int64 // which fresh stream re-states first
	}{
		{name: "new generation first (rev248 node C)", order: []int64{child, parent}},
		{name: "old generation first", order: []int64{parent, child}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			path := filepath.Join(t.TempDir(), cache.ObservedUpstreamsFile)

			// Agent 1: the old generation asks for two twins whose sources are
			// not on the node. Refused (503), subscriptions open: dormant.
			c1 := newGenerationNode(ctx, t, path)
			cb1 := newOnDemandObserver(c1, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks()
			require.NoError(t, cb1.OnStreamDeltaRequest(oldGen, firstCDS()))
			require.NoError(t, cb1.OnStreamDeltaRequest(oldGen, &discoveryv3.DeltaDiscoveryRequest{
				TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{awayA, awayB},
			}))
			require.Equal(t, []string{awayA, awayB}, c1.DormantQUICPairs())
			c1.FlushObservedUpstreams()

			// Agent 2 restarts on the persisted ledger, mid hot restart: the
			// child re-subscribes nothing, the parent re-subscribes awayA.
			c2 := newGenerationNode(ctx, t, path)
			require.Equal(t, []string{awayA, awayB}, c2.DormantQUICPairs(), "precondition: reloaded from the persisted ledger")
			cb2 := newOnDemandObserver(c2, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks()
			restate := map[int64]*discoveryv3.DeltaDiscoveryRequest{child: firstCDS(), parent: firstCDS(awayA)}
			for _, s := range tc.order {
				require.NoError(t, cb2.OnStreamDeltaRequest(s, restate[s]))
			}
			assert.Equal(t, []string{awayA}, c2.DormantQUICPairs(),
				"while both generations are live, awayA is held by the parent that re-subscribed it; awayB by nobody")
			assert.Empty(t, c2.QUICPairs())

			// The parent drains and exits: its stream ends while the child's is
			// live. Its holdings go with it.
			cb2.OnDeltaStreamClosed(parent, node)
			assert.Empty(t, c2.DormantQUICPairs(),
				"no live generation subscribes to awayA: pruned, not left as a phantom dormant pair (issue #1052)")
			c2.FlushObservedUpstreams()

			// And the next agent restart reloads no phantom either.
			c3 := newGenerationNode(ctx, t, path)
			assert.Empty(t, c3.DormantQUICPairs(), "the persisted ledger carries no dormant pair without a live subscription")
		})
	}
}

// A later first use on the child is the child's own subscription: the pair
// survives the parent's exit.
func TestOnDemandObserver_ChildFirstUseSurvivesParentExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := newGenerationNode(ctx, t, filepath.Join(t.TempDir(), cache.ObservedUpstreamsFile))
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))
	away := proxy.QUICClusterName("demo/echo", restateDomain, "demo/away")
	const parent, child int64 = 1, 2

	cb := o.Callbacks()
	require.NoError(t, cb.OnStreamDeltaRequest(parent, firstCDS(away)))
	require.NoError(t, cb.OnStreamDeltaRequest(child, firstCDS()))
	require.NoError(t, cb.OnStreamDeltaRequest(child, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{away},
	}))
	cb.OnDeltaStreamClosed(parent, &corev3.Node{Id: restateNode})
	assert.Equal(t, []string{away}, c.DormantQUICPairs(), "the child subscribed to it itself: still dormant")
}
