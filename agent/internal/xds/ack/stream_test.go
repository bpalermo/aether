package ack

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of #1624: what a proxy holds is kept per stream, a wait is for the
// version the agent publishes, and nothing known is not a listener absent.
//
// A stream is one proxy process. The tests name two of them parent and child
// where they are the two generations of a hot restart.
//
// Every test that expects a wait NOT to return uses unresolvedWait, and every
// one that expects it to return uses a deadline far above anything a healthy
// run needs, so neither kind passes or fails on timing.

const (
	// resolvedWait bounds a wait that must return.
	resolvedWait = 10 * time.Second
	// unresolvedWait is how long a wait that must NOT return is given.
	unresolvedWait = 50 * time.Millisecond

	otherListener = "outbound_http_other-pod"
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

// connect is go-control-plane announcing a delta stream: typeURL is empty for
// the ADS stream the proxy opens.
func connect(t *Tracker, streamID int64, typeURL string) {
	_ = t.Callbacks().OnDeltaStreamOpen(context.Background(), streamID, typeURL)
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

// holding plays the opening Listener exchange of a proxy that states it holds
// `stated`, against an agent that has nothing to add or remove: the request,
// the empty first response, and its ACK.
func holding(t *Tracker, streamID int64, stated map[string]string) {
	open(t, streamID, resourcev3.ListenerType, stated)
	sendDelta(t, streamID, "opening", nil, nil)
	ackDelta(t, streamID, "opening", "")
}

func requirePresent(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	require.NoError(t, tr.WaitListenerPresent(ctx, name), msgAndArgs...)
}

// requireNotPresent asserts the wait runs to its deadline: neither answered
// nor failed by a NACK.
func requireNotPresent(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, name)
	require.Error(t, err, msgAndArgs...)
	require.Contains(t, err.Error(), "timed out", msgAndArgs...)
}

// requireRejected asserts the wait for the listener to be present fails with
// the proxy's error.
func requireRejected(t *testing.T, tr *Tracker, name, detail string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, name)
	require.Error(t, err, msgAndArgs...)
	require.Contains(t, err.Error(), "envoy rejected config", msgAndArgs...)
	require.Contains(t, err.Error(), detail, msgAndArgs...)
}

func requireAbsent(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	require.NoError(t, tr.WaitListenerAbsent(ctx, name), msgAndArgs...)
}

// requireNotAbsent asserts the removal wait runs to its deadline.
func requireNotAbsent(t *testing.T, tr *Tracker, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
	defer cancel()
	err := tr.WaitListenerAbsent(ctx, name)
	require.Error(t, err, msgAndArgs...)
	require.Contains(t, err.Error(), "timed out", msgAndArgs...)
}

// --- a proxy that states the published version answers the wait (#1511) ---

// TestWaitListenerPresent_AnsweredByWhatTheProxyStates: after an agent restart
// the proxy holds the pod's listener at the version the agent publishes, so
// go-control-plane sends nothing for it and nothing acknowledges it by name.
// The proxy says so itself, in its opening request.
func TestWaitListenerPresent_AnsweredByWhatTheProxyStates(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1"})
	requireNotPresent(t, tr, testListener, "no proxy is connected")

	open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	requireNotPresent(t, tr, testListener, "the response to the statement is not written: it may carry the listener")

	sendDelta(tr, 1, "n1", nil, nil)
	requirePresent(t, tr, testListener, "the proxy stated the published version and is sent nothing for it")
}

// TestWaitListenerPresent_AnsweredByAStatementWhileWaiting: the wait is
// already blocked when the proxy reconnects, which is the order a CNI ADD
// retried across an agent restart sees.
func TestWaitListenerPresent_AnsweredByAStatementWhileWaiting(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1", otherListener: "a1"})

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		done <- tr.WaitListenerPresent(ctx, testListener)
	}()

	// The opening response also carries a listener the proxy did not hold.
	open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	sendListeners(tr, 1, "n1", map[string]string{otherListener: "a1"}, nil)
	require.NoError(t, <-done)

	requireNotPresent(t, tr, otherListener, "sent is not acknowledged")
	ackDelta(tr, 1, "n1", "")
	requirePresent(t, tr, otherListener)
}

// TestStatement_AnswersOnlyForTheVersionStated: a listener the proxy did not
// state, or stated at a version that is not the published one, is answered by
// nothing but the proxy's answer to the response that carries it.
func TestStatement_AnswersOnlyForTheVersionStated(t *testing.T) {
	t.Run("not stated", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1", otherListener: "h1"})
		holding(tr, 1, map[string]string{otherListener: "h1"})
		requirePresent(t, tr, otherListener)
		requireNotPresent(t, tr, testListener, "the proxy never stated this listener")
	})

	t.Run("stated at another version, then accepted", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h2"})
		open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "stale"})
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h2"}, nil)
		requireNotPresent(t, tr, testListener, "the published version is on the wire, not acknowledged")
		ackDelta(tr, 1, "n1", "")
		requirePresent(t, tr, testListener)
	})

	t.Run("stated at another version, then rejected", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h2", otherListener: "h1"})
		open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "stale", otherListener: "h1"})
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, 1, "n1", "Permission denied")
		requireRejected(t, tr, testListener, "Permission denied", "the proxy holds the version it stated, not the published one")
		// The rejected response did not carry this one: the proxy holds it as
		// it stated it, whatever it made of the response.
		requirePresent(t, tr, otherListener)
	})

	t.Run("only the first request of the type states", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		holding(tr, 1, nil)
		// go-control-plane reads initial_resource_versions from the first
		// request of a type on a stream and from no other.
		open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		requireNotPresent(t, tr, testListener, "not the opening request")
	})

	t.Run("a statement of another type", func(t *testing.T) {
		tr := NewTracker(slog.New(slog.DiscardHandler))
		// The same name and version are published as a cluster too.
		tr.SetPublishedVersion(func(string, string) (string, bool) { return "h1", true })
		open(tr, 1, resourcev3.ClusterType, map[string]string{testListener: "h1"})
		sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", nil)
		ackDelta(tr, 1, "n1", "")
		requireNotPresent(t, tr, testListener, "a cluster of that name was stated, not a listener")
	})
}

// --- a wait is for the version the agent publishes, not for a name ---

// TestWaitListenerPresent_IsForThePublishedVersion: a pod's listener is named
// after the pod, so a same-named replacement publishes other content under a
// name the proxy already holds. What the proxy acknowledged or stated for the
// name before says nothing about the replacement: its wait is for the
// acknowledgement of the response that carries the published version.
func TestWaitListenerPresent_IsForThePublishedVersion(t *testing.T) {
	for name, hold := range map[string]func(tr *Tracker){
		"acknowledged earlier": func(tr *Tracker) {
			holding(tr, 1, nil)
			sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
			ackDelta(tr, 1, "n1", "")
		},
		"stated": func(tr *Tracker) {
			holding(tr, 1, map[string]string{testListener: "h1"})
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

			// The stream ends with the update in flight.
			tr.onDeltaStreamClosed(1, nil)
			requireNotPresent(t, tr, testListener)

			// The proxy reconnects, states what it holds, and is sent the
			// update again.
			open(tr, 2, resourcev3.ListenerType, map[string]string{testListener: "h1"})
			sendListeners(tr, 2, "n1", map[string]string{testListener: "h2"}, nil)
			requireNotPresent(t, tr, testListener)
			ackDelta(tr, 2, "n1", "")
			requirePresent(t, tr, testListener, "the proxy acknowledged the published version")

			// Nothing published under the name: no version to hold.
			delete(published, testListener)
			requireNotPresent(t, tr, testListener, "nothing is published under the name")
			requireNotAbsent(t, tr, testListener, "the proxy holds it until it acknowledges the removal")
		})
	}
}

// TestWaitListenerPresent_ARejectionIsOfAVersion: a NACK is the proxy's word
// on the version it was sent. It fails the wait for that version, at once and
// with the proxy's error, and says nothing of a version published since.
func TestWaitListenerPresent_ARejectionIsOfAVersion(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	holding(tr, 1, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "Permission denied")
	requireRejected(t, tr, testListener, "Permission denied", "the published version is the one the proxy rejected")

	// The replacement is published: the rejection was of the other pod's.
	published[testListener] = "h2"
	requireNotPresent(t, tr, testListener, "nothing has answered the replacement's listener yet")
	sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
	requireNotPresent(t, tr, testListener)
	ackDelta(tr, 1, "n2", "")
	requirePresent(t, tr, testListener)

	// A rejected removal leaves the proxy holding what it held, and is the
	// rejection the removal wait fails on.
	sendDelta(tr, 1, "n3", nil, []string{testListener})
	ackDelta(tr, 1, "n3", "cannot remove")
	requirePresent(t, tr, testListener, "the proxy still holds the published version")
	ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
	defer cancel()
	err := tr.WaitListenerAbsent(ctx, testListener)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot remove")
}

// TestWaitListenerPresent_ARejectedRemovalFailsNoWaitForPresence, with or
// without a published version to hold a rejection to: the proxy that refused
// to drop the listener holds it.
func TestWaitListenerPresent_ARejectedRemovalFailsNoWaitForPresence(t *testing.T) {
	for name, tr := range map[string]*Tracker{
		"a wait for a version": publishing(map[string]string{testListener: ""}),
		"a wait for a name":    NewTracker(slog.New(slog.DiscardHandler)),
	} {
		t.Run(name, func(t *testing.T) {
			holding(tr, 1, nil)
			sendDelta(tr, 1, "n1", []string{testListener}, nil)
			ackDelta(tr, 1, "n1", "")
			sendDelta(tr, 1, "n2", nil, []string{testListener})
			ackDelta(tr, 1, "n2", "cannot remove")
			requirePresent(t, tr, testListener)
		})
	}
}

// TestWaitListenerPresent_NotWhileSomethingIsUnansweredOnThatStream: the
// proxy acknowledged h1, was sent h2, and the agent went back to h1. The
// acknowledged version is the published one again and is not what the proxy
// was last sent: it may have applied h2.
func TestWaitListenerPresent_NotWhileSomethingIsUnansweredOnThatStream(t *testing.T) {
	published := map[string]string{testListener: "h1"}
	tr := publishing(published)
	holding(tr, 1, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "")

	sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
	requireNotPresent(t, tr, testListener, "h2 is on the wire")

	ackDelta(tr, 1, "n2", "")
	requireNotPresent(t, tr, testListener, "the proxy holds h2 and h1 is published")
	sendListeners(tr, 1, "n3", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n3", "")
	requirePresent(t, tr, testListener)
}

// TestWait_HoldingsAndPublishedVersionAreReadTogether: the wait reads what the
// proxies hold under the tracker's lock and the published version outside it.
// If the tracker changes in between, the two were never true together and the
// wait looks again instead of answering from the pair.
func TestWait_HoldingsAndPublishedVersionAreReadTogether(t *testing.T) {
	for name, between := range map[string]func(tr *Tracker){
		// The proxy is sent and acknowledges h2, and the agent goes back to h1.
		"acknowledged": func(tr *Tracker) {
			sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
			ackDelta(tr, 1, "n2", "")
		},
		"sent": func(tr *Tracker) {
			sendListeners(tr, 1, "n2", map[string]string{testListener: "h2"}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tr := NewTracker(slog.New(slog.DiscardHandler))
			asked := 0
			tr.SetPublishedVersion(func(string, string) (string, bool) {
				asked++
				if asked == 1 {
					between(tr)
				}
				return "h1", true
			})
			holding(tr, 1, nil)
			sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
			ackDelta(tr, 1, "n1", "")

			ctx, cancel := context.WithTimeout(context.Background(), unresolvedWait)
			defer cancel()
			require.Error(t, tr.WaitListenerPresent(ctx, testListener), "the proxy is on h2 by the time h1 is read as published")
			require.GreaterOrEqual(t, asked, 2, "the wait looked again")
		})
	}
}

// --- nothing known is not a listener absent (#1572) ---

// TestWaitListenerAbsent_UnknownIsNotAbsent walks a proxy reconnecting to a
// restarted agent. The proxy holds the pod's listener throughout; the removal
// wait must not return before the agent knows what the proxy holds.
func TestWaitListenerAbsent_UnknownIsNotAbsent(t *testing.T) {
	tr := publishing(map[string]string{})

	requireNotAbsent(t, tr, testListener, "no proxy is connected: the one that reconnects may hold it")

	connect(tr, 1, "")
	requireNotAbsent(t, tr, testListener, "a proxy connected and has not said what listeners it holds")

	open(tr, 1, resourcev3.ClusterType, nil)
	sendVersioned(tr, 1, resourcev3.ClusterType, "c1", "v1", []string{"c"})
	ackDelta(tr, 1, "c1", "")
	requireNotAbsent(t, tr, testListener, "its clusters say nothing of its listeners")

	open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
	requireNotAbsent(t, tr, testListener, "stated")

	sendDelta(tr, 1, "n1", nil, []string{testListener})
	requireNotAbsent(t, tr, testListener, "the removal is on the wire")

	ackDelta(tr, 1, "n1", "")
	requireAbsent(t, tr, testListener, "the proxy acknowledged the removal")
}

// TestWaitListenerAbsent_KnownFromTheFirstResponse: a proxy that states no
// such listener does not hold it, and that is known from the moment its
// statement is answered, not from the statement. The answer was computed from
// the snapshot of a moment before and can add the very listener a pod DEL has
// just taken out of the snapshot.
func TestWaitListenerAbsent_KnownFromTheFirstResponse(t *testing.T) {
	t.Run("nothing to add", func(t *testing.T) {
		tr := publishing(map[string]string{})
		open(tr, 1, resourcev3.ListenerType, map[string]string{otherListener: "h1"})
		requireNotAbsent(t, tr, testListener, "the statement is not answered yet")
		sendDelta(tr, 1, "n1", nil, nil)
		requireAbsent(t, tr, testListener, "not stated, and not in the answer")
		requireNotAbsent(t, tr, otherListener, "stated")
	})

	t.Run("the answer adds the listener", func(t *testing.T) {
		tr := publishing(map[string]string{})
		open(tr, 1, resourcev3.ListenerType, nil)
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
		requireNotAbsent(t, tr, testListener, "on the wire: the proxy may have applied it")
		ackDelta(tr, 1, "n1", "")
		requireNotAbsent(t, tr, testListener, "the proxy holds it")
		sendDelta(tr, 1, "n2", nil, []string{testListener})
		ackDelta(tr, 1, "n2", "")
		requireAbsent(t, tr, testListener)
	})

	t.Run("a first response that answers no statement", func(t *testing.T) {
		// The tracker did not see the request: it cannot know the whole set.
		tr := publishing(map[string]string{})
		sendDelta(tr, 1, "n1", []string{otherListener}, nil)
		ackDelta(tr, 1, "n1", "")
		requireNotAbsent(t, tr, testListener)
	})
}

// TestWaitListenerAbsent_OnlyAStreamThatCarriesListenersCounts: a delta
// stream opened for another type alone (not ADS) will never say anything of
// listeners, and holds no removal wait.
func TestWaitListenerAbsent_OnlyAStreamThatCarriesListenersCounts(t *testing.T) {
	tr := publishing(map[string]string{})
	holding(tr, 1, nil)
	connect(tr, 2, resourcev3.ClusterType)
	open(tr, 2, resourcev3.ClusterType, nil)
	requireAbsent(t, tr, testListener)

	connect(tr, 3, resourcev3.ListenerType)
	requireNotAbsent(t, tr, testListener, "a stream opened for listeners has not said what it holds")
}

// TestWaitListenerAbsent_ARejectedAddIsNotAbsent: the proxy rejected the
// response that added the listener. That does not make the listener absent:
// the wait is for the acknowledged removal, which go-control-plane sends.
// Before #1624 the removal wait returned the rejection at once.
func TestWaitListenerAbsent_ARejectedAddIsNotAbsent(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1"})
	holding(tr, 1, nil)
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
	ackDelta(tr, 1, "n1", "Permission denied")
	requireNotAbsent(t, tr, testListener)

	sendDelta(tr, 1, "n2", nil, []string{testListener})
	requireNotAbsent(t, tr, testListener)
	ackDelta(tr, 1, "n2", "")
	requireAbsent(t, tr, testListener)
}

// --- what is known of a proxy goes with its stream (#1585, #1573) ---

// TestStreamClose_ForgetsWhatItsProxyHeld: the listener was acknowledged, and
// the stream closed. The next stream is a new proxy process, which holds
// nothing and says so; or the same one, which says what it holds.
func TestStreamClose_ForgetsWhatItsProxyHeld(t *testing.T) {
	acknowledged := func(t *testing.T) *Tracker {
		tr := publishing(map[string]string{testListener: "h1"})
		holding(tr, 1, nil)
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, 1, "n1", "")
		requirePresent(t, tr, testListener, "fixture")
		tr.onDeltaStreamClosed(1, nil)
		return tr
	}

	t.Run("no proxy", func(t *testing.T) {
		tr := acknowledged(t)
		requireNotPresent(t, tr, testListener, "nothing is connected to hold it")
		requireNotAbsent(t, tr, testListener, "and nothing is known to lack it")
	})

	t.Run("a new process", func(t *testing.T) {
		tr := acknowledged(t)
		open(tr, 2, resourcev3.ListenerType, nil)
		sendListeners(tr, 2, "n1", map[string]string{testListener: "h1"}, nil)
		requireNotPresent(t, tr, testListener, "the new process has not acknowledged the listener")
		ackDelta(tr, 2, "n1", "")
		requirePresent(t, tr, testListener)
	})

	t.Run("the same process", func(t *testing.T) {
		tr := acknowledged(t)
		holding(tr, 2, map[string]string{testListener: "h1"})
		requirePresent(t, tr, testListener)
	})

	t.Run("a rejection goes too", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		holding(tr, 1, nil)
		sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, 1, "n1", "Permission denied")
		requireRejected(t, tr, testListener, "Permission denied", "fixture")
		tr.onDeltaStreamClosed(1, nil)
		requireNotPresent(t, tr, testListener, "the proxy that rejected it is gone: nothing to fail the wait")
	})

	t.Run("an answer for a closed stream", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n1", nil, nil)
		tr.onDeltaStreamClosed(1, nil)
		// Nothing of a closed stream is taken up again, and it does not come
		// back as a proxy that has yet to say what it holds.
		ackDelta(tr, 1, "n1", "")
		open(tr, 1, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		sendDelta(tr, 1, "n2", nil, nil)
		assert.Zero(t, proxies(tr))
		requireNotPresent(t, tr, testListener)

		holding(tr, 2, nil)
		requireAbsent(t, tr, testListener, "the proxy that is connected does not hold it")
	})
}

// TestTrackerForgetsARemovedListener: a listener whose removal a proxy
// acknowledged is forgotten, not kept as absent (#1573), so pod churn leaves
// nothing behind; and what a proxy holds of a type nobody waits on is not
// kept at all.
func TestTrackerForgetsARemovedListener(t *testing.T) {
	tr := publishing(map[string]string{})
	holding(tr, 1, nil)
	open(tr, 1, resourcev3.ClusterType, map[string]string{"stated": "h"})

	const pods = 200
	for i := range pods {
		name := fmt.Sprintf("outbound_http_pod-%d", i)
		cluster := fmt.Sprintf("cluster-%d", i)
		sendVersioned(tr, 1, resourcev3.ClusterType, fmt.Sprintf("c%d", i), "v", []string{cluster})
		ackDelta(tr, 1, fmt.Sprintf("c%d", i), "")
		sendDelta(tr, 1, fmt.Sprintf("a%d", i), []string{name}, nil)
		ackDelta(tr, 1, fmt.Sprintf("a%d", i), "")
		if i%2 == 0 {
			// Every other one is rejected once before it is removed.
			sendDelta(tr, 1, fmt.Sprintf("u%d", i), []string{name}, nil)
			ackDelta(tr, 1, fmt.Sprintf("u%d", i), "rejected")
		}
	}
	require.Equal(t, pods, remembered(tr), "fixture: the proxy holds every listener, and no cluster is kept")

	for i := range pods {
		sendDelta(tr, 1, fmt.Sprintf("r%d", i), nil, []string{fmt.Sprintf("outbound_http_pod-%d", i)})
		ackDelta(tr, 1, fmt.Sprintf("r%d", i), "")
	}
	assert.Zero(t, remembered(tr), "a removed listener's name is still kept")
	assert.Empty(t, unanswered(tr))
}

// --- two proxy generations at once ---

const parent, child = int64(1), int64(2)

// TestTwoGenerations_NoAnswerOfOneErasesTheOthers: with one state per name
// the last answer to arrive won, whatever generation and version it was of.
// Each case is one generation's late answer arriving after the other's newer
// one.
func TestTwoGenerations_NoAnswerOfOneErasesTheOthers(t *testing.T) {
	t.Run("an older acknowledgement after a newer one", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		holding(tr, parent, nil)
		holding(tr, child, nil)
		sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "")

		published[testListener] = "h2"
		sendListeners(tr, child, "c2", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, child, "c2", "")
		requirePresent(t, tr, testListener, "the generation taking over holds the published version")

		// The parent acknowledges its older response, and exits before it is
		// sent h2.
		ackDelta(tr, parent, "p1", "")
		requirePresent(t, tr, testListener, "the parent's h1 does not replace the child's h2")
		tr.onDeltaStreamClosed(parent, nil)
		requirePresent(t, tr, testListener)
	})

	t.Run("an older add after an acknowledged removal", func(t *testing.T) {
		tr := publishing(map[string]string{})
		holding(tr, parent, nil)
		holding(tr, child, nil)
		sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "")
		sendDelta(tr, child, "c2", nil, []string{testListener})
		ackDelta(tr, child, "c2", "")

		ackDelta(tr, parent, "p1", "")
		requireNotAbsent(t, tr, testListener, "the parent holds it: it has sockets in the pod's netns")

		// The parent exits before it acknowledges its own removal. Nothing is
		// left that holds the listener, and nothing says so for ever.
		tr.onDeltaStreamClosed(parent, nil)
		requireAbsent(t, tr, testListener)
	})

	t.Run("an older removal after a newer add", func(t *testing.T) {
		published := map[string]string{}
		tr := publishing(published)
		holding(tr, parent, map[string]string{testListener: "h1"})
		holding(tr, child, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, []string{testListener})
		sendDelta(tr, child, "c1", nil, []string{testListener})
		ackDelta(tr, child, "c1", "")

		// Published again, sent to and acknowledged by the child.
		published[testListener] = "h2"
		sendListeners(tr, child, "c2", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, child, "c2", "")
		ackDelta(tr, parent, "p1", "")
		requirePresent(t, tr, testListener, "the parent's removal is of what the parent held")
		requireNotAbsent(t, tr, testListener)
	})

	t.Run("an older rejection after a newer one", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		holding(tr, parent, nil)
		holding(tr, child, nil)
		sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)

		published[testListener] = "h2"
		sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, child, "c1", "child refused h2")
		ackDelta(tr, parent, "p1", "parent refused h1")
		requireRejected(t, tr, testListener, "child refused h2", "the rejection of what is published stands")
	})

	t.Run("a rejection of an older version next to an acknowledgement of the published one", func(t *testing.T) {
		published := map[string]string{testListener: "h1"}
		tr := publishing(published)
		holding(tr, parent, nil)
		holding(tr, child, nil)
		sendListeners(tr, parent, "p1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, parent, "p1", "parent refused h1")

		published[testListener] = "h2"
		sendListeners(tr, child, "c1", map[string]string{testListener: "h2"}, nil)
		ackDelta(tr, child, "c1", "")
		requirePresent(t, tr, testListener)
	})
}

// TestTwoGenerations_WhichMustHoldTheListener: one generation holding the
// published version answers the wait for it to be present, and one rejecting
// it fails that wait. The removal wait is for every generation.
func TestTwoGenerations_WhichMustHoldTheListener(t *testing.T) {
	t.Run("present: one is enough", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		holding(tr, parent, map[string]string{testListener: "h1"})
		// The child has connected and is still loading its clusters.
		connect(tr, child, "")
		requirePresent(t, tr, testListener)

		// A response the child has not answered does not hold up the wait.
		open(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		requirePresent(t, tr, testListener)
	})

	t.Run("present: a rejection by either fails it", func(t *testing.T) {
		tr := publishing(map[string]string{testListener: "h1"})
		holding(tr, parent, map[string]string{testListener: "h1"})
		open(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "Permission denied")
		requireRejected(t, tr, testListener, "Permission denied", "one generation holding the version does not make the other accept it")

		// Whichever order they spoke in.
		tr = publishing(map[string]string{testListener: "h1"})
		open(tr, child, resourcev3.ListenerType, nil)
		sendListeners(tr, child, "c1", map[string]string{testListener: "h1"}, nil)
		ackDelta(tr, child, "c1", "Permission denied")
		holding(tr, parent, map[string]string{testListener: "h1"})
		requireRejected(t, tr, testListener, "Permission denied")
	})

	t.Run("absent: every one", func(t *testing.T) {
		tr := publishing(map[string]string{})
		holding(tr, parent, map[string]string{testListener: "h1"})
		holding(tr, child, map[string]string{testListener: "h1"})
		sendDelta(tr, parent, "p1", nil, []string{testListener})
		sendDelta(tr, child, "c1", nil, []string{testListener})

		ackDelta(tr, child, "c1", "")
		requireNotAbsent(t, tr, testListener, "the parent has not acknowledged the removal")
		ackDelta(tr, parent, "p1", "")
		requireAbsent(t, tr, testListener)
	})

	t.Run("absent: a generation that has not said what it holds", func(t *testing.T) {
		tr := publishing(map[string]string{})
		holding(tr, parent, nil)
		requireAbsent(t, tr, testListener, "fixture")
		connect(tr, child, "")
		requireNotAbsent(t, tr, testListener, "the stream that just opened may be a proxy reconnecting with the listener")
		holding(tr, child, nil)
		requireAbsent(t, tr, testListener)
	})
}

// TestStreamClose_WakesAWaiterItUnblocks: a wait held up by one stream alone
// is looked at again when that stream closes, not at its deadline.
func TestStreamClose_WakesAWaiterItUnblocks(t *testing.T) {
	tr := publishing(map[string]string{})
	holding(tr, parent, map[string]string{testListener: "h1"})
	holding(tr, child, nil)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), resolvedWait)
		defer cancel()
		done <- tr.WaitListenerAbsent(ctx, testListener)
	}()
	requireNotAbsent(t, tr, testListener, "fixture: the parent holds the listener")
	tr.onDeltaStreamClosed(parent, nil)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(resolvedWait / 2):
		t.Fatal("the waiter was not woken by the stream closing")
	}
}

// TestStreamClose_TellsTheDeliveryObserverOutsideTheLock: a closing stream
// tells the DeliveryObserver what left flight with the tracker's lock
// released, so the observer can ask the tracker something itself.
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
	open(tr, 1, resourcev3.ListenerType, nil)
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

// --- an answer is of its own stream and its own type ---

// TestAck_ANonceEchoedUnderAnotherTypeAnswersNothing: the nonce of a Listener
// response echoed by a request about clusters is not an acknowledgement of
// the listeners. The response stays in flight for the answer that is.
func TestAck_ANonceEchoedUnderAnotherTypeAnswersNothing(t *testing.T) {
	tr, told := observe(t)
	tr.SetPublishedVersion(func(string, string) (string, bool) { return "h1", true })
	holding(tr, 1, nil)
	*told = nil
	sendListeners(tr, 1, "n1", map[string]string{testListener: "h1"}, nil)

	require.NoError(t, tr.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}))
	requireNotPresent(t, tr, testListener, "a request about clusters acknowledged a listener")
	assert.Empty(t, *told, "and the AckObserver was told so")
	assert.Len(t, unanswered(tr), 1, "the response is still waiting for its answer")

	ackDelta(tr, 1, "n1", "")
	requirePresent(t, tr, testListener)
	assert.Len(t, *told, 1)
}

// TestAckObserver_TheStatementItIsHandedIsNotTheTrackersWorkingCopy: the
// AckObserver is handed the proxy's statement to read, after the tracker's
// lock is released. What the tracker goes on writing as the proxy answers is
// its own account, not that map.
func TestAckObserver_TheStatementItIsHandedIsNotTheTrackersWorkingCopy(t *testing.T) {
	tr, told := observe(t)
	holding(tr, 1, map[string]string{testListener: "h1"})
	require.Len(t, *told, 1)
	stated := (*told)[0].Stated

	sendListeners(tr, 1, "n1", map[string]string{otherListener: "h2"}, []string{testListener})
	ackDelta(tr, 1, "n1", "")
	assert.Equal(t, map[string]string{testListener: "h1"}, stated)
}

// TestTracker_ConcurrentStreamsAndWaiters is for the race detector.
func TestTracker_ConcurrentStreamsAndWaiters(t *testing.T) {
	tr := publishing(map[string]string{testListener: "h1", otherListener: "h2"})
	tr.SetAckObserver(func(context.Context, Accepted) {})
	tr.SetDeliveryObserver(func(context.Context, Delivery) {})
	var wg sync.WaitGroup
	for worker := int64(1); worker <= 8; worker++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := int64(0); i < 200; i++ {
				stream := worker*1000 + i
				connect(tr, stream, "")
				open(tr, stream, resourcev3.ListenerType, map[string]string{testListener: "h1", otherListener: "h2"})
				open(tr, stream, resourcev3.ClusterType, map[string]string{"c": "h"})
				sendDelta(tr, stream, "n1", nil, []string{otherListener})
				ackDelta(tr, stream, "n1", "")
				sendDelta(tr, stream, "n2", []string{otherListener}, nil)
				ackDelta(tr, stream, "n2", "boom")
				tr.onDeltaStreamClosed(stream, nil)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				_ = tr.WaitListenerPresent(ctx, testListener)
				_ = tr.WaitListenerAbsent(ctx, otherListener)
				cancel()
			}
		}()
	}
	wg.Wait()
	assert.Zero(t, proxies(tr))
}
