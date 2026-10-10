package server

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removalNotAcked is what RemovePod logs when its wait for the proxy to drop
// the pod's listeners ended without an answer.
const removalNotAcked = "envoy did not ack listener removal"

// lockedBuffer is a log sink a test reads while the server writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRemovePod_WaitsUntilAProxySaysWhatItHolds is the CNI DEL side of #1572.
// The pod's network namespace is torn down when RemovePod returns, and both of
// the pod's listeners are bound inside it, so RemovePod must not take "the
// agent knows of no such listener" for "the proxy holds no such listener".
func TestRemovePod_WaitsUntilAProxySaysWhatItHolds(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-live", "default", "container-live")
	request := &cniv1.RemovePodRequest{ContainerId: "container-live", Name: "pod-live", Namespace: "default"}

	// server is a CNI server that stores the pod, with the given tracker.
	server := func(t *testing.T, tracker *ack.Tracker) (*CNIServer, *lockedBuffer) {
		t.Helper()
		store := storage.NewMockStorage[*cniv1.CNIPod]()
		require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), pod))
		s := newTestCNIServer(nil, store, &testRegistry{}, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")
		s.ackTracker = tracker
		logged := &lockedBuffer{}
		s.log = slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
		return s, logged
	}
	remove := func(t *testing.T, s *CNIServer) {
		t.Helper()
		resp, err := s.RemovePod(ctx, request)
		require.NoError(t, err)
		require.Equal(t, cniv1.RemovePodResponse_RESULT_SUCCESS, resp.GetResult())
	}

	t.Run("no proxy connected: the wait runs to its bound", func(t *testing.T) {
		// An agent that has just restarted: the proxy holds the listeners and
		// has not reconnected to say so.
		s, logged := server(t, ack.NewTracker(slog.New(slog.DiscardHandler)))
		start := time.Now()
		remove(t, s)
		assert.GreaterOrEqual(t, time.Since(start), envoyAckTimeout, "RemovePod returned before the wait could have ended")
		assert.Contains(t, logged.String(), removalNotAcked)
	})

	// Both of the pod's listeners are bound in its network namespace, and
	// RemovePod waits for each. A proxy that holds only one of them shows
	// that wait alone: with the other dropped from RemovePod, this case
	// would still pass for the listener the proxy does not hold.
	for name, listener := range map[string]string{
		"a proxy that holds the outbound listener: until it acknowledges its removal": proxy.OutboundListenerName(pod),
		"a proxy that holds the inbound listener: until it acknowledges its removal":  proxy.InboundListenerName(pod),
	} {
		t.Run(name, func(t *testing.T) {
			tracker := ack.NewTracker(slog.New(slog.DiscardHandler))
			proxyStatesItsListeners(tracker, 1, map[string]string{listener: "h"})
			s, logged := server(t, tracker)

			returned := make(chan struct{})
			go func() {
				defer close(returned)
				remove(t, s)
			}()
			select {
			case <-returned:
				t.Fatalf("RemovePod returned while the proxy holds %s", listener)
			case <-time.After(200 * time.Millisecond):
			}

			callbacks := tracker.Callbacks()
			callbacks.OnStreamDeltaResponse(1, nil, &discoveryv3.DeltaDiscoveryResponse{
				TypeUrl: resourcev3.ListenerType, Nonce: "removal",
				RemovedResources: []string{listener},
			})
			require.NoError(t, callbacks.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
				TypeUrl: resourcev3.ListenerType, ResponseNonce: "removal",
			}))
			<-returned
			assert.NotContains(t, logged.String(), removalNotAcked, "the removal was acknowledged within the wait")
		})
	}

	t.Run("a proxy that holds neither: at once", func(t *testing.T) {
		tracker := ack.NewTracker(slog.New(slog.DiscardHandler))
		proxyStatesItsListeners(tracker, 1, map[string]string{proxy.OutboundListenerName(validCNIPod("another-pod", "default", "container-other")): "h"})
		s, logged := server(t, tracker)
		remove(t, s)
		assert.NotContains(t, logged.String(), removalNotAcked)
	})
}
