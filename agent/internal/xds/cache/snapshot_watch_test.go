package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// The tests in this file are about #1619 and #1620: what a snapshot build
// waits for inside go-control-plane's SetSnapshot, which it calls with
// snapshotMu held.
//
// They run the agent's cache under the pinned go-control-plane SERVER (the
// stream handlers of pkg/server/v3, with their own channels), not under a
// watch made by hand: whether a build can stall is a property of the channel a
// stream gives the cache, and a test that makes the channel itself decides the
// answer.

// stuckProxyWait is how long a build is given before the test calls it
// stalled. A build here takes milliseconds.
const stuckProxyWait = 30 * time.Second

// servedDeltaTypes is the resource types a node proxy watches on its delta ADS
// stream: the ones a node agent's snapshot carries.
var servedDeltaTypes = []string{
	resourcev3.ClusterType,
	resourcev3.EndpointType,
	resourcev3.ListenerType,
	resourcev3.RouteType,
	resourcev3.SecretType,
	resourcev3.ExtensionConfigType,
}

// proxyStream is the server side of one xDS stream, with the proxy played by
// the test. Send blocks until the test takes the response (recv): a test that
// stops taking them is a proxy that stopped reading its stream, in the
// strictest form, with no flow-control window to fill first.
type proxyStream[Req, Resp any] struct {
	grpc.ServerStream
	ctx      context.Context
	requests chan Req
	sent     chan Resp
	// sending is signalled each time the stream's goroutine enters Send.
	sending chan struct{}
}

func newProxyStream[Req, Resp any](ctx context.Context) *proxyStream[Req, Resp] {
	return &proxyStream[Req, Resp]{
		ctx:      ctx,
		requests: make(chan Req),
		sent:     make(chan Resp),
		sending:  make(chan struct{}, 64),
	}
}

func (s *proxyStream[Req, Resp]) Context() context.Context { return s.ctx }

func (s *proxyStream[Req, Resp]) Send(resp Resp) error {
	select {
	case s.sending <- struct{}{}:
	default:
	}
	select {
	case s.sent <- resp:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *proxyStream[Req, Resp]) Recv() (Req, error) {
	select {
	case req := <-s.requests:
		return req, nil
	case <-s.ctx.Done():
		var zero Req
		return zero, io.EOF
	}
}

// request sends one request as the proxy.
func (s *proxyStream[Req, Resp]) request(t *testing.T, req Req) {
	t.Helper()
	select {
	case s.requests <- req:
	case <-time.After(stuckProxyWait):
		t.Fatal("the stream did not take the proxy's request")
	}
}

// recv takes the next response as the proxy.
func (s *proxyStream[Req, Resp]) recv(t *testing.T) Resp {
	t.Helper()
	select {
	case resp := <-s.sent:
		return resp
	case <-time.After(stuckProxyWait):
		t.Fatal("the stream wrote no response")
		panic("unreachable")
	}
}

// stopReading marks the moment the proxy stops taking responses: it forgets
// the writes made so far, so that awaitSending is about a later one. Call it
// when every response written so far has been taken.
func (s *proxyStream[Req, Resp]) stopReading() {
	for {
		select {
		case <-s.sending:
		default:
			return
		}
	}
}

// awaitSending returns once the stream's goroutine is in Send, with a response
// the proxy has not taken.
func (s *proxyStream[Req, Resp]) awaitSending(t *testing.T) {
	t.Helper()
	select {
	case <-s.sending:
	case <-time.After(stuckProxyWait):
		t.Fatal("the stream never tried to write a response")
	}
}

type deltaProxyStream = proxyStream[*discoveryv3.DeltaDiscoveryRequest, *discoveryv3.DeltaDiscoveryResponse]

// serveDeltaADS runs the pinned server's delta ADS handler over the cache for
// one proxy stream, and stops it when the test ends.
func serveDeltaADS(t *testing.T, c *SnapshotCache, callbacks serverv3.Callbacks) *deltaProxyStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := serverv3.NewServer(ctx, c, callbacks)
	stream := newProxyStream[*discoveryv3.DeltaDiscoveryRequest, *discoveryv3.DeltaDiscoveryResponse](ctx)
	var done sync.WaitGroup
	done.Go(func() { _ = srv.DeltaStreamHandler(stream, resourcev3.AnyType) })
	t.Cleanup(func() {
		cancel()
		done.Wait()
	})
	return stream
}

// openEveryDeltaWatch plays a proxy's start on the stream: a wildcard
// subscription to every served type, each first response acknowledged. When it
// returns the cache holds one open watch per type, which is the state a
// connected proxy at rest is in.
func openEveryDeltaWatch(t *testing.T, c *SnapshotCache, stream *deltaProxyStream) {
	t.Helper()
	for _, typeURL := range servedDeltaTypes {
		stream.request(t, &discoveryv3.DeltaDiscoveryRequest{Node: &corev3.Node{Id: c.nodeName}, TypeUrl: typeURL})
		resp := stream.recv(t)
		require.Equal(t, typeURL, resp.GetTypeUrl())
		stream.request(t, &discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeURL, ResponseNonce: resp.GetNonce()})
	}
	awaitDeltaWatches(t, c, len(servedDeltaTypes))
}

// awaitDeltaWatches waits until the cache holds n open delta watches for the
// node.
func awaitDeltaWatches(t *testing.T, c *SnapshotCache, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		info := c.GetStatusInfo(c.nodeName)
		return info != nil && info.GetNumDeltaWatches() == n
	}, stuckProxyWait, time.Millisecond, "open delta watches")
}

// changeEveryType changes the cache so that the next snapshot differs from the
// last in every type a proxy watches except listeners (a pod, which the caller
// adds with the build): a cluster with its load assignment and its virtual
// host, a secret, and the subset-header extension config.
func changeEveryType(c *SnapshotCache, i int) {
	name := fmt.Sprintf("svc-%03d.aether-test.aether.internal", i)
	c.clusterMu.Lock()
	c.clusters[name] = clusterEntry{
		cluster:        &clusterv3.Cluster{Name: name},
		loadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: name},
		vhost:          &routev3.VirtualHost{Name: name, Domains: []string{name}},
		service:        "aether-test/echo",
		sanNamespaces:  []string{"aether-test"},
	}
	c.clusterMu.Unlock()
	c.recomputeMTLSClusters()
	serveSecrets(c, fmt.Sprintf("spiffe://aether.internal/ns/aether-test/sa/svc-%03d", i))
	c.subsetMu.Lock()
	c.subsetHeaderKeys = append(c.subsetHeaderKeys, fmt.Sprintf("x-subset-%03d", i))
	c.subsetMu.Unlock()
}

// watchTestPod is a pod of its own netns, so that adding it changes the
// listener set.
func watchTestPod(i int) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             fmt.Sprintf("echo-%03d", i),
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: fmt.Sprintf("/var/run/netns/cni-%03d", i),
	}
}

// buildEveryType changes every watched type and builds one snapshot with it,
// through AddPod as a CNI ADD does, and fails the test if the build is still
// running after stuckProxyWait.
func buildEveryType(t *testing.T, ctx context.Context, c *SnapshotCache, i int) error {
	t.Helper()
	changeEveryType(c, i)
	done := make(chan error, 1)
	go func() { done <- c.AddPod(ctx, watchTestPod(i), bindingTrustDomain) }()
	select {
	case err := <-done:
		return err
	case <-time.After(stuckProxyWait):
		t.Fatalf("snapshot build %d is still waiting after %s: it is stalled on a watch", i, stuckProxyWait)
		return nil
	}
}

// typeCounts takes n responses from the stream and counts them by type.
func typeCounts(t *testing.T, stream *deltaProxyStream, n int) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(servedDeltaTypes))
	for range n {
		counts[stream.recv(t).GetTypeUrl()]++
	}
	return counts
}

// TestABuildDoesNotWaitOnAProxyThatStoppedReading is the state #1619 asks
// about: a connected proxy, with a watch open for every type, stops reading
// its stream, and the node goes on changing. The stream's goroutine is then
// blocked writing the first response, and the others have to go somewhere.
//
// They go into the stream's channel, which has room for them: a watch is
// answered once, and the stream opens the next watch of a type only when it
// handles the proxy's next request, which a proxy that reads nothing never
// gets to make. So the first build after the proxy stops hands over one
// response per type and every later build finds no watch to answer. No build
// waits, and none returns an error.
func TestABuildDoesNotWaitOnAProxyThatStoppedReading(t *testing.T) {
	c, _, _, tracker := ackedPinFixture(t)
	ctx := context.Background()
	require.NoError(t, buildEveryType(t, ctx, c, 0))
	stream := serveDeltaADS(t, c, tracker.Callbacks())
	openEveryDeltaWatch(t, c, stream)

	// The proxy reads nothing from here on.
	stream.stopReading()
	require.NoError(t, buildEveryType(t, ctx, c, 1))
	assert.Zero(t, c.GetStatusInfo(c.nodeName).GetNumDeltaWatches(),
		"every watch was answered into the stream's channel although the stream wrote none of it")
	stream.awaitSending(t)
	for i := 2; i < 40; i++ {
		require.NoError(t, buildEveryType(t, ctx, c, i), "build %d with the proxy not reading", i)
	}

	// The proxy reads again: one response per type, each from the snapshot of
	// the build that answered its watch, and the stream carries on.
	counts := typeCounts(t, stream, len(servedDeltaTypes))
	for _, typeURL := range servedDeltaTypes {
		assert.Equal(t, 1, counts[typeURL], "responses of type %s while the proxy did not read", typeURL)
	}
}

// TestABuildDoesNotWaitWhenTheStreamIsBetweenARequestAndItsWatch drives the
// stream's channel to the most it can hold, which is one response more than
// the test above: the stream has emptied its channel to handle a request and
// has not yet replaced that type's watch (it is in the request callback, where
// the agent admits on-demand clusters), a build answers every open watch, the
// old watch of that type included, and the watch the request then opens is
// answered at once as well. That is one response per type plus one, seven for
// the agent's six types, in a channel with room for twenty.
//
// After it the proxy reads nothing, and builds go on. None waits.
func TestABuildDoesNotWaitWhenTheStreamIsBetweenARequestAndItsWatch(t *testing.T) {
	c, _, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	require.NoError(t, buildEveryType(t, ctx, c, 0))

	inCallback := make(chan struct{})
	release := make(chan struct{})
	var hold, released sync.Once
	releaseStream := func() { released.Do(func() { close(release) }) }
	var armed bool
	var mu sync.Mutex
	stream := serveDeltaADS(t, c, serverv3.CallbackFuncs{
		StreamDeltaRequestFunc: func(int64, *discoveryv3.DeltaDiscoveryRequest) error {
			mu.Lock()
			held := armed
			mu.Unlock()
			if held {
				hold.Do(func() {
					close(inCallback)
					<-release
				})
			}
			return nil
		},
	})
	// Registered after the stream's own cleanup, so it runs before it: the
	// stream's goroutine may be waiting in the callback when the test ends
	// early, and the handler cannot return until it is let go.
	t.Cleanup(releaseStream)
	openEveryDeltaWatch(t, c, stream)

	// A cluster request with no nonce, as an on-demand subscription sends. The
	// stream stops in its callback: past the point where it emptied its
	// channel, before it cancels the cluster watch and opens the next.
	stream.stopReading()
	mu.Lock()
	armed = true
	mu.Unlock()
	stream.request(t, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResourceNamesSubscribe: []string{"quic:not-published"},
	})
	select {
	case <-inCallback:
	case <-time.After(stuckProxyWait):
		t.Fatal("fixture: the stream never reached its request callback")
	}
	require.NoError(t, buildEveryType(t, ctx, c, 1))
	require.Zero(t, c.GetStatusInfo(c.nodeName).GetNumDeltaWatches(), "fixture: the build answered every open watch")
	releaseStream()
	// The request's own watch is answered when it is made (the stream was not
	// told of the cluster response that is still in its channel), and then the
	// stream blocks writing the first of the seven.
	stream.awaitSending(t)
	for i := 2; i < 40; i++ {
		require.NoError(t, buildEveryType(t, ctx, c, i), "build %d with the proxy not reading", i)
	}

	counts := typeCounts(t, stream, len(servedDeltaTypes)+1)
	for _, typeURL := range servedDeltaTypes {
		want := 1
		if typeURL == resourcev3.ClusterType {
			want = 2
		}
		assert.Equal(t, want, counts[typeURL], "responses of type %s while the proxy did not read", typeURL)
	}
}

// TestABuildDoesNotWaitOnAStuckSecretStream is the same for the other kind of
// stream a node proxy holds: a state-of-the-world secret stream, one per SDS
// secret. That server makes a channel with room for one response for every
// request and a watch is answered once, so a proxy that stops reading leaves
// one response in the channel and no watch.
func TestABuildDoesNotWaitOnAStuckSecretStream(t *testing.T) {
	c, _, _, tracker := ackedPinFixture(t)
	ctx := context.Background()
	const secret = "spiffe://aether.internal/ns/aether-test/sa/echo"
	serveSecrets(c, secret)
	require.NoError(t, buildEveryType(t, ctx, c, 0))

	streamCtx, cancel := context.WithCancel(ctx)
	srv := serverv3.NewServer(streamCtx, c, tracker.Callbacks())
	stream := newProxyStream[*discoveryv3.DiscoveryRequest, *discoveryv3.DiscoveryResponse](streamCtx)
	var done sync.WaitGroup
	done.Go(func() { _ = srv.StreamHandler(stream, resourcev3.SecretType) })
	t.Cleanup(func() {
		cancel()
		done.Wait()
	})

	stream.request(t, &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: c.nodeName}, ResourceNames: []string{secret}})
	first := stream.recv(t)
	require.Len(t, first.GetResources(), 1)
	stream.request(t, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{secret}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})
	require.Eventually(t, func() bool {
		return c.GetStatusInfo(c.nodeName).GetNumWatches() == 1
	}, stuckProxyWait, time.Millisecond, "the acknowledged secret request leaves a watch open")

	// The proxy reads nothing from here on.
	stream.stopReading()
	require.NoError(t, buildEveryType(t, ctx, c, 1))
	assert.Zero(t, c.GetStatusInfo(c.nodeName).GetNumWatches(), "the watch was answered into its channel")
	stream.awaitSending(t)
	for i := 2; i < 40; i++ {
		require.NoError(t, buildEveryType(t, ctx, c, i), "build %d with the proxy not reading", i)
	}
	assert.NotEqual(t, first.GetVersionInfo(), stream.recv(t).GetVersionInfo())
}

// TestACallersEndedContextDoesNotLeaveAWatchUnanswered is #1620 as it can
// happen. SetSnapshot hands each open watch its response in a select against
// the end of the context it was called with. With the caller's context, a
// caller whose context has already ended (a CNI ADD whose RPC was abandoned,
// say) makes both ready for every watch, and which one wins is random, watch
// by watch. The first watch to lose ended the set: the build returned an error
// for a snapshot that was installed, and that watch and the ones after it
// were left unanswered, so the proxy was not sent the change.
//
// The proxy here reads everything it is sent. Every build under an ended
// context must still succeed and deliver.
func TestACallersEndedContextDoesNotLeaveAWatchUnanswered(t *testing.T) {
	c, _, _, tracker := ackedPinFixture(t)
	require.NoError(t, buildEveryType(t, context.Background(), c, 0))
	stream := serveDeltaADS(t, c, tracker.Callbacks())
	openEveryDeltaWatch(t, c, stream)

	ended, end := context.WithCancel(context.Background())
	end()
	for i := 1; i <= 20; i++ {
		require.NoError(t, buildEveryType(t, ended, c, i),
			"build %d: the snapshot is installed, and the caller's context has nothing to do with the proxy's stream", i)
		// The build answered every watch: the proxy is sent one response per
		// type, and acknowledges each, which opens the watches again.
		responses := make([]*discoveryv3.DeltaDiscoveryResponse, 0, len(servedDeltaTypes))
		for range servedDeltaTypes {
			responses = append(responses, stream.recv(t))
		}
		for _, resp := range responses {
			stream.request(t, &discoveryv3.DeltaDiscoveryRequest{TypeUrl: resp.GetTypeUrl(), ResponseNonce: resp.GetNonce()})
		}
		awaitDeltaWatches(t, c, len(servedDeltaTypes))
	}
}

// TestABuildThatCouldNotAnswerAWatchSaysSo is the other half of #1620: what a
// build returns if a watch's channel does refuse its response. No stream of the
// pinned server does (the tests above), so the watch here is made by hand, on a
// channel nobody reads. The build waits watchAnswerTimeout, not for the
// caller's context, installs the snapshot, and returns an error a caller can
// tell from a snapshot that was not built.
func TestABuildThatCouldNotAnswerAWatchSaysSo(t *testing.T) {
	c, rec, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	c.watchAnswerTimeout = 50 * time.Millisecond
	require.NoError(t, buildEveryType(t, ctx, c, 0))

	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node: &corev3.Node{Id: c.nodeName}, TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}, streamv3.NewDeltaSubscription(nil, nil, clusterVersions(t, c), true), make(chan cachev3.DeltaResponse))
	require.NoError(t, err)
	require.NotNil(t, cancel, "fixture: the watch must be open")
	defer cancel()

	rec.reset()
	start := time.Now()
	err = buildEveryType(t, ctx, c, 1)
	require.ErrorIs(t, err, ErrWatchNotAnswered)
	assert.False(t, snapshotInstalled(errors.New("failed to create snapshot")), "any other build error is not an installed snapshot")
	assert.True(t, snapshotInstalled(err))
	waited := time.Since(start)
	assert.GreaterOrEqual(t, waited, c.watchAnswerTimeout, "it waited the bound")
	assert.Less(t, waited, defaultWatchAnswerTimeout, "the bound in force, not the default")
	assert.Contains(t, clusterVersions(t, c), "svc-001.aether-test.aether.internal", "the snapshot is the one served")
	lines := rec.with(snapshotWatchUnansweredMsg)
	require.Len(t, lines, 1)
	assert.Equal(t, snapshotVersion(t, c), lines[0].attrs["snapshot_version"])
}

// TestCallersThatOnlyLogDoNotCallAnInstalledSnapshotAFailure: the callers
// inside the package that have nobody to return a build's error to log it as
// "failed to regenerate snapshot". For ErrWatchNotAnswered that would be untrue
// (the snapshot is installed and served) and a second line for one event: the
// build has already logged the unanswered watch at WARN.
func TestCallersThatOnlyLogDoNotCallAnInstalledSnapshotAFailure(t *testing.T) {
	c, rec, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	c.watchAnswerTimeout = 20 * time.Millisecond
	c.quicPublishWindow = -1
	require.NoError(t, buildEveryType(t, ctx, c, 0))

	// A watch nobody reads, owed a cluster: every build from here on fails to
	// answer it.
	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node: &corev3.Node{Id: c.nodeName}, TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}, streamv3.NewDeltaSubscription(nil, nil, clusterVersions(t, c), true), make(chan cachev3.DeltaResponse))
	require.NoError(t, err)
	require.NotNil(t, cancel, "fixture: the watch must be open")
	defer cancel()
	changeEveryType(c, 1)

	rec.reset()
	c.regenerateAllAppDeliveryClusters()
	c.regenerateAllHTTPListeners()
	c.rebuildIdentityDerived(ctx)
	c.quicPublish.mu.Lock()
	c.quicPublish.pending, c.quicPublish.running = true, true
	c.quicPublish.mu.Unlock()
	c.runQUICPublisher(ctx)

	require.Len(t, rec.with(snapshotWatchUnansweredMsg), 4, "fixture: each of the four builds failed to answer the watch")
	for _, l := range rec.all() {
		assert.Less(t, l.level, slog.LevelError, "an installed snapshot is not reported as a failed build: %q", l.msg)
	}
	assert.Len(t, rec.with("published observed east-west QUIC pairs: one snapshot for the coalesced admissions"), 1,
		"and the publisher says it published")
}
