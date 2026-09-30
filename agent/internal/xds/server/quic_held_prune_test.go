package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	agentv1 "aethermesh.dev/api/aether/agent/v1"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// The #1073 gate, at the server: an agent-only restart while the proxy keeps
// running. The proxy's fresh delta stream re-states the twins it uses as HELD
// (initial_resource_versions, no on-demand subscription: after its own last
// restart they reached it through the wildcard, and a held twin is never
// fetched again). One --east-west-quic-pair-fetch-window later the pre-#1073
// agent pruned every one of them -- ~200 x 503 NC per burst on the #979
// proving soak, and an Envoy SIGBUS on one node (#1074).
//
// This drives the agent's real observer and real snapshot cache through
// go-control-plane's delta server, over a demand set persisted by an agent
// before #1073 (no demand_confirmed bit).

// stripConfirmation rewrites the persisted set at path as a pre-#1073 agent
// wrote it.
func stripConfirmation(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	stored := &agentv1.ObservedUpstreams{}
	require.NoError(t, protojson.Unmarshal(data, stored))
	for _, p := range stored.GetQuicPairs() {
		p.SetDemandConfirmed(false)
	}
	data, err = protojson.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestOnDemandObserver_AgentRestartKeepsHeldTwinsPastTheFetchWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	path := filepath.Join(t.TempDir(), cache.ObservedUpstreamsFile)
	heldA := proxy.QUICClusterName("demo/echo", restateDomain, "demo/sa-00")
	heldB := proxy.QUICClusterName("demo/echo", restateDomain, "demo/sa-01")
	unused := proxy.QUICClusterName("demo/echo", restateDomain, "demo/sa-02")

	// Agent 1: three pairs first used on demand, persisted.
	c1 := newHeldNode(ctx, t, path)
	cb1 := newOnDemandObserver(c1, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks()
	require.NoError(t, cb1.OnStreamDeltaRequest(1, firstCDS()))
	require.NoError(t, cb1.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{heldA, heldB, unused},
	}))
	require.ElementsMatch(t, []string{heldA, heldB, unused}, c1.QUICPairs())
	c1.FlushObservedUpstreams()
	stripConfirmation(t, path)

	// Agent 2 on the persisted set; the running proxy reconnects on a fresh
	// stream and re-states heldA and heldB as held (a proxy restart since
	// agent 1 moved them onto the wildcard). It holds nothing for `unused`.
	c2 := newHeldNode(ctx, t, path)
	require.ElementsMatch(t, []string{heldA, heldB, unused}, c2.QUICPairs(), "precondition: restored")
	requireTwinsServed(t, c2, heldA, heldB, unused)
	o := newOnDemandObserver(c2, &mockRegistry{}, slog.New(slog.DiscardHandler))
	stream := deltaStream(ctx, t, serverv3.NewServer(ctx, c2, o.Callbacks()))
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node:                    &corev3.Node{Id: restateNode},
		TypeUrl:                 resourcev3.ClusterType,
		ResourceNamesSubscribe:  []string{"*"},
		InitialResourceVersions: versions([]string{heldA, heldB}),
	}))
	resp := recvDelta(t, stream)
	for _, name := range []string{heldA, heldB} {
		assert.NotContains(t, resp.GetRemovedResources(), name, "a held twin the agent serves is never answered absent")
	}

	// Past the fetch window.
	c2.SetQUICPairFetchWindow(time.Nanosecond)
	time.Sleep(time.Millisecond)
	c2.PruneUnfetchedQUICPairs()
	assert.ElementsMatch(t, []string{heldA, heldB}, c2.QUICPairs(),
		"zero held twins pruned; only the pair the proxy neither holds nor fetched goes")

	c2.FlushObservedUpstreams()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	stored := &agentv1.ObservedUpstreams{}
	require.NoError(t, protojson.Unmarshal(data, stored))
	require.Len(t, stored.GetQuicPairs(), 2)
	for _, p := range stored.GetQuicPairs() {
		assert.True(t, p.GetDemandConfirmed(), "held pair confirmed in the store: %s", p.GetSource())
	}

	// Agent 3 after a node reboot: the fresh proxy holds nothing and fetches
	// nothing (its twins arrive through the wildcard). Still nothing pruned.
	c3 := newHeldNode(ctx, t, path)
	cb3 := newOnDemandObserver(c3, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks()
	require.NoError(t, cb3.OnStreamDeltaRequest(1, firstCDS()))
	c3.SetQUICPairFetchWindow(time.Nanosecond)
	time.Sleep(time.Millisecond)
	c3.PruneUnfetchedQUICPairs()
	got := c3.QUICPairs()
	slices.Sort(got)
	assert.Equal(t, []string{heldA, heldB}, got)
}

// newHeldNode is a node with three local ServiceAccounts (demo/sa-00..02) and
// demo/echo in its dependency set with an endpoint in the registry, persisting
// its observed set at path.
func newHeldNode(ctx context.Context, t *testing.T, path string) *cache.SnapshotCache {
	t.Helper()
	c := cache.NewSnapshotCache(restateNode, slog.New(slog.DiscardHandler))
	c.RestoreDependency(ctx, "demo/echo")
	c.EnableObservedUpstreamsStore(ctx, path)
	t.Cleanup(c.FlushObservedUpstreams)
	for _, sa := range []string{"sa-00", "sa-01", "sa-02"} {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: sa + "-0", Namespace: "demo", ServiceAccount: sa,
			NetworkNamespace: "/var/run/netns/cni-" + sa,
		}, restateDomain))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, firstUseNodeSVID))
	reg := &mockRegistry{listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		if protocol != registryv1.Service_PROTOCOL_HTTP {
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		}
		return map[string][]*registryv1.ServiceEndpoint{"demo/echo": {{
			Ip: "10.0.3.1", ClusterName: "cluster-1", Port: 8080, Weight: 100,
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "demo", PodName: "echo-0", NodeName: "node-2"},
		}}}, nil
	}}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", restateNode, reg))
	return c
}

// requireTwinsServed waits for the node's snapshot to carry every twin.
func requireTwinsServed(t *testing.T, c *cache.SnapshotCache, twins ...string) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap, err := c.GetSnapshot(restateNode)
		if err != nil {
			return false
		}
		clusters := snap.GetResources(resourcev3.ClusterType)
		for _, twin := range twins {
			if _, ok := clusters[twin]; !ok {
				return false
			}
		}
		return true
	}, 5*time.Second, 5*time.Millisecond, "the restored twins are served")
}
