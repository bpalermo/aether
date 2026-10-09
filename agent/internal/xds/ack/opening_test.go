package ack

import (
	"context"
	"log/slog"
	"testing"

	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// observe returns a tracker and the calls its AckObserver has received.
func observe(t *testing.T) (*Tracker, *[]observed) {
	t.Helper()
	tr := NewTracker(slog.New(slog.DiscardHandler))
	got := &[]observed{}
	tr.SetAckObserver(func(_ context.Context, typeURL, version string) {
		*got = append(*got, observed{typeURL, version})
	})
	return tr, got
}

// TestAckObserver_ToldOfAnAcknowledgedEmptyOpeningResponse is #1483. A proxy
// that reconnects states, in its first request of a type, the version of every
// resource it holds; go-control-plane answers that request even when the
// snapshot holds exactly those (//agent/test/mtlspool,
// TestReconnectingProxyStatesTheClustersItHolds measures both on the pinned
// proxy). That empty answer is the server saying "the snapshot with this
// version is what you stated", and its ACK is the only acknowledgement a
// restarted agent gets from an in-sync proxy until a resource changes.
func TestAckObserver_ToldOfAnAcknowledgedEmptyOpeningResponse(t *testing.T) {
	tr, got := observe(t)

	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v7", nil)
	assert.Empty(t, *got, "sent is not acknowledged")
	ackDelta(tr, 1, "n1", "")
	require.Equal(t, []observed{{resourcev3.ClusterType, "v7"}}, *got,
		"the acknowledged empty first response of a type on a stream names the snapshot the proxy holds")
	ackDelta(tr, 1, "n1", "")
	assert.Len(t, *got, 1, "and is told once")
}

// TestAckObserver_NotToldOfALaterEmptyResponse is the limit of the above, and
// it is what keeps the reading true. Only the FIRST response of a type on a
// stream is computed against what the proxy stated. Every later one is
// computed against what the server has SENT on the stream, accepted or not:
// go-control-plane records a response's resources as returned when it writes
// it. And it does send later empty responses: it answers every wildcard
// request that carries no nonce, which is what an on-demand subscription made
// in the middle of a stream is.
//
// So after a rejected cluster update, an on-demand subscribe is answered with
// an empty response naming the very snapshot the proxy rejected, and the proxy
// ACKs it (there is nothing in it to reject). Reading that as "the proxy holds
// this snapshot" would be false in exactly the state the acknowledged gauge
// exists to show.
func TestAckObserver_NotToldOfALaterEmptyResponse(t *testing.T) {
	tr, got := observe(t)

	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"c1"})
	ackDelta(tr, 1, "n1", "")
	require.Equal(t, []observed{{resourcev3.ClusterType, "v1"}}, *got)

	// The update the proxy rejects.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v2", []string{"c1"})
	ackDelta(tr, 1, "n2", "rejected")
	require.Len(t, *got, 1, "a NACK is not told as an ACK")

	// The empty answer to an on-demand subscribe, built from the same v2.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n3", "v2", nil)
	ackDelta(tr, 1, "n3", "")
	assert.Len(t, *got, 1, "an empty response that is not the first of its type on the stream says nothing about what the proxy holds")
	assert.Empty(t, tr.inflight, "and is not kept waiting for an ACK")
}

// TestAckObserver_OpeningResponseIsPerStreamAndPerType: the first response is
// counted for each type on each stream, since each stream's first request of
// each type carries its own statement.
func TestAckObserver_OpeningResponseIsPerStreamAndPerType(t *testing.T) {
	tr, got := observe(t)

	// Another type's response does not use up the Cluster type's first.
	sendVersioned(tr, 1, resourcev3.ListenerType, "n1", "v1", []string{testListener})
	ackDelta(tr, 1, "n1", "")
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v1", nil)
	ackDelta(tr, 1, "n2", "")
	require.Equal(t, []observed{{resourcev3.ListenerType, "v1"}, {resourcev3.ClusterType, "v1"}}, *got)

	// A second stream (a reconnect, or the other proxy generation of a hot
	// restart) opens with its own statement.
	sendVersioned(tr, 2, resourcev3.ClusterType, "n1", "v2", nil)
	ackDelta(tr, 2, "n1", "")
	require.Len(t, *got, 3)
	assert.Equal(t, observed{resourcev3.ClusterType, "v2"}, (*got)[2])

	// Not the first on stream 1 any more.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n3", "v2", nil)
	ackDelta(tr, 1, "n3", "")
	assert.Len(t, *got, 3)
}

// TestAckObserver_EmptyOpeningResponseThatIsNotAcknowledged: a rejected one is
// counted as a NACK and told to nobody, and one whose stream ends first is
// dropped with the stream.
func TestAckObserver_EmptyOpeningResponseThatIsNotAcknowledged(t *testing.T) {
	tr, got := observe(t)

	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", nil)
	ackDelta(tr, 1, "n1", "rejected")
	assert.Empty(t, *got, "a NACK is not told as an ACK")

	sendVersioned(tr, 2, resourcev3.ClusterType, "n1", "v1", nil)
	tr.onDeltaStreamClosed(2, nil)
	ackDelta(tr, 2, "n1", "")
	assert.Empty(t, *got, "the stream ended before the ACK")
	assert.Empty(t, tr.inflight)
}

// TestTrackerForgetsAClosedStream: what the tracker keeps per stream goes with
// the stream, so an agent that outlives many proxy reconnects holds nothing
// for the streams that ended.
func TestTrackerForgetsAClosedStream(t *testing.T) {
	tr, _ := observe(t)
	for stream := int64(1); stream <= 50; stream++ {
		openDelta(tr, stream, resourcev3.ClusterType, map[string]string{"c1": "h1"})
		openDelta(tr, stream, resourcev3.ListenerType, map[string]string{testListener: "h1"})
		// A type the stream opens and is never answered for.
		openDelta(tr, stream, resourcev3.RouteType, map[string]string{"r1": "h1"})
		sendVersioned(tr, stream, resourcev3.ClusterType, "n1", "v1", nil)
		sendVersioned(tr, stream, resourcev3.ListenerType, "n2", "v1", []string{testListener})
		ackDelta(tr, stream, "n1", "")
		tr.onDeltaStreamClosed(stream, nil)
	}
	assert.Empty(t, tr.inflight)
	assert.Empty(t, tr.answered)
	assert.Empty(t, tr.stated)
}
