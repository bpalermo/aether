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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testListener = "outbound_http_my-pod"

// sendDelta simulates the server sending a delta response carrying added and
// removed listener resources under the given nonce on the given stream.
func sendDelta(t *Tracker, streamID int64, nonce string, added, removed []string) {
	resp := &discoveryv3.DeltaDiscoveryResponse{
		TypeUrl:          resourcev3.ListenerType,
		Nonce:            nonce,
		RemovedResources: removed,
	}
	for _, name := range added {
		resp.Resources = append(resp.Resources, &discoveryv3.Resource{Name: name})
	}
	t.onDeltaResponse(streamID, nil, resp)
}

// ackDelta simulates Envoy ACKing (errMsg == "") or NACKing the response with
// the given nonce.
func ackDelta(t *Tracker, streamID int64, nonce, errMsg string) {
	req := &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:       resourcev3.ListenerType,
		ResponseNonce: nonce,
	}
	if errMsg != "" {
		req.ErrorDetail = status.New(codes.InvalidArgument, errMsg).Proto()
	}
	_ = t.onDeltaRequest(streamID, req)
}

func TestWaitListenerPresent_AckedBeforeWait(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, tr.WaitListenerPresent(ctx, testListener))
}

func TestWaitListenerPresent_AckedWhileWaiting(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- tr.WaitListenerPresent(ctx, testListener)
	}()

	// Let the waiter block, then deliver the ACK.
	time.Sleep(50 * time.Millisecond)
	ackDelta(tr, 1, "n1", "")

	require.NoError(t, <-done)
}

func TestWaitListenerPresent_TimesOutWithoutAck(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	// Response sent but never acknowledged.
	sendDelta(tr, 1, "n1", []string{testListener}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestWaitListenerPresent_NackSurfacesEnvoyError(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- tr.WaitListenerPresent(ctx, testListener)
	}()

	time.Sleep(50 * time.Millisecond)
	ackDelta(tr, 1, "n1", "cannot bind '127.0.0.1:18081' in netns: Permission denied")

	err := <-done
	require.Error(t, err)
	assert.Contains(t, err.Error(), "envoy rejected config")
	assert.Contains(t, err.Error(), "Permission denied")
}

func TestNackClearedBySubsequentAck(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	ackDelta(tr, 1, "n1", "bad config")

	// A retried update that Envoy accepts clears the rejection.
	sendDelta(tr, 1, "n2", []string{testListener}, nil)
	ackDelta(tr, 1, "n2", "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, tr.WaitListenerPresent(ctx, testListener))
}

func TestWaitListenerAbsent(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))

	t.Run("never-known listener is absent immediately", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, tr.WaitListenerAbsent(ctx, "unknown"))
	})

	t.Run("present listener blocks until removal is acked", func(t *testing.T) {
		sendDelta(tr, 1, "n1", []string{testListener}, nil)
		ackDelta(tr, 1, "n1", "")

		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done <- tr.WaitListenerAbsent(ctx, testListener)
		}()

		time.Sleep(50 * time.Millisecond)
		sendDelta(tr, 1, "n2", nil, []string{testListener})
		ackDelta(tr, 1, "n2", "")

		require.NoError(t, <-done)
	})
}

func TestStreamCloseDropsInflight(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	tr.onDeltaStreamClosed(1, nil)

	// An ACK arriving for the closed stream's nonce is ignored.
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener))
}

// observed is one call of an AckObserver, as the type and the snapshot version
// of the response that was answered.
type observed struct{ typeURL, version string }

// sendVersioned is sendDelta for any resource type, with the version of the
// snapshot the response was built from.
func sendVersioned(t *Tracker, streamID int64, typeURL, nonce, version string, added []string) {
	resp := &discoveryv3.DeltaDiscoveryResponse{TypeUrl: typeURL, Nonce: nonce, SystemVersionInfo: version}
	for _, name := range added {
		resp.Resources = append(resp.Resources, &discoveryv3.Resource{Name: name})
	}
	t.onDeltaResponse(streamID, nil, resp)
}

// TestAckObserver_ToldTheSnapshotVersionOfEveryAck: an ACK echoes only the
// nonce, so the tracker has to carry what the response was to the observer
// itself (#1425). A later NACK is never an acknowledgement, and neither is a
// response that was only sent.
func TestAckObserver_ToldTheSnapshotVersionOfEveryAck(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	var got []observed
	tr.SetAckObserver(func(_ context.Context, accepted Accepted) {
		// The tracker's lock is released by now: calling back into the tracker
		// from the observer must not deadlock.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tr.WaitListenerAbsent(ctx, "never-present")
		got = append(got, observed{accepted.TypeURL, accepted.SystemVersion})
	})

	sendVersioned(tr, 1, resourcev3.ClusterType, "n1", "v1", []string{"c1"})
	assert.Empty(t, got, "sent is not acknowledged")
	ackDelta(tr, 1, "n1", "")
	assert.Equal(t, []observed{{resourcev3.ClusterType, "v1"}}, got)

	sendVersioned(tr, 1, resourcev3.ClusterType, "n2", "v2", []string{"c1"})
	ackDelta(tr, 1, "n2", "rejected")
	assert.Len(t, got, 1, "a NACK is not told as an ACK")

	sendVersioned(tr, 1, resourcev3.ListenerType, "n3", "v2", []string{testListener})
	ackDelta(tr, 1, "n3", "")
	require.Len(t, got, 2)
	assert.Equal(t, observed{resourcev3.ListenerType, "v2"}, got[1], "every type is told; the observer picks")

	// The same nonce on another stream is another response.
	sendVersioned(tr, 1, resourcev3.ClusterType, "n4", "v3", []string{"c1"})
	ackDelta(tr, 2, "n4", "")
	assert.Len(t, got, 2, "an ACK is matched on its own stream")
	ackDelta(tr, 1, "n4", "")
	require.Len(t, got, 3)
	assert.Equal(t, observed{resourcev3.ClusterType, "v3"}, got[2])
	ackDelta(tr, 1, "n4", "")
	assert.Len(t, got, 3, "and told once")

	tr.SetAckObserver(nil)
	sendVersioned(tr, 1, resourcev3.ClusterType, "n5", "v4", []string{"c1"})
	ackDelta(tr, 1, "n5", "")
	assert.Len(t, got, 3, "no observer: nothing is told, and the tracker works as before")
}

func TestAckOnOneStreamDoesNotResolveAnother(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	// Same nonce value on a different stream must not resolve stream 1's response.
	ackDelta(tr, 2, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener))
}
