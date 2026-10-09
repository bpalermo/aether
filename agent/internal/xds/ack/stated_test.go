package ack

import (
	"context"
	"log/slog"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of #1511: a listener the proxy states it holds, in the opening
// request of the Listener type on a stream, at the version the agent
// publishes. go-control-plane sends nothing for it, so before #1511 a wait for
// it ran to its deadline.
//
// Every test that expects a wait NOT to resolve uses unresolvedWait, and every
// one that expects it to resolve uses a deadline far above anything a healthy
// run needs, so neither kind passes or fails on timing.

const (
	// resolvedWait bounds a wait that must succeed.
	resolvedWait = 10 * time.Second
	// unresolvedWait is how long a wait that must NOT resolve is given.
	unresolvedWait = 50 * time.Millisecond
)

// openDelta simulates the first request of a type on a stream: the one whose
// initial_resource_versions state what the proxy holds. No subscribe names is
// the wildcard subscription Envoy opens LDS and CDS with.
func openDelta(t *Tracker, streamID int64, typeURL string, stated map[string]string, subscribe ...string) {
	_ = t.onDeltaRequest(streamID, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                 typeURL,
		InitialResourceVersions: stated,
		ResourceNamesSubscribe:  subscribe,
	})
}

func requirePresent(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	require.NoError(t, tr.WaitListenerPresent(ctx, name), msgAndArgs...)
}

// requireNotPresent asserts the wait runs to its deadline: neither resolved
// nor failed by a NACK.
func requireNotPresent(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, name)
	require.Error(t, err, msgAndArgs...)
	require.Contains(t, err.Error(), "timed out", msgAndArgs...)
}

// TestWaitListenerPresent_ResolvedByTheOpeningExchange is #1511. The proxy
// states the listener; the server's first response of the type neither adds
// nor removes it, which is go-control-plane saying the stated version is the
// published one; the proxy acknowledges that response. Each step alone
// resolves nothing.
func TestWaitListenerPresent_ResolvedByTheOpeningExchange(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))

	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	requireNotPresent(t, tr, testListener, "a stated version has not been compared with anything yet")

	sendDelta(tr, 1, "n1", nil, nil)
	requireNotPresent(t, tr, testListener, "sent is not acknowledged")

	ackDelta(tr, 1, "n1", "")
	requirePresent(t, tr, testListener, "the proxy stated the listener and the server found nothing to change")
}

// TestWaitListenerPresent_ResolvedByTheOpeningExchangeWhileWaiting: the wait
// is already blocked when the proxy reconnects, which is the order a CNI ADD
// retried across an agent restart sees.
func TestWaitListenerPresent_ResolvedByTheOpeningExchangeWhileWaiting(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		done <- tr.WaitListenerPresent(ctx, testListener)
	}()

	// The opening response also carries a listener the proxy did not hold.
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", []string{"another"}, nil)
	ackDelta(tr, 1, "n1", "")

	require.NoError(t, <-done)
	requirePresent(t, tr, "another")
}

// TestOpeningExchange_ResolvesOnlyWhatTheProxyStated: a listener the proxy did
// not name is not resolved by an opening exchange that resolves another.
func TestOpeningExchange_ResolvesOnlyWhatTheProxyStated(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))

	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{"another": "h1"})
	sendDelta(tr, 1, "n1", nil, nil)
	ackDelta(tr, 1, "n1", "")

	requirePresent(t, tr, "another")
	requireNotPresent(t, tr, testListener, "the proxy never stated this listener")
}

// TestOpeningExchange_StatedAtAnotherVersionIsNotResolvedByTheStatement: when
// the stated version is not the published one (older or newer: the server
// only knows it differs), go-control-plane sends the listener. Then the
// statement proves nothing about the published version, and only the proxy's
// answer to that response does.
func TestOpeningExchange_StatedAtAnotherVersionIsNotResolvedByTheStatement(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "stale", "held": "h1"})
		sendDelta(tr, 1, "n1", []string{testListener}, nil)
		requireNotPresent(t, tr, testListener, "the published version is on the wire, not acknowledged")

		ackDelta(tr, 1, "n1", "Permission denied")
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		err := tr.WaitListenerPresent(ctx, testListener)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "envoy rejected config", "the proxy holds the version it stated, not the published one")

		// The listener the same request stated at the published version is
		// left unknown: a rejected opening response resolves nothing.
		requireNotPresent(t, tr, "held", "nothing is concluded from an opening response the proxy rejected")
	})

	t.Run("accepted", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "stale"})
		sendDelta(tr, 1, "n1", []string{testListener}, nil)
		ackDelta(tr, 1, "n1", "")
		requirePresent(t, tr, testListener, "the ACK of the response that carried it, as before #1511")
	})
}

// TestOpeningExchange_StatedButRemovedIsAbsent: a listener the proxy states
// but the snapshot no longer has is removed by the opening response.
func TestOpeningExchange_StatedButRemovedIsAbsent(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, []string{testListener})
	ackDelta(tr, 1, "n1", "")
	requireNotPresent(t, tr, testListener, "the opening response removed it")

	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	require.NoError(t, tr.WaitListenerAbsent(ctx, testListener))
}

// TestOpeningExchange_LaterNackStillSurfaces: resolving a listener from the
// opening exchange does not hide a later rejected update of it.
func TestOpeningExchange_LaterNackStillSurfaces(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, nil)
	ackDelta(tr, 1, "n1", "")
	requirePresent(t, tr, testListener)

	sendDelta(tr, 1, "n2", []string{testListener}, nil)
	ackDelta(tr, 1, "n2", "Permission denied")

	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Permission denied")
}

// TestOpeningExchange_ClearsAnEarlierNack: a proxy never states a version it
// rejected (//agent/test/mtlspool, TestReconnectingProxyStatesNoClusterItRejected),
// so a listener it states at the published version was accepted, whatever an
// earlier stream recorded against the name.
func TestOpeningExchange_ClearsAnEarlierNack(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	ackDelta(tr, 1, "n1", "Permission denied")
	tr.onDeltaStreamClosed(1, nil)

	openDelta(tr, 2, resourcev3.ListenerType, map[string]string{testListener: "h0"})
	sendDelta(tr, 2, "n1", nil, nil)
	ackDelta(tr, 2, "n1", "")
	requirePresent(t, tr, testListener)
}

// TestOpeningExchange_StreamResetBetween: what a stream's opening request
// stated dies with the stream. Another stream's first response, or the same
// nonce on another stream, resolves nothing for it.
func TestOpeningExchange_StreamResetBetween(t *testing.T) {
	t.Run("closed before the first response", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		tr.onDeltaStreamClosed(1, nil)

		// The next stream is a proxy that holds nothing (a new generation).
		openDelta(tr, 2, resourcev3.ListenerType, nil)
		sendDelta(tr, 2, "n1", nil, nil)
		ackDelta(tr, 2, "n1", "")
		requireNotPresent(t, tr, testListener, "the stream that stated it is gone")

		// A response on the closed stream's ID finds no statement either.
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})

	t.Run("closed before the acknowledgement", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n1", nil, nil)
		tr.onDeltaStreamClosed(1, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener, "the ACK never arrived on the stream that stated it")
	})

	t.Run("acknowledged on another stream", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n1", nil, nil)
		openDelta(tr, 2, resourcev3.ListenerType, nil)
		sendDelta(tr, 2, "n1", nil, nil)
		ackDelta(tr, 2, "n1", "")
		requireNotPresent(t, tr, testListener, "stream 2 stated nothing")
	})
}

// TestOpeningExchange_OnlyTheFirstRequestStates: go-control-plane reads
// initial_resource_versions from the first request of a type on a stream and
// from no other, so the tracker does the same. A later request that carries
// the field states nothing, whether it arrives after the first response or
// before it.
func TestOpeningExchange_OnlyTheFirstRequestStates(t *testing.T) {
	t.Run("after the first response", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")

		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		assert.Empty(t, tr.stated, "and is not kept")
		sendDelta(tr, 1, "n2", []string{"another"}, nil)
		ackDelta(tr, 1, "n2", "")
		requireNotPresent(t, tr, testListener, "not the opening request")
	})

	t.Run("a second request before the first response", func(t *testing.T) {
		// The server may answer the second request rather than the first, with
		// a subscription the second request changed: the tracker cannot tell
		// what was compared, so it concludes nothing.
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})

	t.Run("a second request that states it, before the first response", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})
}

// TestOpeningExchange_OnlyAWildcardSubscriptionIsCompared: for a subscription
// by name go-control-plane compares only the subscribed names, so a stated
// resource missing from the response may simply not have been looked at. The
// tracker reads the opening exchange only for the wildcard subscription, which
// is how Envoy subscribes to listeners.
func TestOpeningExchange_OnlyAWildcardSubscriptionIsCompared(t *testing.T) {
	t.Run("by name", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"}, "some-other-listener")
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})

	t.Run("explicit wildcard", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"}, "*")
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requirePresent(t, tr, testListener)
	})

	t.Run("wildcard subscribed and unsubscribed in one request", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		_ = tr.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
			TypeUrl:                  resourcev3.ListenerType,
			InitialResourceVersions:  map[string]string{testListener: "h1"},
			ResourceNamesSubscribe:   []string{"*"},
			ResourceNamesUnsubscribe: []string{"*"},
		})
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})
}

// TestOpeningExchange_IsPerType: a name stated for another type says nothing
// about the listener of that name, and another type's first response is not
// the Listener type's.
func TestOpeningExchange_IsPerType(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))

	// ackDelta's requests are typed as Listener requests; these are not.
	ackTyped := func(typeURL, nonce string) {
		_ = tr.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeURL, ResponseNonce: nonce})
	}

	openDelta(tr, 1, resourcev3.ClusterType, map[string]string{testListener: "h1"})
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", nil)
	ackTyped(resourcev3.ClusterType, "n1")
	requireNotPresent(t, tr, testListener, "a cluster of that name was stated, not a listener")

	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendVersioned(tr, 1, resourcev3.RouteType, "n2", "v1", nil)
	ackTyped(resourcev3.RouteType, "n2")
	requireNotPresent(t, tr, testListener, "the Listener type has not been answered")

	sendDelta(tr, 1, "n3", nil, nil)
	ackDelta(tr, 1, "n3", "")
	requirePresent(t, tr, testListener)
}

// TestWaitListenerAbsent_WaitsForAListenerTheProxyStated: before #1511 a
// restarted agent knew nothing of a listener the proxy still held, read that
// as absent, and returned from the removal wait at once. Now the wait is for
// the acknowledged removal, as it is for an agent that did not restart.
func TestWaitListenerAbsent_WaitsForAListenerTheProxyStated(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, nil)
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerAbsent(ctx, testListener), "the proxy holds it until it acknowledges the removal")

	sendDelta(tr, 1, "n2", nil, []string{testListener})
	ackDelta(tr, 1, "n2", "")
	ctx2, cancel2 := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel2()
	require.NoError(t, tr.WaitListenerAbsent(ctx2, testListener))
}
