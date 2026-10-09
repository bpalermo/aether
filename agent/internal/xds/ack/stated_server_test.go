package ack

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// The tests in stated_test.go drive the tracker's callbacks by hand, so they
// take on trust what go-control-plane puts in the first response of a stream.
// These run the tracker behind the real go-control-plane delta server and
// snapshot cache, with real per-resource versions, and a client that plays the
// proxy: they hold the tracker to "a wait is resolved from the opening
// exchange only for the version the proxy stated", and fail on a
// go-control-plane bump that changes how the opening request is answered.

const (
	serverNodeID  = "node"
	otherListener = "outbound_http_other-pod"
)

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

// proxyStream is one delta ADS stream of the pretend proxy.
type proxyStream struct {
	t      *testing.T
	stream discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesClient
	cancel context.CancelFunc
}

// open starts a stream and sends the opening Listener request, stating the
// given versions.
func (s *deltaServer) open(t *testing.T, stated map[string]string) *proxyStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(s.conn).DeltaAggregatedResources(ctx)
	require.NoError(t, err)
	p := &proxyStream{t: t, stream: stream, cancel: cancel}
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node:                    &corev3.Node{Id: serverNodeID},
		TypeUrl:                 resourcev3.ListenerType,
		InitialResourceVersions: stated,
	}))
	return p
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
		TypeUrl: resourcev3.ListenerType, ResponseNonce: resp.GetNonce(),
	}))
}

func (p *proxyStream) nack(resp *discoveryv3.DeltaDiscoveryResponse, msg string) {
	p.t.Helper()
	require.NoError(p.t, p.stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ListenerType, ResponseNonce: resp.GetNonce(),
		ErrorDetail: status.New(codes.InvalidArgument, msg).Proto(),
	}))
}

// TestServer_StatedAtThePublishedVersionResolvesTheWait is #1511 end to end
// on the server side: the proxy states both listeners at the versions the
// snapshot publishes, go-control-plane answers with an empty response, and the
// ACK of it resolves the waits.
func TestServer_StatedAtThePublishedVersionResolvesTheWait(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))

	p := s.open(t, map[string]string{
		testListener:  s.versions[testListener],
		otherListener: s.versions[otherListener],
	})
	resp, added := p.recv()
	require.Empty(t, added, "go-control-plane sends nothing for a listener stated at the published version")
	require.Empty(t, resp.GetRemovedResources())
	requireNotPresent(t, s.tracker, testListener, "the opening response is not acknowledged yet")

	p.ack(resp)
	requirePresent(t, s.tracker, testListener)
	requirePresent(t, s.tracker, otherListener)
}

// TestServer_StatedAtAnotherVersionIsNotResolvedByTheStatement: a listener the
// proxy states at a version that is not the published one, whether an older
// one, a newer one (the agent rolled back) or an empty one, is sent again, and
// its wait is resolved by nothing but the proxy's answer to that response.
// The listener stated at the published version next to it shows the opening
// exchange was read, so the unresolved wait is not the tracker doing nothing.
func TestServer_StatedAtAnotherVersionIsNotResolvedByTheStatement(t *testing.T) {
	// Real versions of the same listener with other bytes: what a proxy that
	// last heard from an agent publishing a different config states.
	other := startDeltaServer(t, testServerListener(testListener, 9))
	otherVersion := other.versions[testListener]

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

			p.nack(resp, "Permission denied")
			ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
			defer cancel()
			err := s.tracker.WaitListenerPresent(ctx, testListener)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Permission denied")
			requireNotPresent(t, s.tracker, otherListener, "a rejected opening response resolves nothing")
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
		requirePresent(t, s.tracker, otherListener, "and stated this one at the published version")
	})
}

// TestServer_NotStatedIsNotResolvedByTheOpeningExchange: the proxy states one
// listener and not the other. The other is sent, and stays unresolved until
// its own acknowledgement.
func TestServer_NotStatedIsNotResolvedByTheOpeningExchange(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))

	p := s.open(t, map[string]string{otherListener: s.versions[otherListener]})
	_, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	requireNotPresent(t, s.tracker, testListener, "the proxy never stated it")
	requireNotPresent(t, s.tracker, otherListener, "and has not acknowledged the opening response")
}

// TestServer_StatedButNotPublishedIsRemoved: a listener the proxy states that
// the snapshot does not have is removed by the opening response, never
// resolved as present.
func TestServer_StatedButNotPublishedIsRemoved(t *testing.T) {
	s := startDeltaServer(t, testServerListener(otherListener, 2))

	p := s.open(t, map[string]string{
		testListener:  "anything",
		otherListener: s.versions[otherListener],
	})
	resp, added := p.recv()
	require.Empty(t, added)
	require.Equal(t, []string{testListener}, resp.GetRemovedResources())
	p.ack(resp)
	requirePresent(t, s.tracker, otherListener)
	requireNotPresent(t, s.tracker, testListener)
}

// TestServer_NackAfterTheOpeningSurfaces: a listener resolved from the opening
// exchange and then updated with a config the proxy rejects fails its wait
// with the proxy's error.
func TestServer_NackAfterTheOpeningSurfaces(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))

	p := s.open(t, map[string]string{testListener: s.versions[testListener]})
	resp, _ := p.recv()
	p.ack(resp)
	requirePresent(t, s.tracker, testListener)

	s.publish(t, "v2", testServerListener(testListener, 3))
	resp, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	p.nack(resp, "Permission denied")

	// The listener is known present, so a wait returns at once until the
	// server has read the NACK.
	var err error
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		err = s.tracker.WaitListenerPresent(ctx, testListener)
		return err != nil
	}, resolvedWait, time.Millisecond)
	assert.Contains(t, err.Error(), "Permission denied")
}

// TestServer_StreamResetBeforeTheAcknowledgement: the proxy states the
// listener, the stream ends before it acknowledges the opening response, and
// the stream that follows is a proxy that holds nothing and does not
// acknowledge what it is sent. Nothing is resolved.
func TestServer_StreamResetBeforeTheAcknowledgement(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))

	p := s.open(t, map[string]string{testListener: s.versions[testListener]})
	_, added := p.recv()
	require.Empty(t, added)
	p.cancel()
	require.Eventually(t, func() bool {
		s.tracker.mu.Lock()
		defer s.tracker.mu.Unlock()
		return len(s.tracker.answered) == 0
	}, resolvedWait, time.Millisecond, "the server never closed the stream")

	p2 := s.open(t, nil)
	_, added = p2.recv()
	require.Equal(t, []string{testListener}, added, "a proxy that states nothing is sent everything")
	requireNotPresent(t, s.tracker, testListener)
}

// TestServer_SnapshotChangedBetweenStatementAndResponse: the proxy connects
// before the agent has a snapshot for it, so the opening request is answered
// later, from the snapshot set then. The comparison is still with what the
// proxy stated: the listener whose published version is the stated one is
// resolved, the one that differs is sent.
func TestServer_SnapshotChangedBetweenStatementAndResponse(t *testing.T) {
	versionsOf := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2)).versions

	s := newDeltaServer(t)
	p := s.open(t, map[string]string{
		testListener:  versionsOf[testListener],
		otherListener: versionsOf[otherListener],
	})
	// The opening request is held: there is no snapshot to answer it from.
	require.Eventually(t, func() bool {
		s.tracker.mu.Lock()
		defer s.tracker.mu.Unlock()
		return len(s.tracker.stated) == 1
	}, resolvedWait, time.Millisecond)

	s.publish(t, "v1", testServerListener(testListener, 1), testServerListener(otherListener, 7))
	resp, added := p.recv()
	require.Equal(t, []string{otherListener}, added)
	p.ack(resp)
	requirePresent(t, s.tracker, testListener)
	requirePresent(t, s.tracker, otherListener)
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
