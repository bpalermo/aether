package ack

import (
	"context"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
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
	tr := NewTracker(logr.Discard())
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, tr.WaitListenerPresent(ctx, testListener))
}

func TestWaitListenerPresent_AckedWhileWaiting(t *testing.T) {
	tr := NewTracker(logr.Discard())
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
	tr := NewTracker(logr.Discard())
	// Response sent but never acknowledged.
	sendDelta(tr, 1, "n1", []string{testListener}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := tr.WaitListenerPresent(ctx, testListener)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestWaitListenerPresent_NackSurfacesEnvoyError(t *testing.T) {
	tr := NewTracker(logr.Discard())
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
	tr := NewTracker(logr.Discard())
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
	tr := NewTracker(logr.Discard())

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
	tr := NewTracker(logr.Discard())
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	tr.onDeltaStreamClosed(1, nil)

	// An ACK arriving for the closed stream's nonce is ignored.
	ackDelta(tr, 1, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener))
}

func TestAckOnOneStreamDoesNotResolveAnother(t *testing.T) {
	tr := NewTracker(logr.Discard())
	sendDelta(tr, 1, "n1", []string{testListener}, nil)
	// Same nonce value on a different stream must not resolve stream 1's response.
	ackDelta(tr, 2, "n1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener))
}
