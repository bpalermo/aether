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

// publishing returns a tracker wired, as the node agent's is, to the versions
// of the listeners the agent publishes. The test changes the map to publish.
func publishing(listeners map[string]string) *Tracker {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	tr.SetPublishedVersion(func(typeURL, name string) (string, bool) {
		if typeURL != resourcev3.ListenerType {
			return "", false
		}
		version, ok := listeners[name]
		return version, ok
	})
	return tr
}

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

// sendListeners is sendDelta with the version of each listener it adds, as
// every real delta response carries it.
func sendListeners(t *Tracker, streamID int64, nonce string, added map[string]string, removed []string) {
	resp := &discoveryv3.DeltaDiscoveryResponse{
		TypeUrl:          resourcev3.ListenerType,
		Nonce:            nonce,
		RemovedResources: removed,
	}
	for name, version := range added {
		resp.Resources = append(resp.Resources, &discoveryv3.Resource{Name: name, Version: version})
	}
	t.onDeltaResponse(streamID, nil, resp)
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

// requireAbsentNow asserts WaitListenerAbsent returns at once.
func requireAbsentNow(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.NoError(t, tr.WaitListenerAbsent(ctx, name), msgAndArgs...)
}

// TestWaitListenerPresent_ResolvedByTheOpeningExchange is #1511. The proxy
// states the listener; the server's first response of the type neither adds
// nor removes it, which is go-control-plane saying the stated version is the
// published one; the proxy acknowledges that response. Each step alone
// resolves nothing.
func TestWaitListenerPresent_ResolvedByTheOpeningExchange(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1"})

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
	tr := publishing(map[string]string{testListener: "h1", "another": "a1"})

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		done <- tr.WaitListenerPresent(ctx, testListener)
	}()

	// The opening response also carries a listener the proxy did not hold.
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendListeners(tr, 1, "n1", map[string]string{"another": "a1"}, nil)
	ackDelta(tr, 1, "n1", "")

	require.NoError(t, <-done)
	requirePresent(t, tr, "another")
}

// TestOpeningExchange_NothingIsReadWithoutThePublishedVersion: a statement is
// about one version, and without the published one to hold it to (a tracker
// nobody called SetPublishedVersion on) no wait is resolved by it. It is still
// told to the AckObserver, which holds it to what it knows itself (#1508).
func TestOpeningExchange_NothingIsReadWithoutThePublishedVersion(t *testing.T) {
	tr, told := observe(t)
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, nil)
	for _, entry := range tr.inflight {
		assert.Nil(t, entry.held, "nothing is kept to resolve a wait with")
	}
	ackDelta(tr, 1, "n1", "")
	require.Len(t, *told, 1)
	assert.Equal(t, map[string]string{testListener: "h1"}, (*told)[0].Stated)
	requireNotPresent(t, tr, testListener)
}

// TestOpeningExchange_ResolvesOnlyWhatTheProxyStated: a listener the proxy did
// not name is not resolved by an opening exchange that resolves another.
func TestOpeningExchange_ResolvesOnlyWhatTheProxyStated(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1", "another": "h1"})

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
		tr := publishing(map[string]string{testListener: "h2", "held": "h1"})
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "stale", "held": "h1"})
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h2"}, nil)
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
		tr := publishing(map[string]string{testListener: "h2"})
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "stale"})
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, 1, "n1", "")
		requirePresent(t, tr, testListener, "the ACK of the response that carried it, as before #1511")
	})
}

// TestOpeningExchange_StatedButRemovedIsAbsent: a listener the proxy states
// but the snapshot no longer has is removed by the opening response.
func TestOpeningExchange_StatedButRemovedIsAbsent(t *testing.T) {
	// Published again by the time anyone looks, at the very version stated:
	// only the acknowledged removal says the proxy does not hold it.
	tr := publishing(map[string]string{testListener: "h1"})
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, []string{testListener})
	ackDelta(tr, 1, "n1", "")
	requireNotPresent(t, tr, testListener, "the opening response removed it")
	requireAbsentNow(t, tr, testListener)
}

// TestOpeningExchange_LaterNackStillSurfaces: resolving a listener from the
// opening exchange does not hide a later rejected update of it.
func TestOpeningExchange_LaterNackStillSurfaces(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, nil)
	ackDelta(tr, 1, "n1", "")
	requirePresent(t, tr, testListener)

	published[testListener] = "h2"
	sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
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
	published := map[string]string{testListener: "h9"}
	tr := publishing(published)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h9"}, nil)
	ackDelta(tr, 1, "n1", "Permission denied")
	tr.onDeltaStreamClosed(1, nil)

	// The agent goes back to the version the proxy holds.
	published[testListener] = "h0"
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
		tr := publishing(map[string]string{testListener: "h1"})
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
		tr := publishing(map[string]string{testListener: "h1"})
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n1", nil, nil)
		tr.onDeltaStreamClosed(1, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener, "the ACK never arrived on the stream that stated it")
	})

	t.Run("acknowledged on another stream", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
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
	published := func() map[string]string { return map[string]string{testListener: "h1", "another": "a1"} }

	t.Run("after the first response", func(t *testing.T) {
		tr := publishing(published())
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")

		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		assert.Nil(t, tr.streams[streamType{streamID: 1, typeURL: resourcev3.ListenerType}].stated, "and is not kept")
		sendListeners(tr, 1, "n2", map[string]string{"another": "a1"}, nil)
		ackDelta(tr, 1, "n2", "")
		requireNotPresent(t, tr, testListener, "not the opening request")
	})

	t.Run("a second request before the first response", func(t *testing.T) {
		// The server may answer the second request rather than the first, with
		// a subscription the second request changed: the tracker cannot tell
		// what was compared, so it concludes nothing.
		tr := publishing(published())
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})

	t.Run("a second request that states it, before the first response", func(t *testing.T) {
		tr := publishing(published())
		openDelta(tr, 1, resourcev3.ListenerType, nil)
		openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener)
	})
}

// TestOpeningExchange_OnlyAWildcardSubscriptionIsCompared: for a subscription
// by name go-control-plane compares only the subscribed names, and it forgets
// the stated version of a name the opening request unsubscribes from, so a
// stated resource missing from the response may simply not have been looked
// at. The tracker reads the opening exchange only for the plain wildcard
// subscription, which is how Envoy subscribes to listeners.
func TestOpeningExchange_OnlyAWildcardSubscriptionIsCompared(t *testing.T) {
	open := func(tr *Tracker, subscribe, unsubscribe []string) {
		_ = tr.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
			TypeUrl:                  resourcev3.ListenerType,
			InitialResourceVersions:  map[string]string{testListener: "h1"},
			ResourceNamesSubscribe:   subscribe,
			ResourceNamesUnsubscribe: unsubscribe,
		})
		sendDelta(tr, 1, "n1", nil, nil)
		ackDelta(tr, 1, "n1", "")
	}

	for name, tc := range map[string]struct {
		subscribe, unsubscribe []string
		read                   bool
	}{
		"legacy wildcard":                                {nil, nil, true},
		"explicit wildcard":                              {[]string{"*"}, nil, true},
		"explicit wildcard and a name":                   {[]string{"*", "some-other-listener"}, nil, true},
		"by name":                                        {[]string{"some-other-listener"}, nil, false},
		"wildcard subscribed and unsubscribed":           {[]string{"*"}, []string{"*"}, false},
		"wildcard, unsubscribing the stated name":        {[]string{"*"}, []string{testListener}, false},
		"legacy wildcard, unsubscribing the stated name": {nil, []string{testListener}, false},
		"wildcard, unsubscribing another name":           {[]string{"*"}, []string{"some-other-listener"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			tr := publishing(map[string]string{testListener: "h1"})
			open(tr, tc.subscribe, tc.unsubscribe)
			if tc.read {
				requirePresent(t, tr, testListener)
			} else {
				requireNotPresent(t, tr, testListener)
			}
		})
	}
}

// TestOpeningExchange_IsPerType: a name stated for another type says nothing
// about the listener of that name, and another type's first response is not
// the Listener type's.
func TestOpeningExchange_IsPerType(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	// The same name and version are published as a cluster too.
	tr.SetPublishedVersion(func(string, string) (string, bool) { return "h1", true })

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

// TestWaitListenerPresent_IsForThePublishedVersion: a pod's listener is named
// after the pod, so a same-named replacement publishes other content under a
// name the proxy already holds. What the proxy acknowledged or stated for the
// name before says nothing about the replacement: its wait is for the
// acknowledgement of the response that carries the published version. This is
// true of an agent that did not restart as much as of one that did.
func TestWaitListenerPresent_IsForThePublishedVersion(t *testing.T) {
	for name, hold := range map[string]func(tr *Tracker){
		"acknowledged earlier": func(tr *Tracker) {
			openDelta(tr, 1, resourcev3.ListenerType, nil)
			sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
			ackDelta(tr, 1, "n1", "")
		},
		"stated": func(tr *Tracker) {
			openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
			sendDelta(tr, 1, "n1", nil, nil)
			ackDelta(tr, 1, "n1", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			published := map[string]string{testListener: "h1"}
			tr := publishing(published)
			hold(tr)
			requirePresent(t, tr, testListener, "fixture: the proxy holds the published version")

			// The replacement is published. Nothing has been sent yet.
			published[testListener] = "h2"
			requireNotPresent(t, tr, testListener, "the proxy holds the listener of the pod that is gone")

			sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
			requireNotPresent(t, tr, testListener, "sent is not acknowledged")

			// The stream ends with the update in flight: still not the
			// published version, on no stream at all.
			tr.onDeltaStreamClosed(1, nil)
			requireNotPresent(t, tr, testListener)

			// The proxy reconnects, states what it holds, and is sent the
			// update again.
			openDelta(tr, 2, resourcev3.ListenerType, map[string]string{testListener: "h1"})
			sendListeners(tr, 2, "n1", map[string]string{testListener: "h2"}, nil)
			requireNotPresent(t, tr, testListener)
			ackDelta(tr, 2, "n1", "")
			requirePresent(t, tr, testListener, "the proxy acknowledged the published version")

			// And a removal still needs its own acknowledgement.
			delete(published, testListener)
			requireNotPresent(t, tr, testListener, "nothing is published under the name")
			ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
			defer cancel()
			require.Error(t, tr.WaitListenerAbsent(ctx, testListener), "the proxy holds it until it acknowledges the removal")
		})
	}
}

// TestWaitListenerAbsent_WaitsForAListenerTheProxyStated: before #1511 a
// restarted agent knew nothing of a listener the proxy still held, read that
// as absent, and returned from the removal wait at once. Now the wait is for
// the acknowledged removal, as it is for an agent that did not restart, from
// the moment the opening response is acknowledged.
func TestWaitListenerAbsent_WaitsForAListenerTheProxyStated(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	openDelta(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendDelta(tr, 1, "n1", nil, nil)

	// The limit: until the opening response is acknowledged the tracker
	// knows nothing of the listener, and unknown reads as absent. A removal
	// waited for in that window (or before any proxy has connected) is not
	// waited for, as before #1511.
	requireAbsentNow(t, tr, testListener, "stated and compared, not yet acknowledged: still unknown")

	ackDelta(tr, 1, "n1", "")

	// The removal wait does not depend on what is published: the pod is gone
	// from the snapshot by the time a DEL waits.
	delete(published, testListener)
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	require.Error(t, tr.WaitListenerAbsent(ctx, testListener), "the proxy holds it until it acknowledges the removal")

	sendDelta(tr, 1, "n2", nil, []string{testListener})
	ackDelta(tr, 1, "n2", "")
	ctx2, cancel2 := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel2()
	require.NoError(t, tr.WaitListenerAbsent(ctx2, testListener))
}

// TestOpeningExchange_DoesNotOverwriteWhatWasSaidAfterItWasSent: two proxy
// generations are connected (a hot restart) and the tracker keeps one state
// per name. What the old generation stated was compared when its opening
// response was sent; anything either generation acknowledged or rejected
// after that is newer, and the old generation's late ACK of its opening
// response must not replace it.
func TestOpeningExchange_DoesNotOverwriteWhatWasSaidAfterItWasSent(t *testing.T) {
	const parent, child = int64(1), int64(2)

	t.Run("a rejection", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		openDelta(tr, parent, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, nil)

		// The new generation is sent the listener and rejects it (its bind
		// in the pod's netns failed).
		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "Permission denied")

		ackDelta(tr, parent, "p1", "")
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		err := tr.WaitListenerPresent(ctx, testListener)
		require.Error(t, err, "the generation that is taking over rejected the listener")
		assert.Contains(t, err.Error(), "Permission denied")
	})

	t.Run("a rejection of the stated version, whenever it was", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "Permission denied")

		// The old generation reconnects afterwards, holding that version.
		openDelta(tr, parent, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, nil)
		ackDelta(tr, parent, "p1", "")
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		err := tr.WaitListenerPresent(ctx, testListener)
		require.Error(t, err, "one generation holding the version does not make the other accept it")
		assert.Contains(t, err.Error(), "Permission denied")
	})

	t.Run("a rejection of a newer version", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		openDelta(tr, parent, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, nil)

		published[testListener] = "h2"
		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, child, "c1", "Permission denied")

		ackDelta(tr, parent, "p1", "")
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		err := tr.WaitListenerPresent(ctx, testListener)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Permission denied", "the rejection of what is published is the news, not the older statement")
	})

	t.Run("an acknowledgement of a newer version", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		openDelta(tr, parent, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, nil)

		published[testListener] = "h2"
		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, child, "c1", "")
		requirePresent(t, tr, testListener)

		ackDelta(tr, parent, "p1", "")
		requirePresent(t, tr, testListener, "the older statement does not put the listener back to the version it named")
	})

	t.Run("an acknowledged removal", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		openDelta(tr, parent, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, nil)

		openDelta(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "")
		sendDelta(tr, child, "c2", nil, []string{testListener})
		ackDelta(tr, child, "c2", "")

		ackDelta(tr, parent, "p1", "")
		// The same content is published again (the pod is re-added as it was).
		requireNotPresent(t, tr, testListener, "the last word on the listener is its acknowledged removal")
		requireAbsentNow(t, tr, testListener)
	})
}

// TestWaitListenerPresent_ARejectionIsOfAVersion: a NACK is the proxy's word
// on the version it was sent. It fails the wait for that version, at once and
// with the proxy's error, and says nothing of a version published since: a
// same-named replacement waits for the answer to its own listener, and a
// rejected removal fails no wait for the listener to be present.
func TestWaitListenerPresent_ARejectionIsOfAVersion(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	openDelta(tr, 1, resourcev3.ListenerType, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "Permission denied")

	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Permission denied", "the published version is the one the proxy rejected")

	// The replacement is published: the rejection was of the other pod's.
	published[testListener] = "h2"
	requireNotPresent(t, tr, testListener, "nothing has answered the replacement's listener yet")
	sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
	requireNotPresent(t, tr, testListener)
	ackDelta(tr, 1, "n2", "")
	requirePresent(t, tr, testListener)

	// A rejected removal leaves the proxy holding what it held.
	sendDelta(tr, 1, "n3", nil, []string{testListener})
	ackDelta(tr, 1, "n3", "cannot remove")
	requirePresent(t, tr, testListener, "the proxy still holds the published version")
	ctx2, cancel2 := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel2()
	err = tr.WaitListenerAbsent(ctx2, testListener)
	require.Error(t, err, "the removal wait is the one a rejected removal fails")
	assert.Contains(t, err.Error(), "cannot remove")
}

// TestWaitListenerPresent_WithoutAPublishedVersionAnyRejectionFails: the
// tracker nobody gave a published version to behaves as it always did.
func TestWaitListenerPresent_WithoutAPublishedVersionAnyRejectionFails(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "")
	sendDelta(tr, 1, "n2", nil, []string{testListener})
	ackDelta(tr, 1, "n2", "cannot remove")
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot remove")
}
