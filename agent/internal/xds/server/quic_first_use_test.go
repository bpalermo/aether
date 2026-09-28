package server

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1049 gates, at the server: the agent's real observer and snapshot cache
// behind go-control-plane's delta server, over an in-memory gRPC connection.

const firstUseNodeSVID = "spiffe://aether.internal/ns/aether-system/sa/aether-agent"

// newFirstUseNode is a node with one local ServiceAccount (demo/source-a) and
// one QUIC-enabled destination (demo/echo) loaded from the registry. It returns
// source-a's twin name and SPIFFE ID.
func newFirstUseNode(ctx context.Context, t *testing.T) (*cache.SnapshotCache, string, string) {
	t.Helper()
	c := cache.NewSnapshotCache(restateNode, slog.New(slog.DiscardHandler))
	c.SetEastWestQUICServices([]string{"demo/echo"})
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "source-a-0", Namespace: "demo", ServiceAccount: "source-a",
		NetworkNamespace: "/var/run/netns/cni-source-a",
	}, restateDomain))
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
	return c, proxy.QUICClusterName("demo/echo", restateDomain, "demo/source-a"), "spiffe://" + restateDomain + "/ns/demo/sa/source-a"
}

func secretsNamed(names ...string) []*tlsv3.Secret {
	out := make([]*tlsv3.Secret, 0, len(names))
	for _, n := range names {
		out = append(out, &tlsv3.Secret{Name: n})
	}
	return out
}

// pump reads every delta response of stream into a channel, so a wait that
// times out never leaves a reader behind to swallow the next response.
func pump(stream discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesClient) <-chan *discoveryv3.DeltaDiscoveryResponse {
	ch := make(chan *discoveryv3.DeltaDiscoveryResponse, 16)
	go func() {
		defer close(ch)
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			ch <- resp
		}
	}()
	return ch
}

// recvWithin returns the next delta response, or nil if none arrives within d.
func recvWithin(responses <-chan *discoveryv3.DeltaDiscoveryResponse, d time.Duration) *discoveryv3.DeltaDiscoveryResponse {
	select {
	case r := <-responses:
		return r
	case <-time.After(d):
		return nil
	}
}

func resourceNames(resp *discoveryv3.DeltaDiscoveryResponse) []string {
	var out []string
	for _, r := range resp.GetResources() {
		out = append(out, r.GetName())
	}
	return out
}

// TestOnDemandSubscribeForATwinTheWildcardAlreadySentIsAnswered is the
// main-worker-03 "not found during on-demand discovery" at 15:45:04.7. The
// twin reached the proxy through the wildcard CDS subscription first; a
// request then routed to it while it was not yet an active cluster, and
// Envoy's ODCDS manager subscribed to the name on the same stream.
// go-control-plane answers a named subscribe only on a version change, so the
// subscription heard nothing, its 15 s initial-fetch timeout fired and Envoy
// reported the twin missing -- an absent answer for a twin the agent was
// serving, which fails every request still waiting on the name.
//
// Want: the subscribe itself is answered, with the twin.
// Red without CreateDeltaWatch's re-send: no response at all.
func TestOnDemandSubscribeForATwinTheWildcardAlreadySentIsAnswered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, twin, sourceID := newFirstUseNode(ctx, t)
	require.NoError(t, c.SetSecrets(ctx, secretsNamed(firstUseNodeSVID, sourceID)))
	decision, reason := c.ObserveQUICTwin(ctx, twin)
	require.Equal(t, cache.QUICTwinAdded, decision, reason)
	require.Eventually(t, func() bool {
		snap, err := c.GetSnapshot(restateNode)
		if err != nil {
			return false
		}
		_, ok := snap.GetResources(resourcev3.ClusterType)[twin]
		return ok
	}, 5*time.Second, 5*time.Millisecond)

	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))
	stream := deltaStream(ctx, t, serverv3.NewServer(ctx, c, o.Callbacks()))
	node := &corev3.Node{Id: restateNode}
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node: node, TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{"*"},
	}))
	responses := pump(stream)
	first := recvWithin(responses, 10*time.Second)
	require.NotNil(t, first)
	require.Contains(t, resourceNames(first), twin, "precondition: the wildcard delivered the twin")

	start := time.Now()
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node: node, TypeUrl: resourcev3.ClusterType, ResponseNonce: first.GetNonce(),
		ResourceNamesSubscribe: []string{twin},
	}))
	resp := recvWithin(responses, 2*time.Second)
	require.NotNil(t, resp, "the on-demand subscribe must be answered; silence ends in Envoy's 15 s initial-fetch timeout and a 'not found' (issue #1049)")
	t.Logf("on-demand subscribe for a twin already sent via the wildcard answered in %s", time.Since(start))
	assert.Equal(t, []string{twin}, resourceNames(resp), "answered with the twin, and only the twin")
	assert.Empty(t, resp.GetRemovedResources())
}

// TestOnDemandTwinWaitingForItsCertificateIsHeldNeverAbsent: a first-use
// request for a pair whose source's certificate SPIRE has not delivered yet
// (the new k6 pod on every node at 15:44:46). The pair is admitted and the
// subscription is HELD -- no response, and never removed_resources -- until
// the certificate lands, then answered with the twin with no further request.
func TestOnDemandTwinWaitingForItsCertificateIsHeldNeverAbsent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, twin, sourceID := newFirstUseNode(ctx, t)
	// SPIRE is serving the node, but not source-a's SVID yet.
	require.NoError(t, c.SetSecrets(ctx, secretsNamed(firstUseNodeSVID)))

	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))
	stream := deltaStream(ctx, t, serverv3.NewServer(ctx, c, o.Callbacks()))
	node := &corev3.Node{Id: restateNode}
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node: node, TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{"*"},
	}))
	responses := pump(stream)
	first := recvWithin(responses, 10*time.Second)
	require.NotNil(t, first)
	require.NotContains(t, resourceNames(first), twin)

	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node: node, TypeUrl: resourcev3.ClusterType, ResponseNonce: first.GetNonce(),
		ResourceNamesSubscribe: []string{twin},
	}))
	require.Eventually(t, func() bool { return len(c.QUICPairs()) == 1 }, 5*time.Second, 5*time.Millisecond, "first use admits the pair")
	if held := recvWithin(responses, 300*time.Millisecond); held != nil {
		assert.NotContains(t, held.GetRemovedResources(), twin, "a subscribed twin of a QUIC-enabled destination is never answered absent")
		assert.NotContains(t, resourceNames(held), twin, "no twin before its certificate")
		require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
			Node: node, TypeUrl: resourcev3.ClusterType, ResponseNonce: held.GetNonce(),
		}))
	}

	require.NoError(t, c.SetSecrets(ctx, secretsNamed(firstUseNodeSVID, sourceID)))
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := recvWithin(responses, time.Until(deadline))
		require.NotNil(t, resp, "the certificate's snapshot must answer the held subscription")
		assert.NotContains(t, resp.GetRemovedResources(), twin)
		if slices.Contains(resourceNames(resp), twin) {
			return
		}
		require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
			Node: node, TypeUrl: resourcev3.ClusterType, ResponseNonce: resp.GetNonce(),
		}))
	}
}
