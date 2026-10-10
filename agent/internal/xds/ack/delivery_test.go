package ack

import (
	"context"
	"log/slog"
	"testing"

	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// event is one call of either observer, in the order they were made.
type event struct {
	kind  string // "sent", "ended" or "accepted"
	names []string
}

// observeDeliveries returns a tracker and every call its two observers
// receive, in order.
func observeDeliveries(t *testing.T) (*Tracker, *[]event) {
	t.Helper()
	tr := NewTracker(slog.New(slog.DiscardHandler))
	got := &[]event{}
	names := func(resources []Resource) []string {
		out := make([]string, 0, len(resources))
		for _, r := range resources {
			out = append(out, r.Name)
		}
		return out
	}
	tr.SetDeliveryObserver(func(_ context.Context, d Delivery) {
		kind := "sent"
		if d.Ended {
			kind = "ended"
		}
		*got = append(*got, event{kind, names(d.Resources)})
	})
	tr.SetAckObserver(func(_ context.Context, a Accepted) {
		*got = append(*got, event{"accepted", names(a.Added)})
	})
	return tr, got
}

// TestDeliveryObserver_EveryResponseInFlightLeavesItOnce: a response that
// carries a resource is in flight from the moment it is written until it is
// answered or its stream ends, and the observer is told of both ends, once
// each. That is what lets the reader of an ACK keep what it knows about a
// version for exactly as long as the ACK can still come (#1508).
func TestDeliveryObserver_EveryResponseInFlightLeavesItOnce(t *testing.T) {
	tr, got := observeDeliveries(t)

	// Acknowledged: told to the AckObserver first, then it leaves flight, so
	// the AckObserver reads the ACK with what was kept for it.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"a", "b"})
	require.Equal(t, []event{{"sent", []string{"a", "b"}}}, *got)
	ackDelta(tr, 1, "n1", "")
	require.Equal(t, []event{
		{"sent", []string{"a", "b"}},
		{"accepted", []string{"a", "b"}},
		{"ended", []string{"a", "b"}},
	}, *got)
	ackDelta(tr, 1, "n1", "")
	require.Len(t, *got, 3, "an answer is taken once")

	// Rejected: nothing is accepted, and it leaves flight.
	*got = nil
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v2", []string{"a"})
	ackDelta(tr, 1, "n2", "rejected")
	require.Equal(t, []event{{"sent", []string{"a"}}, {"ended", []string{"a"}}}, *got)

	// The stream ends with responses unanswered: each leaves flight, and a
	// late answer finds nothing.
	*got = nil
	sendVersioned(tr, 1, resourcev3.ClusterType, "n3", "v3", []string{"a"})
	sendVersioned(tr, 2, resourcev3.ClusterType, "n3", "v3", []string{"b"})
	tr.onDeltaStreamClosed(1, nil)
	require.Equal(t, []event{{"sent", []string{"a"}}, {"sent", []string{"b"}}, {"ended", []string{"a"}}}, *got,
		"only the closed stream's response leaves")
	ackDelta(tr, 1, "n3", "")
	assert.Len(t, *got, 3)
	tr.onDeltaStreamClosed(2, nil)
	assert.Equal(t, event{"ended", []string{"b"}}, (*got)[3])
	assert.Empty(t, unanswered(tr))
}

// TestDeliveryObserver_AResponseThatCarriesNothingIsNotInFlight: an empty
// response puts no version in flight, whether it is kept (the opening one) or
// dropped (a later one), and a removal alone does not either.
func TestDeliveryObserver_AResponseThatCarriesNothingIsNotInFlight(t *testing.T) {
	tr, got := observeDeliveries(t)

	open(tr, 1, resourcev3.ClusterType, map[string]string{"a": "h"})
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", nil)
	ackDelta(tr, 1, "n1", "")
	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v1", nil)
	ackDelta(tr, 1, "n2", "")
	sendDelta(tr, 1, "n3", nil, []string{"gone"})
	ackDelta(tr, 1, "n3", "")
	for _, e := range *got {
		assert.Equal(t, "accepted", e.kind, "%v", e)
	}
	assert.Len(t, *got, 2, "the opening answer and the removal")
}

// TestDeliveryObserver_ARepeatedNonceEndsTheResponseItReplaces: nonces are
// unique per stream in go-control-plane. Were one ever repeated, the response
// it replaces could never be answered, and must not stay in flight for ever.
func TestDeliveryObserver_ARepeatedNonceEndsTheResponseItReplaces(t *testing.T) {
	tr, got := observeDeliveries(t)

	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"a"})
	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v2", []string{"b"})
	require.Equal(t, []event{{"sent", []string{"a"}}, {"ended", []string{"a"}}, {"sent", []string{"b"}}}, *got)
}
