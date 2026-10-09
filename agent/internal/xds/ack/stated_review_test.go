package ack

import (
	"context"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// The cases an adversarial review of #1511 found, where reading the opening
// exchange by the listener's NAME was wrong. The first four failed on the
// first version of the change; the rest held and are kept as its limits.

// holds reports whether the tracker's last word on the listener is that a
// proxy holds some version of it, whatever is published.
func holds(tr *Tracker, name string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.state[resourcev3.ListenerType+"/"+name].present
}

// TestServer_ContentChangedBetweenTheOpeningResponseAndItsAck: the proxy states the
// listener at the version the snapshot publishes when the opening response is
// computed. Before the proxy acknowledges that (empty) response, the snapshot
// republishes the SAME NAME with OTHER CONTENT (a same-named replacement pod:
// new netns), and the CNI ADD for it starts its wait. The ACK of the opening
// response, which does not carry the listener, resolves the wait although the
// proxy holds only the old content; the new content is sent only afterwards.
func TestServer_ContentChangedBetweenTheOpeningResponseAndItsAck(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	oldVersion := s.versions[testListener]

	p := s.open(t, map[string]string{testListener: oldVersion})
	opening, added := p.recv()
	require.Empty(t, added)
	require.Empty(t, opening.GetRemovedResources())

	// The replacement's listener is published; its ADD waits.
	s.publish(t, "v2", testServerListener(testListener, 2))
	require.NotEqual(t, oldVersion, s.versions[testListener])
	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		waited <- s.tracker.WaitListenerPresent(ctx, testListener)
	}()

	p.ack(opening)
	// The server sends the new content only now; the proxy has not seen it,
	// let alone acknowledged it.
	next, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	require.Equal(t, s.versions[testListener], next.GetResources()[0].GetVersion())

	err := <-waited
	require.Error(t, err, "the wait was resolved by the ACK of a response that did not carry the listener, while the proxy holds only the old content")
}

// TestServer_ReplacementAfterARestartWaitsForItsOwnAck: no tight race. After an
// agent restart the proxy states the listener of a pod whose DEL the agent
// never served (the plugin returns success on an unreachable agent), and the
// opening exchange marks it present. The same-named replacement's ADD then
// publishes other content and its wait returns at once, before the proxy has
// been sent it. On main the restarted agent knew nothing and waited for the
// real ACK.
func TestServer_ReplacementAfterARestartWaitsForItsOwnAck(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, map[string]string{testListener: s.versions[testListener]})
	opening, _ := p.recv()
	p.ack(opening)
	// Let the server process the ACK before the snapshot changes.
	require.Eventually(t, func() bool {
		s.tracker.mu.Lock()
		defer s.tracker.mu.Unlock()
		return len(s.tracker.inflight) == 0
	}, resolvedWait, time.Millisecond)

	s.publish(t, "v2", testServerListener(testListener, 2))
	_, added := p.recv()
	require.Equal(t, []string{testListener}, added, "the replacement is on the wire, not acknowledged")
	requireNotPresent(t, s.tracker, testListener, "nothing carrying the replacement's listener has been acknowledged")
}

// TestOpeningExchange_LateAckOfAnotherGenerationDoesNotUndoARemoval: two proxy
// generations are connected (hot restart during an agent restart). The old one
// states the listener and is answered. The pod is deleted; the new generation
// acknowledges the removal and the DEL's wait returns. The old generation's
// ACK of its opening response arrives afterwards and marks the listener
// present again; the old generation then exits before it acknowledges its own
// removal. Nothing ever clears the state: the snapshot no longer has the
// listener, so no stream is ever sent a removal for it.
func TestOpeningExchange_LateAckOfAnotherGenerationDoesNotUndoARemoval(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)

	// Old generation reconnects, stating the listener; the snapshot has it.
	openDelta(tr, parent, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, parent, "p1", nil, nil)

	// New generation holds it too (sent and acknowledged on its stream).
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendDelta(tr, child, "c1", []string{testListener}, nil)
	ackDelta(tr, child, "c1", "")

	// CNI DEL: the listener leaves the snapshot. The child is sent the removal
	// and acknowledges it.
	delete(published, testListener)
	sendDelta(tr, child, "c2", nil, []string{testListener})
	ackDelta(tr, child, "c2", "")
	requireAbsentNow(t, tr, testListener, "the DEL's wait returns here")

	// The draining parent acknowledges its opening response late, is sent the
	// removal, and exits before acknowledging it.
	ackDelta(tr, parent, "p1", "")
	sendDelta(tr, parent, "p2", nil, []string{testListener})
	tr.onDeltaStreamClosed(parent, nil)

	requireNotPresent(t, tr, testListener, "no live proxy holds the listener and the snapshot does not publish it")
	// Not only because nothing is published under the name: the tracker's own
	// last word is the removal, so a later DEL does not wait for an
	// acknowledgement nobody is left to give, and a pod re-added with the same
	// content waits for its own.
	requireAbsentNow(t, tr, testListener, "the acknowledged removal stands")
	published[testListener] = "h1"
	requireNotPresent(t, tr, testListener, "the same content published again has been acknowledged by nobody")
}

// TestServer_WildcardWithANamedUnsubscribeIsNotRead: a first request
// that subscribes to "*" and unsubscribes a name it states. go-control-plane
// deletes that name from the stated versions (Subscription.
// UpdateResourceSubscriptions), so the response neither adds nor removes it
// even though the snapshot does not publish it. The tracker reads nothing from
// a first request that unsubscribes from anything.
func TestServer_WildcardWithANamedUnsubscribeIsNotRead(t *testing.T) {
	s := startDeltaServer(t, testServerListener(otherListener, 2))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(s.conn).DeltaAggregatedResources(ctx)
	require.NoError(t, err)
	p := &proxyStream{t: t, stream: stream, cancel: cancel}
	require.NoError(t, stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		Node:                     &corev3.Node{Id: serverNodeID},
		TypeUrl:                  resourcev3.ListenerType,
		ResourceNamesSubscribe:   []string{"*"},
		ResourceNamesUnsubscribe: []string{testListener},
		InitialResourceVersions:  map[string]string{testListener: "anything"},
	}))
	resp, added := p.recv()
	require.Equal(t, []string{otherListener}, added)
	require.Empty(t, resp.GetRemovedResources(), "go-control-plane forgot the statement: it does not even remove the name")
	p.ack(resp)
	requirePresent(t, s.tracker, otherListener)
	requireNotPresent(t, s.tracker, testListener, "the snapshot does not publish it and the server never compared it")
	require.False(t, holds(s.tracker, testListener), "whatever is published later, the statement was never compared")
}

// TestServer_RemovedBeforeTheOpeningAck: stated, then removed by a later
// snapshot before the ACK of the opening response. On one stream the removal
// is not lost: the proxy does hold the listener until it acknowledges the
// removal, and the tracker says so at each step.
func TestServer_RemovedBeforeTheOpeningAck(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))
	p := s.open(t, map[string]string{
		testListener:  s.versions[testListener],
		otherListener: s.versions[otherListener],
	})
	opening, added := p.recv()
	require.Empty(t, added)

	s.publish(t, "v2", testServerListener(otherListener, 2))
	p.ack(opening)
	removal, _ := p.recv()
	require.Equal(t, []string{testListener}, removal.GetRemovedResources())
	// Nothing is published under the name any more, so no ADD wait can be
	// answered by it; the tracker still knows the proxy holds it.
	require.Eventually(t, func() bool { return holds(s.tracker, testListener) }, resolvedWait, time.Millisecond, "the proxy still holds it")
	requireNotPresent(t, s.tracker, testListener, "not at a published version")

	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, s.tracker.WaitListenerAbsent(ctx, testListener), "the removal is not acknowledged yet")

	p.ack(removal)
	ctx2, cancel2 := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel2()
	require.NoError(t, s.tracker.WaitListenerAbsent(ctx2, testListener))
}

// TestServer_SecondRequestBeforeTheFirstResponse: the
// proxy's opening request is held (no snapshot), a second request of the type
// arrives, and only then is the snapshot published. Nothing is concluded.
func TestServer_SecondRequestBeforeTheFirstResponse(t *testing.T) {
	versionsOf := startDeltaServer(t, testServerListener(testListener, 1)).versions
	s := newDeltaServer(t)
	p := s.open(t, map[string]string{testListener: versionsOf[testListener]})
	require.NoError(t, p.stream.Send(&discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ListenerType, ResourceNamesUnsubscribe: []string{"*"},
	}))
	require.Eventually(t, func() bool {
		s.tracker.mu.Lock()
		defer s.tracker.mu.Unlock()
		names, ok := s.tracker.stated[streamType{streamID: 1, typeURL: resourcev3.ListenerType}]
		return ok && names == nil
	}, resolvedWait, time.Millisecond)
	s.publish(t, "v1", testServerListener(testListener, 1))
	requireNotPresent(t, s.tracker, testListener)
}

// TestTracker_ConcurrentStreamsAndWaiters is for the race detector.
func TestTracker_ConcurrentStreamsAndWaiters(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1", otherListener: "h2"})
	tr.SetAckObserver(func(context.Context, string, string) {})
	var wg sync.WaitGroup
	for stream := int64(1); stream <= 8; stream++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				openDelta(tr, stream, resourcev3.ListenerType, map[string]string{testListener: "h1", otherListener: "h2"})
				openDelta(tr, stream, resourcev3.ClusterType, map[string]string{"c": "h"})
				sendDelta(tr, stream, "n1", nil, []string{otherListener})
				ackDelta(tr, stream, "n1", "")
				sendDelta(tr, stream, "n2", []string{otherListener}, nil)
				ackDelta(tr, stream, "n2", "boom")
				tr.onDeltaStreamClosed(stream, nil)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				_ = tr.WaitListenerPresent(ctx, testListener)
				_ = tr.WaitListenerAbsent(ctx, otherListener)
				cancel()
			}
		}()
	}
	wg.Wait()
}
