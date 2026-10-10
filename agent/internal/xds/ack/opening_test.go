package ack

import (
	"context"
	"log/slog"
	"testing"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// observe returns a tracker and the calls its AckObserver has received.
func observe(t *testing.T) (*Tracker, *[]Accepted) {
	t.Helper()
	tr := NewTracker(slog.New(slog.DiscardHandler))
	got := &[]Accepted{}
	tr.SetAckObserver(func(_ context.Context, accepted Accepted) {
		*got = append(*got, accepted)
	})
	return tr, got
}

// open simulates the first request of a type on a stream: no nonce, and the
// proxy's statement of the resources of the type it holds.
func open(t *Tracker, streamID int64, typeURL string, stated map[string]string) {
	_ = t.onDeltaRequest(streamID, &discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeURL, InitialResourceVersions: stated})
}

// TestAckObserver_ToldOfAnAcknowledgedEmptyOpeningResponse is #1483. A proxy
// that reconnects states, in its first request of a type, the version of every
// resource it holds; go-control-plane answers that request even when the
// snapshot holds exactly those (//agent/test/mtlspool,
// TestReconnectingProxyStatesTheClustersItHolds measures both on the pinned
// proxy). The ACK of that empty answer is the only word a restarted agent gets
// from an in-sync proxy until a resource changes, and with the statement it
// says exactly what the proxy holds.
func TestAckObserver_ToldOfAnAcknowledgedEmptyOpeningResponse(t *testing.T) {
	tr, got := observe(t)
	stated := map[string]string{"c1": "h1", "c2": "h2"}

	open(tr, 1, resourcev3.ClusterType, stated)
	assert.Empty(t, *got, "a statement alone is told to nobody: its answer is")
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v7", nil)
	assert.Empty(t, *got, "sent is not acknowledged")
	ackDelta(tr, 1, "n1", "")
	require.Equal(t, []Accepted{{TypeURL: resourcev3.ClusterType, SystemVersion: "v7", Opening: true, Stated: stated}}, *got,
		"the acknowledged empty first response of a type on a stream: the proxy holds what it stated")
	ackDelta(tr, 1, "n1", "")
	assert.Len(t, *got, 1, "and is told once")
}

// TestAckObserver_OpeningResponseCarriesTheStatementWhateverItsAnswer: the
// first response of a type is the one computed against the proxy's statement,
// so its answer settles the whole set. Acknowledged, the proxy holds what it
// stated with the response applied over it. REJECTED, it holds what it stated
// and nothing of the response: a proxy never states a version it rejected, and
// a NACK changes none (//agent/test/mtlspool,
// TestReconnectingProxyStatesNoClusterItRejected).
func TestAckObserver_OpeningResponseCarriesTheStatementWhateverItsAnswer(t *testing.T) {
	tr, got := observe(t)
	stated := map[string]string{"kept": "h1", "changed": "h2", "gone": "h3"}

	open(tr, 1, resourcev3.ClusterType, stated)
	tr.onDeltaResponse(1, nil, &discoveryv3.DeltaDiscoveryResponse{
		TypeUrl: resourcev3.ClusterType, Nonce: "n1", SystemVersionInfo: "v2",
		Resources:        []*discoveryv3.Resource{{Name: "changed", Version: "h2b"}, {Name: "new", Version: "h4"}},
		RemovedResources: []string{"gone"},
	})
	ackDelta(tr, 1, "n1", "")
	require.Equal(t, []Accepted{{
		TypeURL: resourcev3.ClusterType, SystemVersion: "v2", Opening: true, Stated: stated,
		Added:   []Resource{{Name: "changed", Version: "h2b"}, {Name: "new", Version: "h4"}},
		Removed: []string{"gone"},
	}}, *got)

	// The same exchange on another stream, rejected.
	open(tr, 2, resourcev3.ClusterType, stated)
	tr.onDeltaResponse(2, nil, &discoveryv3.DeltaDiscoveryResponse{
		TypeUrl: resourcev3.ClusterType, Nonce: "n1", SystemVersionInfo: "v2",
		Resources:        []*discoveryv3.Resource{{Name: "changed", Version: "h2b"}},
		RemovedResources: []string{"gone"},
	})
	ackDelta(tr, 2, "n1", "rejected")
	require.Len(t, *got, 2)
	assert.Equal(t, Accepted{TypeURL: resourcev3.ClusterType, SystemVersion: "v2", Opening: true, Rejected: true, Stated: stated}, (*got)[1],
		"a rejected opening response: the statement, and nothing the response carried")
}

// TestAckObserver_ANewProxyStatesNothing: a proxy that holds no resource of
// the type sends no initial_resource_versions. That is still a statement
// ("none"), not the absence of one.
func TestAckObserver_ANewProxyStatesNothing(t *testing.T) {
	tr, got := observe(t)

	open(tr, 1, resourcev3.ClusterType, nil)
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"c1"})
	ackDelta(tr, 1, "n1", "")
	require.Len(t, *got, 1)
	assert.True(t, (*got)[0].Opening)
	assert.NotNil(t, (*got)[0].Stated)
	assert.Empty(t, (*got)[0].Stated)
	assert.Equal(t, []Resource{{Name: "c1"}}, (*got)[0].Added)
}

// TestAckObserver_NotToldOfALaterEmptyResponse is the limit of the opening
// rule. Only the FIRST response of a type on a stream is computed against what
// the proxy stated. Every later one is computed against what the server has
// SENT on the stream, accepted or not: go-control-plane records a response's
// resources as returned when it writes it. And it does send later empty
// responses: it answers every wildcard request that carries no nonce, which is
// what an on-demand subscription made in the middle of a stream is.
//
// So after a rejected cluster update, an on-demand subscribe is answered with
// an empty response naming the very snapshot the proxy rejected, and the proxy
// ACKs it (there is nothing in it to reject). It says nothing about any
// resource, and neither does the request that asked for it.
func TestAckObserver_NotToldOfALaterEmptyResponse(t *testing.T) {
	tr, got := observe(t)

	open(tr, 1, resourcev3.ClusterType, nil)
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"c1"})
	ackDelta(tr, 1, "n1", "")
	require.Len(t, *got, 1)

	// The update the proxy rejects.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v2", []string{"c1"})
	ackDelta(tr, 1, "n2", "rejected")
	require.Len(t, *got, 1, "a later NACK is told to nobody: the proxy stays on what it had")

	// An on-demand subscribe (no nonce; it must not be read as a statement)
	// and its empty answer, built from the same v2.
	open(tr, 1, resourcev3.ClusterType, map[string]string{"c1": "not-a-statement"})
	sendVersioned(tr, 1, resourcev3.ClusterType, "n3", "v2", nil)
	ackDelta(tr, 1, "n3", "")
	assert.Len(t, *got, 1, "an empty response that is not the first of its type on the stream says nothing about what the proxy holds")
	assert.Empty(t, unanswered(tr), "and is not kept waiting for an ACK")
}

// TestAckObserver_AnAckIsOfTheResourcesItsResponseCarried is the tracker's
// half of #1508: a later ACK is told with the resources its response carried,
// at the versions it carried them at, and with nothing else. It is not marked
// as an opening and carries no statement, so an observer cannot read it as
// being about the rest of the snapshot it names.
func TestAckObserver_AnAckIsOfTheResourcesItsResponseCarried(t *testing.T) {
	tr, got := observe(t)

	open(tr, 1, resourcev3.ClusterType, nil)
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"a", "b"})
	ackDelta(tr, 1, "n1", "")
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v2", []string{"a"})
	ackDelta(tr, 1, "n2", "rejected")
	require.Len(t, *got, 1)

	tr.onDeltaResponse(1, nil, &discoveryv3.DeltaDiscoveryResponse{
		TypeUrl: resourcev3.ClusterType, Nonce: "n3", SystemVersionInfo: "v3",
		Resources:        []*discoveryv3.Resource{{Name: "b", Version: "hb3"}},
		RemovedResources: []string{"c"},
	})
	ackDelta(tr, 1, "n3", "")
	require.Len(t, *got, 2)
	assert.Equal(t, Accepted{
		TypeURL: resourcev3.ClusterType, SystemVersion: "v3",
		Added: []Resource{{Name: "b", Version: "hb3"}}, Removed: []string{"c"},
	}, (*got)[1])
}

// TestAckObserver_OpeningResponseIsPerStreamAndPerType: the first response is
// the opening one for each type on each stream, since each stream's first
// request of each type carries its own statement.
func TestAckObserver_OpeningResponseIsPerStreamAndPerType(t *testing.T) {
	tr, got := observe(t)

	// Another type's exchange does not use up the Cluster type's first.
	open(tr, 1, resourcev3.ListenerType, nil)
	open(tr, 1, resourcev3.ClusterType, map[string]string{"c1": "h1"})
	sendVersioned(tr, 1, resourcev3.ListenerType, "n1", "v1", []string{testListener})
	ackDelta(tr, 1, "n1", "")
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v1", nil)
	ackDelta(tr, 1, "n2", "")
	require.Len(t, *got, 2)
	assert.Equal(t, resourcev3.ListenerType, (*got)[0].TypeURL)
	assert.True(t, (*got)[0].Opening)
	assert.Empty(t, (*got)[0].Stated)
	assert.Equal(t, Accepted{TypeURL: resourcev3.ClusterType, SystemVersion: "v1", Opening: true, Stated: map[string]string{"c1": "h1"}}, (*got)[1])

	// A second stream (a reconnect, or the other proxy generation of a hot
	// restart) opens with its own statement.
	open(tr, 2, resourcev3.ClusterType, map[string]string{"c1": "h9"})
	sendVersioned(tr, 2, resourcev3.ClusterType, "n1", "v2", nil)
	ackDelta(tr, 2, "n1", "")
	require.Len(t, *got, 3)
	assert.Equal(t, Accepted{TypeURL: resourcev3.ClusterType, SystemVersion: "v2", Opening: true, Stated: map[string]string{"c1": "h9"}}, (*got)[2])

	// Not the first on stream 1 any more.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n3", "v2", nil)
	ackDelta(tr, 1, "n3", "")
	assert.Len(t, *got, 3)
}

// TestAckObserver_AFirstResponseWithNoStatementIsNotAnOpening: the statement
// is what makes the first response a complete account. A response the tracker
// never saw the request of is told as what it is, an ACK of the resources it
// carried, and when it carried none it is told to nobody.
func TestAckObserver_AFirstResponseWithNoStatementIsNotAnOpening(t *testing.T) {
	tr, got := observe(t)

	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", nil)
	ackDelta(tr, 1, "n1", "")
	assert.Empty(t, *got)
	assert.Empty(t, unanswered(tr))

	sendVersioned(tr, 2, resourcev3.ClusterType, "n1", "v1", []string{"c1"})
	ackDelta(tr, 2, "n1", "")
	require.Len(t, *got, 1)
	assert.False(t, (*got)[0].Opening)
	assert.Nil(t, (*got)[0].Stated)
}

// TestAckObserver_OpeningResponseThatIsNotAnswered: one whose stream ends
// first is dropped with the stream, statement included.
func TestAckObserver_OpeningResponseThatIsNotAnswered(t *testing.T) {
	tr, got := observe(t)

	open(tr, 2, resourcev3.ClusterType, map[string]string{"c1": "h1"})
	sendVersioned(tr, 2, resourcev3.ClusterType, "n1", "v1", nil)
	tr.onDeltaStreamClosed(2, nil)
	ackDelta(tr, 2, "n1", "")
	assert.Empty(t, *got, "the stream ended before the ACK")
	assert.Empty(t, unanswered(tr))

	// And a statement that was never answered at all.
	open(tr, 3, resourcev3.ClusterType, map[string]string{"c1": "h1"})
	tr.onDeltaStreamClosed(3, nil)
	assert.Empty(t, tr.streams)
}

// TestTrackerForgetsAClosedStream: what the tracker keeps per stream goes with
// the stream, so an agent that outlives many proxy reconnects holds nothing
// for the streams that ended. And a statement is held only until the response
// that answers it is written: it then belongs to that response and goes with
// its answer.
func TestTrackerForgetsAClosedStream(t *testing.T) {
	tr, _ := observe(t)
	for stream := int64(1); stream <= 50; stream++ {
		open(tr, stream, resourcev3.ClusterType, map[string]string{"c1": "h1"})
		open(tr, stream, resourcev3.ListenerType, nil)
		sendVersioned(tr, stream, resourcev3.ClusterType, "n1", "v1", nil)
		assert.Nil(t, tr.streams[stream].types[resourcev3.ClusterType].stated,
			"the statement moved to the response that answers it")
		sendVersioned(tr, stream, resourcev3.ListenerType, "n2", "v1", []string{testListener})
		ackDelta(tr, stream, "n1", "")
		tr.onDeltaStreamClosed(stream, nil)
	}
	assert.Empty(t, unanswered(tr))
	assert.Empty(t, tr.streams)
}
