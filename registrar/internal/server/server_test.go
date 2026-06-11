package server

import (
	"context"
	"testing"
	"time"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeWatchServerStream captures events sent by WatchEndpoints. Only Send and
// Context are used; the embedded nil interface panics on anything else.
type fakeWatchServerStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*registrarv1.WatchEndpointsResponse
}

func (f *fakeWatchServerStream) Send(e *registrarv1.WatchEndpointsResponse) error {
	f.sent = append(f.sent, e)
	return nil
}
func (f *fakeWatchServerStream) Context() context.Context { return f.ctx }

func newGateTestServer(t *testing.T) *RegistrarServer {
	t.Helper()
	snap := NewSnapshot()
	snap.Apply([]*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-a",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.1"},
	}})
	return NewRegistrarServer(nil, snap, NewBroadcaster(logr.Discard(), nil), "127.0.0.1:0", logr.Discard(), nil)
}

// TestWatchEndpointsSendsSnapshotComplete verifies the snapshot boundary
// marker (Gap 2 of the rev-66 404 fix): after the initial FULL_SNAPSHOT
// events, the server sends SNAPSHOT_COMPLETE with the snapshot version so
// clients know their cache holds a complete world view.
func TestWatchEndpointsSendsSnapshotComplete(t *testing.T) {
	s := newGateTestServer(t)
	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)

	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeWatchServerStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{}, stream) }()

	require.Eventually(t, func() bool { return len(stream.sent) >= 2 },
		5*time.Second, 10*time.Millisecond, "snapshot + marker never sent")
	cancel()
	<-done

	last := stream.sent[len(stream.sent)-1]
	assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, last.GetType())
	assert.NotEmpty(t, last.GetVersion(), "marker must carry the snapshot version")
	assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, stream.sent[0].GetType())
}

// TestWatchEndpointsGatedOnFirstSync verifies a freshly started registrar
// does not serve a snapshot before its first external-registry sync (the
// co-roll amplifier of the rev-66 404 gap): the RPC blocks until the gate
// opens, then serves.
func TestWatchEndpointsGatedOnFirstSync(t *testing.T) {
	s := newGateTestServer(t)
	synced := make(chan struct{})
	s.GateOnSync(synced)

	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeWatchServerStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{}, stream) }()

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, stream.sent, "nothing may be served before the first sync")

	close(synced) // first sync completes
	require.Eventually(t, func() bool { return len(stream.sent) >= 2 },
		5*time.Second, 10*time.Millisecond, "snapshot not served after gate opened")
	cancel()
	<-done
}

// TestListAllEndpointsGatedOnFirstSync verifies the agent's startup RPC
// fallback is gated the same way.
func TestListAllEndpointsGatedOnFirstSync(t *testing.T) {
	s := newGateTestServer(t)
	synced := make(chan struct{})
	s.GateOnSync(synced)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.ListAllEndpoints(ctx, &registrarv1.ListAllEndpointsRequest{Protocol: registryv1.Service_PROTOCOL_HTTP})
	require.Error(t, err, "must block (and here time out) before the first sync")

	close(synced)
	resp, err := s.ListAllEndpoints(context.Background(), &registrarv1.ListAllEndpointsRequest{Protocol: registryv1.Service_PROTOCOL_HTTP})
	require.NoError(t, err)
	assert.Len(t, resp.GetServices(), 1)
}
