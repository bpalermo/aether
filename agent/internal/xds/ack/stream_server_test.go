package ack

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// The tests in stream_test.go drive the tracker's callbacks by hand, so they
// take on trust the order go-control-plane calls them in and what it puts in
// a response. These run the tracker behind the real go-control-plane delta
// server and snapshot cache, with real per-resource versions, and a client
// that plays the proxy. They fail on a go-control-plane bump that changes how
// a stream is announced, how the opening request is answered, or when a
// response is written relative to its callback.

const serverNodeID = "node"

// testServerListener is a listener whose bytes, and so whose version, depend
// on port.
func testServerListener(name string, port uint32) *listenerv3.Listener {
	return &listenerv3.Listener{
		Name: name,
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address:       "127.0.0.1",
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		}}},
	}
}

// deltaServer is the tracker behind a go-control-plane ADS server.
type deltaServer struct {
	tracker *Tracker
	cache   cachev3.SnapshotCache
	conn    *grpc.ClientConn
	// versions is the per-listener version of the last snapshot set.
	versions map[string]string
}

// startDeltaServer serves the listeners as snapshot v1.
func startDeltaServer(t *testing.T, listeners ...types.Resource) *deltaServer {
	t.Helper()
	s := newDeltaServer(t)
	s.publish(t, "v1", listeners...)
	return s
}

// newDeltaServer serves no snapshot yet: a request is held until publish.
func newDeltaServer(t *testing.T) *deltaServer {
	t.Helper()
	s := &deltaServer{
		tracker: NewTracker(slog.New(slog.DiscardHandler)),
		cache:   cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil),
	}
	// As the node agent wires it: present is "at the version this cache serves".
	s.tracker.SetPublishedVersion(SnapshotVersions(s.cache, serverNodeID))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), s.cache, s.tracker.Callbacks()))
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	s.conn = conn
	return s
}

// publish sets the snapshot and records the versions the cache will compare a
// proxy's statements with.
func (s *deltaServer) publish(t *testing.T, version string, listeners ...types.Resource) {
	t.Helper()
	snapshot, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]types.Resource{resourcev3.ListenerType: listeners})
	require.NoError(t, err)
	require.NoError(t, snapshot.ConstructVersionMap())
	s.versions = snapshot.GetVersionMap(resourcev3.ListenerType)
	require.NoError(t, s.cache.SetSnapshot(context.Background(), serverNodeID, snapshot))
}

// settled waits until the server has read every answer the proxy sent.
func (s *deltaServer) settled(t *testing.T) {
	t.Helper()
	s.leftUnanswered(t, 0)
}

// leftUnanswered waits until exactly n responses are unanswered.
func (s *deltaServer) leftUnanswered(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(unanswered(s.tracker)) == n }, resolvedWait, time.Millisecond,
		"the server never read the proxy's answer")
}

// connected waits until the tracker keeps n streams.
func (s *deltaServer) connected(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return proxies(s.tracker) == n }, resolvedWait, time.Millisecond,
		"the tracker never saw %d streams", n)
}

// proxyStream is one delta ADS stream of the pretend proxy.
type proxyStream struct {
	t      *testing.T
	stream discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesClient
	cancel context.CancelFunc
}

// dial starts an ADS stream and sends nothing on it.
func (s *deltaServer) dial(t *testing.T) *proxyStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(s.conn).DeltaAggregatedResources(ctx)
	require.NoError(t, err)
	return &proxyStream{t: t, stream: stream, cancel: cancel}
}

// open starts a stream and sends the opening Listener request, stating the
// given versions.
func (s *deltaServer) open(t *testing.T, stated map[string]string) *proxyStream {
	t.Helper()
	p := s.dial(t)
	p.request(resourcev3.ListenerType, stated)
	return p
}

// request sends the opening request of a type.
func (p *proxyStream) request(typeURL string, stated map[string]string) {
	p.t.Helper()
	require.NoError(p.t, p.stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node:                    &corev3.Node{Id: serverNodeID},
		TypeUrl:                 typeURL,
		InitialResourceVersions: stated,
	}))
}

// recv returns the next response and the names it adds. The tracker's
// response callback has run by the time the response is on the wire.
func (p *proxyStream) recv() (*discoveryv3.DeltaDiscoveryResponse, []string) {
	p.t.Helper()
	type result struct {
		resp *discoveryv3.DeltaDiscoveryResponse
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := p.stream.Recv()
		got <- result{resp, err}
	}()
	select {
	case r := <-got:
		require.NoError(p.t, r.err)
		var added []string
		for _, res := range r.resp.GetResources() {
			added = append(added, res.GetName())
		}
		return r.resp, added
	case <-time.After(resolvedWait):
		require.FailNow(p.t, "no delta response")
		return nil, nil
	}
}

func (p *proxyStream) ack(resp *discoveryv3.DeltaDiscoveryResponse) {
	p.t.Helper()
	require.NoError(p.t, p.stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resp.GetTypeUrl(), ResponseNonce: resp.GetNonce(),
	}))
}

func (p *proxyStream) nack(resp *discoveryv3.DeltaDiscoveryResponse, msg string) {
	p.t.Helper()
	require.NoError(p.t, p.stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resp.GetTypeUrl(), ResponseNonce: resp.GetNonce(),
		ErrorDetail: status.New(codes.InvalidArgument, msg).Proto(),
	}))
}

// TestServer_StatedAtThePublishedVersionAnswersTheWait is #1511 on the real
// server: the proxy states both listeners at the versions the snapshot
// publishes, go-control-plane answers with an empty response, and the waits
// are answered from the statement.
func TestServer_StatedAtThePublishedVersionAnswersTheWait(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))
	requireNotPresent(t, s.tracker, testListener, "no proxy is connected")

	p := s.open(t, map[string]string{
		testListener:  s.versions[testListener],
		otherListener: s.versions[otherListener],
	})
	resp, added := p.recv()
	require.Empty(t, added, "go-control-plane sends nothing for a listener stated at the published version")
	require.Empty(t, resp.GetRemovedResources())
	requirePresent(t, s.tracker, testListener)
	requirePresent(t, s.tracker, otherListener)
	requireAbsent(t, s.tracker, listenerOf("no-such-pod"), "the proxy said what it holds, and this is not in it")
}

// TestServer_StatedAtAnotherVersionWaitsForItsOwnAnswer: a listener the proxy
// states at a version that is not the published one, whether an older one, a
// newer one (the agent rolled back) or an empty one, is sent again, and its
// wait is answered by nothing but the proxy's answer to that response. The
// listener stated at the published version next to it is held all along.
func TestServer_StatedAtAnotherVersionWaitsForItsOwnAnswer(t *testing.T) {
	// A real version of the same listener with other bytes: what a proxy that
	// last heard from an agent publishing a different config states.
	otherVersion := startDeltaServer(t, testServerListener(testListener, 9)).versions[testListener]

	for name, statedVersion := range map[string]string{
		"a version of other bytes": otherVersion,
		"an empty version":         "",
		"an arbitrary version":     "not-a-version",
	} {
		t.Run(name, func(t *testing.T) {
			s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))
			require.NotEqual(t, s.versions[testListener], statedVersion)

			p := s.open(t, map[string]string{
				testListener:  statedVersion,
				otherListener: s.versions[otherListener],
			})
			resp, added := p.recv()
			require.Equal(t, []string{testListener}, added, "the listener stated at another version is sent, the other is not")
			requireNotPresent(t, s.tracker, testListener, "the proxy stated another version and has not answered the published one")
			requirePresent(t, s.tracker, otherListener)

			p.nack(resp, "Permission denied")
			s.settled(t)
			requireRejected(t, s.tracker, testListener, "Permission denied")
			requirePresent(t, s.tracker, otherListener, "the rejected response did not carry it")
		})
	}

	t.Run("accepted", func(t *testing.T) {
		s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))
		p := s.open(t, map[string]string{
			testListener:  otherVersion,
			otherListener: s.versions[otherListener],
		})
		resp, added := p.recv()
		require.Equal(t, []string{testListener}, added)
		p.ack(resp)
		requirePresent(t, s.tracker, testListener, "the proxy acknowledged the published version")
	})
}

// TestServer_StatedButNotPublishedIsRemoved: a listener the proxy states that
// the snapshot does not have is removed by the opening response. The proxy
// holds it until it acknowledges that: this is a pod deleted while the agent
// was down, or just before the proxy reconnected (#1572).
func TestServer_StatedButNotPublishedIsRemoved(t *testing.T) {
	s := startDeltaServer(t, testServerListener(otherListener, 2))
	requireNotAbsent(t, s.tracker, testListener, "no proxy is connected")

	p := s.open(t, map[string]string{
		testListener:  "anything",
		otherListener: s.versions[otherListener],
	})
	resp, added := p.recv()
	require.Empty(t, added)
	require.Equal(t, []string{testListener}, resp.GetRemovedResources())
	requireNotAbsent(t, s.tracker, testListener, "the removal is not acknowledged")

	p.ack(resp)
	requireAbsent(t, s.tracker, testListener)
	requireNotPresent(t, s.tracker, testListener)
	requirePresent(t, s.tracker, otherListener)
}

// TestServer_AnAdsStreamCountsFromItsListenerRequest: a second proxy
// generation connects and asks for its clusters first, as Envoy does. Until
// it asks for listeners it is not a proxy that may hold one, and a pod DEL is
// not held up by it; from then on it is.
func TestServer_AnAdsStreamCountsFromItsListenerRequest(t *testing.T) {
	s := startDeltaServer(t, testServerListener(otherListener, 2))
	first := s.open(t, nil)
	resp, _ := first.recv()
	first.ack(resp)
	requireAbsent(t, s.tracker, testListener, "fixture: the one proxy does not hold it")

	second := s.dial(t)
	second.request(resourcev3.ClusterType, nil)
	clusters, _ := second.recv()
	second.ack(clusters)
	s.connected(t, 2)
	s.settled(t)
	requireAbsent(t, s.tracker, testListener, "a stream that has not asked for listeners held up the wait")

	second.request(resourcev3.ListenerType, nil)
	listeners, added := second.recv()
	require.Equal(t, []string{otherListener}, added)
	requireAbsent(t, s.tracker, testListener, "it holds none, and its first response does not carry this one")
	requireNotAbsent(t, s.tracker, otherListener, "it is counted now: it has been sent this one")
	second.ack(listeners)

	// The first proxy goes away. Its stream is forgotten, not kept as a
	// proxy that says nothing.
	first.cancel()
	s.connected(t, 1)
	requireAbsent(t, s.tracker, testListener)
	requirePresent(t, s.tracker, otherListener, "the proxy that is left holds it")
}

// TestServer_NoSnapshotYet: the proxy connects before the agent has a
// snapshot for it, so the opening request is answered later, from the
// snapshot set then. Until it is, nothing is known of what the proxy holds.
func TestServer_NoSnapshotYet(t *testing.T) {
	versionsOf := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2)).versions

	s := newDeltaServer(t)
	p := s.open(t, map[string]string{
		testListener:  versionsOf[testListener],
		otherListener: versionsOf[otherListener],
	})
	s.connected(t, 1)
	requireNotAbsent(t, s.tracker, listenerOf("no-such-pod"), "the statement is not answered")

	s.publish(t, "v1", testServerListener(testListener, 1), testServerListener(otherListener, 7))
	resp, added := p.recv()
	require.Equal(t, []string{otherListener}, added)
	requirePresent(t, s.tracker, testListener)
	requireNotPresent(t, s.tracker, otherListener)
	requireAbsent(t, s.tracker, listenerOf("no-such-pod"))
	p.ack(resp)
	requirePresent(t, s.tracker, otherListener)
}

// TestServer_StreamReset: the proxy that held the listener is gone with its
// stream, and the stream that follows is a proxy that holds nothing.
func TestServer_StreamReset(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))

	p := s.open(t, map[string]string{testListener: s.versions[testListener]})
	p.recv()
	requirePresent(t, s.tracker, testListener, "fixture")
	p.cancel()
	s.connected(t, 0)
	requireNotPresent(t, s.tracker, testListener)
	requireNotAbsent(t, s.tracker, testListener)

	p2 := s.open(t, nil)
	resp, added := p2.recv()
	require.Equal(t, []string{testListener}, added, "a proxy that states nothing is sent everything")
	requireNotPresent(t, s.tracker, testListener)
	p2.ack(resp)
	requirePresent(t, s.tracker, testListener)
}

// TestServer_ContentChangedBetweenTheOpeningResponseAndItsAck: the proxy
// states the listener at the version the snapshot publishes. Before it
// acknowledges the (empty) opening response, the SAME NAME is republished
// with OTHER CONTENT (a same-named replacement pod), and the CNI ADD for it
// starts its wait. That wait is for the replacement's own acknowledgement.
func TestServer_ContentChangedBetweenTheOpeningResponseAndItsAck(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	oldVersion := s.versions[testListener]

	p := s.open(t, map[string]string{testListener: oldVersion})
	opening, added := p.recv()
	require.Empty(t, added)

	s.publish(t, "v2", testServerListener(testListener, 2))
	require.NotEqual(t, oldVersion, s.versions[testListener])
	requireNotPresent(t, s.tracker, testListener, "the proxy holds the content of the pod that is gone")

	p.ack(opening)
	next, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	require.Equal(t, s.versions[testListener], next.GetResources()[0].GetVersion(),
		"the version on the wire is the version the tracker reads as published")
	requireNotPresent(t, s.tracker, testListener, "sent is not acknowledged")
	p.ack(next)
	requirePresent(t, s.tracker, testListener)
}

// TestServer_ReturnToAnAcknowledgedVersion: the proxy acknowledged v1. v2 is
// published and sent, not answered. v1 is published again. The last version
// acknowledged is the published one, and it is not what the proxy was last
// sent: the server has yet to send v1 again.
func TestServer_ReturnToAnAcknowledgedVersion(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, nil)
	first, _ := p.recv()
	p.ack(first)
	requirePresent(t, s.tracker, testListener)

	s.publish(t, "v2", testServerListener(testListener, 2))
	second, added := p.recv()
	require.Equal(t, []string{testListener}, added)

	s.publish(t, "v3", testServerListener(testListener, 1))
	requireNotPresent(t, s.tracker, testListener, "the proxy has been sent another version and not yet the published one again")

	p.ack(second)
	third, added := p.recv()
	require.Equal(t, []string{testListener}, added, "the server has to send the published version again")
	requireNotPresent(t, s.tracker, testListener)
	p.ack(third)
	requirePresent(t, s.tracker, testListener)
}

// TestServer_OlderRejectionWithThePublishedVersionOnItsWay: v1 is sent, v2
// published, v1 rejected. The wait is not failed by the rejection of v1; v2
// is sent and its ACK answers it.
func TestServer_OlderRejectionWithThePublishedVersionOnItsWay(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, nil)
	first, _ := p.recv()
	s.publish(t, "v2", testServerListener(testListener, 2))
	p.nack(first, "bind failed")
	second, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	requireNotPresent(t, s.tracker, testListener, "neither answered nor failed by the older rejection")
	p.ack(second)
	requirePresent(t, s.tracker, testListener)
}

// TestServer_RejectionThenIdenticalRepublish: a rejected listener republished
// with the same bytes is never sent again on the stream, so the wait keeps
// failing with the rejection; the proxy that reconnects is sent it again.
func TestServer_RejectionThenIdenticalRepublish(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, nil)
	first, _ := p.recv()
	p.nack(first, "bind failed")
	s.settled(t)
	requireRejected(t, s.tracker, testListener, "bind failed")

	s.publish(t, "v2", testServerListener(testListener, 1))
	requireRejected(t, s.tracker, testListener, "bind failed", "identical bytes: nothing is sent again, the rejection stands")
	// And it is not absent either: the removal has not been acknowledged.
	s.publish(t, "v3")
	removal, _ := p.recv()
	require.Equal(t, []string{testListener}, removal.GetRemovedResources(),
		"go-control-plane removes what it wrote, acknowledged or not")
	requireNotAbsent(t, s.tracker, testListener)
	p.ack(removal)
	requireAbsent(t, s.tracker, testListener)
}

// TestServer_TwoGenerationsRemoveAListener is a pod DEL during a hot restart:
// both generations hold the listener, both are sent its removal, and the
// removal wait returns when the second of them has acknowledged it.
func TestServer_TwoGenerationsRemoveAListener(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))
	stated := map[string]string{testListener: s.versions[testListener], otherListener: s.versions[otherListener]}
	old := s.open(t, stated)
	opening, _ := old.recv()
	old.ack(opening)
	// The new generation starts with nothing and is sent everything.
	next := s.open(t, nil)
	sent, _ := next.recv()
	next.ack(sent)
	s.settled(t)
	requirePresent(t, s.tracker, testListener, "fixture")

	s.publish(t, "v2", testServerListener(otherListener, 2))
	removedOld, _ := old.recv()
	removedNext, _ := next.recv()
	require.Equal(t, []string{testListener}, removedOld.GetRemovedResources())
	require.Equal(t, []string{testListener}, removedNext.GetRemovedResources())
	requireNotAbsent(t, s.tracker, testListener)

	next.ack(removedNext)
	s.leftUnanswered(t, 1)
	requireNotAbsent(t, s.tracker, testListener, "the old generation has not acknowledged the removal")

	old.ack(removedOld)
	requireAbsent(t, s.tracker, testListener)
}

// gatedServer is newDeltaServer with one more callback in front of the
// tracker's, as the agent's on-demand observer is: it holds the stream's
// goroutine on a Cluster request until released. That is a stream goroutine
// that is busy (a Send held by flow control, CreateDeltaWatch waiting for the
// cache mutex, another callback at work) while the cache answers a watch.
func gatedServer(t *testing.T) (s *deltaServer, entered <-chan struct{}, release func()) {
	t.Helper()
	s = &deltaServer{
		tracker: NewTracker(slog.New(slog.DiscardHandler)),
		cache:   cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil),
	}
	s.tracker.SetPublishedVersion(SnapshotVersions(s.cache, serverNodeID))

	in := make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	inner := s.tracker.Callbacks()
	callbacks := serverv3.CallbackFuncs{
		DeltaStreamOpenFunc:     inner.OnDeltaStreamOpen,
		DeltaStreamClosedFunc:   inner.OnDeltaStreamClosed,
		StreamDeltaResponseFunc: inner.OnStreamDeltaResponse,
		StreamDeltaRequestFunc: func(id int64, req *discoveryv3.DeltaDiscoveryRequest) error {
			if req.GetTypeUrl() == resourcev3.ClusterType {
				in <- struct{}{}
				<-gate
			}
			return inner.OnStreamDeltaRequest(id, req)
		},
	}

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), s.cache, callbacks))
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	s.conn = conn
	return s, in, release
}

// TestServer_AbsentReturnsWhileAnAddIsOwedToTheProxy is a LIMIT of the
// tracker, pinned so that nothing claims otherwise. It is not timed: the
// stream's goroutine is held, not raced.
//
// The cache has computed the response that adds a pod's listener (the ADD)
// and the stream has not written it. The pod is deleted. The tracker learns
// of a response when it is written, so it knows of nothing owed to the proxy,
// and the DEL's wait returns "absent". The proxy is then handed the add.
//
// Closing it needs the order the cache computed things in, which the tracker
// is not told.
func TestServer_AbsentReturnsWhileAnAddIsOwedToTheProxy(t *testing.T) {
	s, entered, release := gatedServer(t)
	s.publish(t, "v1", testServerListener(otherListener, 2))

	p := s.open(t, nil)
	opening, _ := p.recv()
	p.ack(opening)
	s.settled(t)
	requireAbsent(t, s.tracker, testListener, "fixture: the proxy said what it holds")

	// The stream's goroutine is busy.
	p.request(resourcev3.ClusterType, nil)
	select {
	case <-entered:
	case <-time.After(resolvedWait):
		t.Fatal("the stream never took the Cluster request")
	}

	// CNI ADD: the listener is published; the open Listener watch is answered
	// into the stream's channel.
	s.publish(t, "v2", testServerListener(otherListener, 2), testServerListener(testListener, 1))
	// CNI DEL: it leaves the snapshot. No Listener watch is open, so nothing
	// is computed for it.
	s.publish(t, "v3", testServerListener(otherListener, 2))

	requireAbsent(t, s.tracker, testListener, "the limit: the tracker has not been told of the add")

	// What the proxy is sent next.
	release()
	var added []string
	for range 2 {
		resp, names := p.recv()
		if resp.GetTypeUrl() == resourcev3.ListenerType {
			added = names
			break
		}
	}
	require.Equal(t, []string{testListener}, added, "the proxy is handed the listener after the wait returned")
	requireNotAbsent(t, s.tracker, testListener, "from here the tracker knows")
}

// TestSnapshotVersions: the published version of a listener is the one in the
// version map of the snapshot the cache serves the node. A node with no
// snapshot, a name the snapshot does not have, and a snapshot whose versions
// have not been computed all publish nothing: a wait is then never answered
// by a version nobody compared.
func TestSnapshotVersions(t *testing.T) {
	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	published := SnapshotVersions(cache, serverNodeID)

	_, ok := published(resourcev3.ListenerType, testListener)
	require.False(t, ok, "no snapshot for the node")

	snapshot, err := cachev3.NewSnapshot("v1", map[resourcev3.Type][]types.Resource{
		resourcev3.ListenerType: {testServerListener(testListener, 1)},
	})
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(context.Background(), serverNodeID, snapshot))
	_, ok = published(resourcev3.ListenerType, testListener)
	require.False(t, ok, "the snapshot's versions are not computed yet")

	require.NoError(t, snapshot.ConstructVersionMap())
	version, ok := published(resourcev3.ListenerType, testListener)
	require.True(t, ok)
	require.Equal(t, snapshot.GetVersionMap(resourcev3.ListenerType)[testListener], version)
	require.NotEmpty(t, version)

	_, ok = published(resourcev3.ListenerType, otherListener)
	require.False(t, ok, "not in the snapshot")
	_, ok = published(resourcev3.ClusterType, testListener)
	require.False(t, ok, "a listener is not a cluster")
	_, ok = SnapshotVersions(cache, "another-node")(resourcev3.ListenerType, testListener)
	require.False(t, ok, "another node's snapshot")
}
