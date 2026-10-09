package ack

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// The cases a second adversarial review of #1511 found, once presence was
// keyed by version. The first three failed then: with one state per name, an
// older answer could replace a newer one, and the last ACKNOWLEDGED version is
// not the last one SENT. The rest held and are kept as limits.

// TestTwoGenerations_AnOlderAckDoesNotEraseANewerOne: hot-restart overlap, no
// agent restart. Both generations are sent the listener at h1; the parent is
// slow to answer. The listener is republished at h2; the child, the generation
// taking over, is sent h2 and acknowledges it: the proxy that will serve holds
// the published version. The parent then acknowledges its OLDER response (h1)
// and exits before it is sent h2. The state is one per name and the last
// writer wins, whatever version and generation: it now says h1, and nothing
// will ever correct it, because the child already holds h2 and is sent nothing
// more. Every later ADD wait for the name (kubelet retry, re-add) runs to its
// deadline, where on main it returned at once.
func TestTwoGenerations_AnOlderAckDoesNotEraseANewerOne(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)

	openDelta(tr, parent, resourcev3.ListenerType, nil)
	sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, child, "c1", "")

	published[testListener] = "h2"
	sendListeners(tr, child, "c2", map[string]string{testListener: "h2"}, nil)
	ackDelta(tr, child, "c2", "")
	requirePresent(t, tr, testListener, "fixture: the child acknowledged the published version")

	// The draining parent answers its first response late and goes away.
	ackDelta(tr, parent, "p1", "")
	tr.onDeltaStreamClosed(parent, nil)

	requirePresent(t, tr, testListener, "the only live proxy acknowledged the published version; an older generation's ACK of an older version must not erase that")
}

// TestTwoGenerations_AnOlderAddDoesNotUndoAnAcknowledgedRemoval: the same rule
// on the removal side, which the sequence number guards for a STATEMENT only.
// The child acknowledges the removal (the DEL's wait returns); the parent's
// in-flight ACK of the earlier add arrives after it, and the parent exits
// before acknowledging its own removal. The listener is present for ever: a
// later DEL of a same-named pod waits out its deadline for a removal no proxy
// is left to acknowledge.
func TestTwoGenerations_AnOlderAddDoesNotUndoAnAcknowledgedRemoval(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)

	openDelta(tr, parent, resourcev3.ListenerType, nil)
	sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, child, "c1", "")

	delete(published, testListener)
	sendListeners(tr, child, "c2", nil, []string{testListener})
	ackDelta(tr, child, "c2", "")
	requireAbsentNow(t, tr, testListener, "fixture: the DEL's wait returns here")

	ackDelta(tr, parent, "p1", "")
	tr.onDeltaStreamClosed(parent, nil)

	requireAbsentNow(t, tr, testListener, "the acknowledged removal stands, as it does against a late statement")
}

// TestServer_ReturnToAnAcknowledgedVersionWaitsWhileANewerOneIsUnanswered: the
// proxy acknowledged v1. v2 is published and SENT (not answered). v1 is
// published again. The wait compares the last acknowledged version with the
// published one and returns at once, while the last thing the proxy was sent
// is v2 and the server has yet to send v1 again. Real go-control-plane server.
func TestServer_ReturnToAnAcknowledgedVersionWaitsWhileANewerOneIsUnanswered(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, nil)
	first, _ := p.recv()
	p.ack(first)
	requirePresent(t, s.tracker, testListener)

	s.publish(t, "v2", testServerListener(testListener, 2))
	second, added := p.recv()
	require.Equal(t, []string{testListener}, added)

	s.publish(t, "v3", testServerListener(testListener, 1))
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	err := s.tracker.WaitListenerPresent(ctx, testListener)

	// What the proxy is in fact still owed: the published version again.
	p.ack(second)
	third, added := p.recv()
	require.Equal(t, []string{testListener}, added, "the server has to send the published version again")
	require.Equal(t, s.versions[testListener], third.GetResources()[0].GetVersion())

	require.Error(t, err, "the wait returned while the proxy had been sent another version and not yet the published one again")
}

// TestServer_RejectionThenIdenticalRepublish: a rejected listener that is
// republished with the same bytes is never sent again on the stream, so the
// wait keeps failing with the rejection, as on main; a stream reset has the
// listener sent again and its ACK clears it.
func TestServer_RejectionThenIdenticalRepublish(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, nil)
	first, _ := p.recv()
	p.nack(first, "bind failed")

	fails := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
		defer cancel()
		err := s.tracker.WaitListenerPresent(ctx, testListener)
		return err != nil && !isTimeout(err)
	}
	require.Eventually(t, fails, resolvedWait, time.Millisecond)
	s.publish(t, "v2", testServerListener(testListener, 1))
	require.True(t, fails(), "identical bytes: nothing is sent again, the rejection stands")

	p.cancel()
	require.Eventually(t, func() bool {
		s.tracker.mu.Lock()
		defer s.tracker.mu.Unlock()
		return len(s.tracker.streams) == 0
	}, resolvedWait, time.Millisecond)
	p2 := s.open(t, nil)
	again, added := p2.recv()
	require.Equal(t, []string{testListener}, added)
	p2.ack(again)
	// A recorded rejection fails a wait at once, so poll for the ACK.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
		defer cancel()
		return s.tracker.WaitListenerPresent(ctx, testListener) == nil
	}, resolvedWait, time.Millisecond)
}

// TestServer_OlderRejectionWithThePublishedVersionInFlight: v1 is sent,
// v2 published, v1 rejected. The wait is not failed by the rejection of v1;
// v2 is sent and its ACK resolves it.
func TestServer_OlderRejectionWithThePublishedVersionInFlight(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1))
	p := s.open(t, nil)
	first, _ := p.recv()
	s.publish(t, "v2", testServerListener(testListener, 2))
	p.nack(first, "bind failed")
	second, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	requireNotPresent(t, s.tracker, testListener, "neither resolved nor failed by the older rejection")
	p.ack(second)
	requirePresent(t, s.tracker, testListener)
}

// TestServer_RemovalAckWithTheNameRepublishedInBetween: a removal is sent,
// the name is published again at another version before the removal is
// acknowledged. The removal wait returns on the removal's ACK; the presence
// wait only on the ACK of the new version.
func TestServer_RemovalAckWithTheNameRepublishedInBetween(t *testing.T) {
	s := startDeltaServer(t, testServerListener(testListener, 1), testServerListener(otherListener, 2))
	p := s.open(t, nil)
	first, _ := p.recv()
	p.ack(first)
	requirePresent(t, s.tracker, testListener)

	s.publish(t, "v2", testServerListener(otherListener, 2))
	removal, _ := p.recv()
	require.Equal(t, []string{testListener}, removal.GetRemovedResources())
	s.publish(t, "v3", testServerListener(testListener, 9), testServerListener(otherListener, 2))
	requireNotPresent(t, s.tracker, testListener)

	p.ack(removal)
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	require.NoError(t, s.tracker.WaitListenerAbsent(ctx, testListener))
	readd, added := p.recv()
	require.Equal(t, []string{testListener}, added)
	requireNotPresent(t, s.tracker, testListener)
	p.ack(readd)
	requirePresent(t, s.tracker, testListener)
}

// TestTwoGenerations_ARejectionIsAlwaysRecorded: the rule that an older answer
// does not replace a newer one is for acknowledgements only. A rejection is
// recorded whenever it arrives: with one state per name the tracker cannot say
// which generation will serve, and a wait that fails loudly with a proxy's
// error is the safe reading of "one of them refused this version".
func TestTwoGenerations_ARejectionIsAlwaysRecorded(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1"})
	const parent, child = int64(1), int64(2)

	openDelta(tr, parent, resourcev3.ListenerType, nil)
	sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, child, "c1", "")
	requirePresent(t, tr, testListener)

	ackDelta(tr, parent, "p1", "Permission denied")
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Permission denied")

	// And an older acknowledgement does not clear it.
	sendListeners(tr, parent, "p2", map[string]string{testListener: "h1"}, nil)
	openDelta(tr, 3, resourcev3.ListenerType, nil)
	sendListeners(tr, 3, "g1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, parent, "p2", "Permission denied")
	ackDelta(tr, 3, "g1", "")
	ctx2, cancel2 := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel2()
	require.Error(t, tr.WaitListenerPresent(ctx2, testListener), "the acknowledgement answers a response sent before the rejection")

	// One that answers a response sent after it does.
	sendListeners(tr, 3, "g2", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 3, "g2", "")
	requirePresent(t, tr, testListener)
}

// TestTwoGenerations_AnOlderRejectionDoesNotReplaceANewerOne: one rejection is
// kept per name. The generation taking over rejects the published version;
// the draining one then rejects the older version it had been sent before.
// The rejection that fails the wait for what is published must stay.
func TestTwoGenerations_AnOlderRejectionDoesNotReplaceANewerOne(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)

	openDelta(tr, parent, resourcev3.ListenerType, nil)
	sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)

	published[testListener] = "h2"
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
	ackDelta(tr, child, "c1", "h2 refused")
	ackDelta(tr, parent, "p1", "h1 refused")

	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	require.Contains(t, err.Error(), "h2 refused", "the rejection of the published version is the one on record")

	// A rejection of a response sent after it does replace it.
	published[testListener] = "h3"
	sendListeners(tr, child, "c2", map[string]string{testListener: "h3"}, nil)
	ackDelta(tr, child, "c2", "h3 refused")
	ctx2, cancel2 := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel2()
	err = tr.WaitListenerPresent(ctx2, testListener)
	require.Error(t, err)
	require.Contains(t, err.Error(), "h3 refused")
}

// TestTwoGenerations_AnOlderRemovalDoesNotUndoANewerAdd: the mirror of the
// older add. Both generations are sent the removal; the one taking over
// acknowledges it and then the listener published again; the other's ACK of
// the removal arrives last.
func TestTwoGenerations_AnOlderRemovalDoesNotUndoANewerAdd(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)

	for _, stream := range []int64{parent, child} {
		openDelta(tr, stream, resourcev3.ListenerType, nil)
		sendListeners(tr, stream, "a", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, stream, "a", "")
	}
	sendListeners(tr, parent, "r", nil, []string{testListener})
	sendListeners(tr, child, "r", nil, []string{testListener})
	ackDelta(tr, child, "r", "")
	requireAbsentNow(t, tr, testListener)

	published[testListener] = "h3"
	sendListeners(tr, child, "b", map[string]string{testListener: "h3"}, nil)
	ackDelta(tr, child, "b", "")
	requirePresent(t, tr, testListener)

	ackDelta(tr, parent, "r", "")
	requirePresent(t, tr, testListener, "the generation taking over holds the published version")
}

// TestWaitListenerPresent_UnansweredOnTheStreamThatAcknowledged: what makes
// the acknowledged version unknown is an unanswered response on the stream
// that acknowledged it. Another generation's unanswered response does not: a
// draining proxy that is slow to answer must not hold up every pod ADD of a
// hot restart.
func TestWaitListenerPresent_UnansweredOnTheStreamThatAcknowledged(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)

	openDelta(tr, parent, resourcev3.ListenerType, nil)
	sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, child, "c1", "")
	requirePresent(t, tr, testListener, "the parent has not answered; the child has")

	// The child is sent a removal and does not answer; the same version is
	// published again. The child may have removed the listener.
	sendListeners(tr, child, "c2", nil, []string{testListener})
	requireNotPresent(t, tr, testListener, "the stream that acknowledged it has an unanswered removal")
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerAbsent(ctx, testListener), "and the removal is not acknowledged either")

	// Another type's unanswered response on that stream is not about the listener.
	ackDelta(tr, child, "c2", "")
	sendListeners(tr, child, "c3", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, child, "c3", "")
	sendVersioned(tr, child, resourcev3.ClusterType, "c4", "v", []string{testListener})
	requirePresent(t, tr, testListener, "a cluster of that name is in flight, not the listener")
}

// TestStreamClose_ForgetsTheVersionOfWhatItLeftUnanswered: a stream that
// closes with an update of a listener unanswered leaves the proxy on one of
// two versions, and the tracker does not know which. The listener stays
// present for the removal wait and is at no version for the presence wait,
// even when the agent goes back to the version last acknowledged. The proxy
// says which it holds when it reconnects.
func TestStreamClose_ForgetsTheVersionOfWhatItLeftUnanswered(t *testing.T) {
	published := map[string]string{testListener: "h1", otherListener: "o1"}
	tr := publishing(published)
	openDelta(tr, 1, resourcev3.ListenerType, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1", otherListener: "o1"}, nil)
	ackDelta(tr, 1, "n1", "")

	published[testListener] = "h2"
	sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
	tr.onDeltaStreamClosed(1, nil)

	published[testListener] = "h1"
	requireNotPresent(t, tr, testListener, "the proxy may have applied the update it never answered")
	requirePresent(t, tr, otherListener, "the unanswered response did not carry this one")
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerAbsent(ctx, testListener), "the proxy holds one version or the other")

	// It reconnects holding the update, and is sent the published version.
	openDelta(tr, 2, resourcev3.ListenerType, map[string]string{testListener: "h2", otherListener: "o1"})
	sendListeners(tr, 2, "n1", map[string]string{testListener: "h1"}, nil)
	requireNotPresent(t, tr, testListener)
	ackDelta(tr, 2, "n1", "")
	requirePresent(t, tr, testListener)
}

// TestStreamClose_AnUnansweredAddMayHaveBeenApplied: a stream closes with the
// add of a listener unanswered. The proxy may have applied it before the
// answer was lost, so the listener is not known to be absent any more: a DEL
// waits for its removal. That holds when this stream had acknowledged its
// removal before, and when nothing at all was known of it. It does not hold
// against another generation's acknowledged removal: that one is newer than
// the add the closing stream never answered.
func TestStreamClose_AnUnansweredAddMayHaveBeenApplied(t *testing.T) {
	absentWaits := func(t *testing.T, tr *Tracker, msg string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
		defer cancel()
		require.Error(t, tr.WaitListenerAbsent(ctx, testListener), msg)
	}

	t.Run("after a removal this stream acknowledged", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, 1, "n1", "")
		sendListeners(tr, 1, "n2", nil, []string{testListener})
		ackDelta(tr, 1, "n2", "")
		requireAbsentNow(t, tr, testListener)

		sendListeners(tr, 1, "n3", map[string]string{testListener: "h1"}, nil)
		tr.onDeltaStreamClosed(1, nil)
		absentWaits(t, tr, "the proxy may hold the listener it was sent again")
		requireNotPresent(t, tr, testListener, "and may not: it is at no known version")

		// It reconnects and says it applied it.
		openDelta(tr, 2, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 2, "n1", nil, nil)
		ackDelta(tr, 2, "n1", "")
		requirePresent(t, tr, testListener)
	})

	t.Run("of a listener nothing was known of", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
		tr.onDeltaStreamClosed(1, nil)
		absentWaits(t, tr, "the proxy may hold the listener it was sent")

		// It reconnects and says it does not: the opening response sends it
		// again, and nothing but the answer to that says it is there.
		openDelta(tr, 2, resourcev3.ListenerType, nil)
		sendListeners(tr, 2, "n1", map[string]string{testListener: "h1"}, nil)
		requireNotPresent(t, tr, testListener)
		ackDelta(tr, 2, "n1", "")
		requirePresent(t, tr, testListener)
	})

	t.Run("an unanswered removal does not make a listener present", func(t *testing.T) {
		// The proxy rejected the listener, so it does not hold it; the
		// server, which counts what it sent as delivered, sends its removal
		// when the pod goes, and the stream closes before the answer.
		tr := publishing(map[string]string{})
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, 1, "n1", "Permission denied")
		sendListeners(tr, 1, "n2", nil, []string{testListener})
		tr.onDeltaStreamClosed(1, nil)
		require.False(t, holds(tr, testListener), "nothing the proxy was sent can have made it hold the listener")
	})

	t.Run("not against another generation's acknowledged removal", func(t *testing.T) {
		tr := publishing(map[string]string{})
		const parent, child = int64(1), int64(2)
		openDelta(tr, parent, resourcev3.ListenerType, nil)
		sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "")
		sendListeners(tr, child, "c2", nil, []string{testListener})
		ackDelta(tr, child, "c2", "")

		tr.onDeltaStreamClosed(parent, nil)
		requireAbsentNow(t, tr, testListener, "the generation that stays acknowledged the removal")
	})
}

// TestStreamClose_KeepsWhatAnotherStreamAcknowledged: the unanswered response
// of a generation that goes away says nothing about the listener the other
// generation acknowledged.
func TestStreamClose_KeepsWhatAnotherStreamAcknowledged(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const parent, child = int64(1), int64(2)
	openDelta(tr, parent, resourcev3.ListenerType, nil)
	sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
	// The child is sent, and acknowledges, a newer version than the one the
	// parent leaves unanswered.
	published[testListener] = "h2"
	openDelta(tr, child, resourcev3.ListenerType, nil)
	sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
	ackDelta(tr, child, "c1", "")

	tr.onDeltaStreamClosed(parent, nil)
	requirePresent(t, tr, testListener)
}

// TestStreamClose_WakesAWaiterItUnblocks: a wait held up only by a stream's
// unanswered response is looked at again when that stream closes, not at its
// deadline.
func TestStreamClose_WakesAWaiterItUnblocks(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1"})
	openDelta(tr, 1, resourcev3.ListenerType, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "")
	// The same version again (go-control-plane resends after a rejection of
	// another): closing the stream leaves the proxy on h1 either way.
	sendListeners(tr, 1, "n2", map[string]string{testListener: "h1"}, nil)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		done <- tr.WaitListenerPresent(ctx, testListener)
	}()
	requireNotPresent(t, tr, testListener, "fixture: the unanswered response holds the wait")
	tr.onDeltaStreamClosed(1, nil)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(resolvedWait / 2):
		t.Fatal("the waiter was not woken by the stream closing")
	}
}

// TestTwoGenerations_ANewerResponseAnsweredLastStands: the order that decides
// between two answers is the order their RESPONSES were sent in, not the order
// the answers arrived in. The draining generation is sent h1; h2 is published
// and the generation taking over is sent h2 before either has answered. The
// one that answers second was sent the newer version: its answer stands,
// whether it accepts or rejects.
func TestTwoGenerations_ANewerResponseAnsweredLastStands(t *testing.T) {
	const parent, child = int64(1), int64(2)
	sent := func(t *testing.T) (*Tracker, map[string]string) {
		t.Helper()
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		openDelta(tr, parent, resourcev3.ListenerType, nil)
		sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
		published[testListener] = "h2"
		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
		return tr, published
	}
	failsWith := func(t *testing.T, tr *Tracker, msg string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		err := tr.WaitListenerPresent(ctx, testListener)
		require.Error(t, err)
		require.Contains(t, err.Error(), msg)
	}

	t.Run("both accept", func(t *testing.T) {
		tr, _ := sent(t)
		ackDelta(tr, parent, "p1", "")
		ackDelta(tr, child, "c1", "")
		requirePresent(t, tr, testListener, "the generation taking over acknowledged the published version, last")
	})
	t.Run("both accept, the newer first", func(t *testing.T) {
		tr, _ := sent(t)
		ackDelta(tr, child, "c1", "")
		ackDelta(tr, parent, "p1", "")
		requirePresent(t, tr, testListener)
	})
	t.Run("both reject", func(t *testing.T) {
		tr, _ := sent(t)
		ackDelta(tr, parent, "p1", "h1 refused")
		ackDelta(tr, child, "c1", "h2 refused")
		failsWith(t, tr, "h2 refused")
	})
	t.Run("both reject, with another response sent before either answers", func(t *testing.T) {
		// What orders two rejections is when their responses were sent, not
		// how much else was sent before the first of them arrived.
		tr, _ := sent(t)
		sendVersioned(tr, child, resourcev3.ClusterType, "c9", "v", []string{"a-cluster"})
		ackDelta(tr, parent, "p1", "h1 refused")
		ackDelta(tr, child, "c1", "h2 refused")
		failsWith(t, tr, "h2 refused")
	})
	t.Run("both reject, the newer first", func(t *testing.T) {
		tr, _ := sent(t)
		ackDelta(tr, child, "c1", "h2 refused")
		ackDelta(tr, parent, "p1", "h1 refused")
		failsWith(t, tr, "h2 refused")
	})
	t.Run("the older accepts, then the newer rejects", func(t *testing.T) {
		tr, _ := sent(t)
		ackDelta(tr, parent, "p1", "")
		ackDelta(tr, child, "c1", "h2 refused")
		failsWith(t, tr, "h2 refused")
	})
	t.Run("the older rejects, then the newer accepts", func(t *testing.T) {
		// The acceptance is of a response sent before the rejection arrived,
		// but the rejection is of another version: it does not fail a wait
		// for the published one.
		tr, _ := sent(t)
		ackDelta(tr, parent, "p1", "h1 refused")
		ackDelta(tr, child, "c1", "")
		requirePresent(t, tr, testListener)
	})
}

// TestWait_StateAndPublishedVersionAreReadTogether: the wait reads the state
// under the tracker's lock and the published version outside it. If the state
// changes in between, the two were never true together and the wait looks
// again instead of answering from the pair.
func TestWait_StateAndPublishedVersionAreReadTogether(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	asked := 0
	tr.SetPublishedVersion(func(string, string) (string, bool) {
		asked++
		if asked == 1 {
			// Between the wait's read of the state (h1) and this answer, the
			// proxy is sent and acknowledges h2, and the agent goes back to
			// publishing h1.
			sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
			ackDelta(tr, 1, "n2", "")
		}
		return "h1", true
	})
	openDelta(tr, 1, resourcev3.ListenerType, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener), "the proxy holds h2 by the time h1 is read as published")
	require.GreaterOrEqual(t, asked, 2, "the wait looked again")
}

// TestWait_AResponseSentWhileItLooksIsSeen: the same for a response that
// enters flight between the wait's two reads. The proxy is sent another
// version while the published one is being read; the agent goes back to the
// first. The acknowledged version is the published one again, and it is not
// what the proxy was last sent.
func TestWait_AResponseSentWhileItLooksIsSeen(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	asked := 0
	tr.SetPublishedVersion(func(string, string) (string, bool) {
		asked++
		if asked == 1 {
			sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
		}
		return "h1", true
	})
	openDelta(tr, 1, resourcev3.ListenerType, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener), "the proxy has been sent h2 and not answered")
}

// TestStreamClose_AnUnansweredAddIsOrderedLikeAnAnswer: what a closing stream
// leaves uncertain is ordered like any answer, by when its response was sent.
// One generation acknowledges the removal; the other is sent the listener
// again LATER and closes without answering: it may hold it, whatever the
// first said before. And an older add acknowledged late does not make the
// uncertain listener certain.
func TestStreamClose_AnUnansweredAddIsOrderedLikeAnAnswer(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	const one, other = int64(1), int64(2)

	openDelta(tr, one, resourcev3.ListenerType, nil)
	sendListeners(tr, one, "a1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, one, "a1", "")
	sendListeners(tr, one, "a2", nil, []string{testListener})
	ackDelta(tr, one, "a2", "")
	requireAbsentNow(t, tr, testListener)

	// A response to a third stream, sent after the removal was acknowledged
	// and before the add below, and answered last.
	openDelta(tr, 3, resourcev3.ListenerType, nil)
	sendListeners(tr, 3, "old", map[string]string{testListener: "h1"}, nil)

	openDelta(tr, other, resourcev3.ListenerType, nil)
	sendListeners(tr, other, "b1", map[string]string{testListener: "h1"}, nil)
	tr.onDeltaStreamClosed(other, nil)

	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerAbsent(ctx, testListener), "the proxy that was sent the listener last may hold it")
	requireNotPresent(t, tr, testListener)

	ackDelta(tr, 3, "old", "")
	requireNotPresent(t, tr, testListener, "an answer to a response sent before the unanswered one settles nothing")
}

// TestStreamClose_TellsTheDeliveryObserverOutsideTheLock: a closing stream
// wakes the waiters and tells the DeliveryObserver what left flight. The
// observer is called with the tracker's lock released, so it can ask the
// tracker something itself.
func TestStreamClose_TellsTheDeliveryObserverOutsideTheLock(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1"})
	ended := 0
	tr.SetDeliveryObserver(func(_ context.Context, delivery Delivery) {
		if !delivery.Ended {
			return
		}
		ended++
		ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
		defer cancel()
		_ = tr.WaitListenerAbsent(ctx, "never-present")
	})
	openDelta(tr, 1, resourcev3.ListenerType, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)

	done := make(chan struct{})
	go func() {
		tr.onDeltaStreamClosed(1, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(resolvedWait):
		t.Fatal("the stream close never returned: the observer was called under the tracker's lock")
	}
	require.Equal(t, 1, ended)
}

func isTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "timed out")
}
