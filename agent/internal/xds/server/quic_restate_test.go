package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// The #1033 gates, at the server: an agent restart is a fresh delta stream on
// which the running Envoy re-states the twins it has. The first talos deploy of
// #1032 (rev245, 2026-09-28) admitted each one as a pair --
// `quic_clusters=24 observed_pairs=24 local_identities=12` on a node whose
// previous agent had built the SAs x destinations twins up front -- and
// persisted them.
//
// These drive the agent's real observer and real snapshot cache through
// go-control-plane's delta server over an in-memory gRPC connection: 12 local
// ServiceAccounts x 2 QUIC-eligible destinations = 24 twins, and an EMPTY
// persisted demand set.

const (
	restateNode   = "node-1"
	restateDomain = "aether.internal"
)

// newRestateNode is a node with 12 local ServiceAccounts and 2 destinations in
// its dependency set (so QUIC-eligible, #979), nothing observed; it returns the 24 twin names.
func newRestateNode(ctx context.Context, t *testing.T) (*cache.SnapshotCache, []string) {
	t.Helper()
	c := cache.NewSnapshotCache(restateNode, slog.New(slog.DiscardHandler))
	destinations := []string{"demo/echo", "demo/other"}
	for _, dst := range destinations {
		c.RestoreDependency(ctx, dst)
	}
	var twins []string
	for i := range 12 {
		sa := fmt.Sprintf("sa-%02d", i)
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: sa + "-0", Namespace: "demo", ServiceAccount: sa,
			NetworkNamespace: "/var/run/netns/cni-" + sa,
		}, restateDomain))
		for _, dst := range destinations {
			twins = append(twins, proxy.QUICClusterName(dst, restateDomain, "demo/"+sa))
		}
	}
	require.Len(t, twins, 24)
	require.Empty(t, c.QUICPairs(), "precondition: nothing persisted, nothing observed")
	return c, twins
}

func versions(names []string) map[string]string {
	held := make(map[string]string, len(names))
	for _, name := range names {
		held[name] = "v-previous-agent"
	}
	return held
}

// TestOnDemandObserver_FreshStreamHeldTwinsAdmitNothing is the talos shape:
// the proxy HOLDS 24 twins an older agent built up front and delivered through
// the wildcard (initial_resource_versions; no on-demand subscription behind
// any of them). Want 0 pairs admitted and all 24 answered absent in the first
// response's removed_resources, so the proxy drops them; then a later
// on-demand request -- a request routing to one twin -- admits exactly that
// pair.
//
// Red on #1032's observer: 24 admitted (RestoreQUICTwin on the held map).
func TestOnDemandObserver_FreshStreamHeldTwinsAdmitNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, twins := newRestateNode(ctx, t)
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))
	stream := deltaStream(ctx, t, serverv3.NewServer(ctx, c, o.Callbacks()))

	node := &corev3.Node{Id: restateNode}
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node:                    node,
		TypeUrl:                 resourcev3.ClusterType,
		ResourceNamesSubscribe:  []string{"*"},
		InitialResourceVersions: versions(twins),
	}))
	resp := recvDelta(t, stream)

	admitted := c.QUICPairs()
	t.Logf("fresh stream: proxy holds %d twins; admitted=%d answered_absent=%d", len(twins), len(admitted), len(resp.GetRemovedResources()))
	assert.Empty(t, admitted, "a held twin is not demand: no pair may be admitted (issue #1033)")
	assert.ElementsMatch(t, twins, resp.GetRemovedResources(), "every held twin the agent does not serve must be answered absent")
	for _, r := range resp.GetResources() {
		assert.False(t, proxy.IsQUICClusterName(r.GetName()), "no twin may be served: %s", r.GetName())
	}

	// A LATER request naming one twin is the on_demand filter: real first use.
	first := twins[3]
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node:                   node,
		TypeUrl:                resourcev3.ClusterType,
		ResponseNonce:          resp.GetNonce(),
		ResourceNamesSubscribe: []string{first},
	}))
	require.Eventually(t, func() bool { return len(c.QUICPairs()) > 0 }, 5*time.Second, 10*time.Millisecond,
		"an on-demand request after the stream's first must admit its pair")
	assert.Equal(t, []string{first}, c.QUICPairs(), "exactly the requested pair")
}

// TestOnDemandObserver_FreshStreamResubscribedTwinsAreServed: a twin the proxy
// RE-SUBSCRIBES on the fresh stream holds a live on-demand subscription, opened
// by a request that routed to it, and Envoy never re-sends it (its ODCDS
// manager answers every later request for the name "already subscribed,
// skipping"). Answering it absent would strand the pair -- every request 503s
// at the on_demand timeout; //test/mtlspool shows it -- so a valid pair is
// admitted. Held-only twins in the same request still admit nothing.
func TestOnDemandObserver_FreshStreamResubscribedTwinsAreServed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, twins := newRestateNode(ctx, t)
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))

	subscribed := twins[:3]
	stranger := proxy.QUICClusterName("demo/echo", restateDomain, "demo/not-on-this-node")
	require.NoError(t, o.Callbacks().OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                 resourcev3.ClusterType,
		ResourceNamesSubscribe:  append([]string{"*", stranger}, subscribed...),
		InitialResourceVersions: versions(append(slices.Clone(twins), stranger)),
	}))
	assert.ElementsMatch(t, subscribed, c.QUICPairs(),
		"the re-subscribed twins' pairs are served; the held-only twins' and the non-local source's are not")
}

// deltaStream serves srv over an in-memory gRPC connection and opens one delta
// ADS stream against it.
func deltaStream(ctx context.Context, t *testing.T, srv serverv3.Server) discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).DeltaAggregatedResources(ctx)
	require.NoError(t, err)
	return stream
}

func recvDelta(t *testing.T, stream discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesClient) *discoveryv3.DeltaDiscoveryResponse {
	t.Helper()
	type result struct {
		resp *discoveryv3.DeltaDiscoveryResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := stream.Recv()
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.resp
	case <-time.After(10 * time.Second):
		t.Fatal("no delta response within 10s")
		return nil
	}
}

// TestOnDemandObserver_FreshStreamRestatesSubscriptionsForDormantPairs (issue
// #1036): a dormant pair -- one whose twin the proxy subscribed to but the
// agent cannot serve right now -- is kept only while the proxy holds the
// subscription, and the agent learns that on every fresh stream: one that
// re-subscribes the twin keeps it; one that does not (a hot-restart child
// holds no ODCDS subscriptions) prunes it. The observer must hand the cache
// every fresh stream's re-statement, including an empty one.
func TestOnDemandObserver_FreshStreamRestatesSubscriptionsForDormantPairs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, _ := newRestateNode(ctx, t)
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))
	cb := o.Callbacks()
	away := proxy.QUICClusterName("demo/echo", restateDomain, "demo/away")

	// Stream 1: its first CDS request, then a first use for a source that is
	// not on the node. Refused (503), but Envoy keeps the subscription open.
	require.NoError(t, cb.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{"*"},
	}))
	require.NoError(t, cb.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{away},
	}))
	require.Equal(t, []string{away}, c.DormantQUICPairs(), "a subscribed, unservable twin is kept dormant")

	// Stream 2 (agent restart, same proxy): re-subscribes it. Kept.
	require.NoError(t, cb.OnStreamDeltaRequest(2, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{"*", away},
	}))
	assert.Equal(t, []string{away}, c.DormantQUICPairs(), "the proxy still subscribes to it: kept")
	assert.Empty(t, c.QUICPairs())

	// Stream 3 (a new proxy generation): names nothing. Pruned.
	require.NoError(t, cb.OnStreamDeltaRequest(3, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{"*"},
	}))
	assert.Empty(t, c.DormantQUICPairs(), "a fresh stream that does not subscribe to it: pruned")
}
