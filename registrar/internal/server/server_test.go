package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
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
	return NewRegistrarServer(nil, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
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

// TestRegisterEndpointSnapshotFirst verifies write-behind semantics at the RPC
// boundary: a failing external registry must NOT fail the RPC or delay
// visibility — the snapshot and broadcast happen immediately and the external
// write is queued (the rev-68 regression: a failed external write left a
// serving pod invisible until the 60s sweep).
func TestRegisterEndpointSnapshotFirst(t *testing.T) {
	reg := &flakyRegistry{failing: true}
	s := newGateTestServer(t)
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	s.UseWriteBehind(q)

	_, err := s.RegisterEndpoint(context.Background(), &registrarv1.RegisterEndpointRequest{
		ServiceName: "svc-new",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.9"},
	})
	require.NoError(t, err, "external-registry failure must not fail the RPC")

	eps := s.snapshot.GetAll(registryv1.Service_PROTOCOL_HTTP)
	require.Contains(t, eps, "svc-new", "endpoint must be in the snapshot immediately")
	assert.True(t, q.Shielding("svc-new", "10.0.0.9"), "external write must be queued")
}

// TestWatchEndpointsFilteredSnapshot verifies a watch carrying a service
// filter receives only the filtered services' snapshot events (plus the
// SNAPSHOT_COMPLETE marker), and that an explicitly empty filter receives the
// marker alone.
func TestWatchEndpointsFilteredSnapshot(t *testing.T) {
	snap := NewSnapshot()
	snap.Apply([]*registrarv1.WatchEndpointsResponse{
		{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: "svc-a",
			Protocol:    registryv1.Service_PROTOCOL_HTTP,
			Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.1"},
		},
		{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: "svc-b",
			Protocol:    registryv1.Service_PROTOCOL_HTTP,
			Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.2"},
		},
	})
	s := NewRegistrarServer(nil, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)

	run := func(req *registrarv1.WatchEndpointsRequest) []*registrarv1.WatchEndpointsResponse {
		ctx, cancel := context.WithCancel(context.Background())
		stream := &fakeWatchServerStream{ctx: ctx}
		done := make(chan error, 1)
		go func() { done <- s.WatchEndpoints(req, stream) }()
		require.Eventually(t, func() bool {
			for _, e := range stream.sent {
				if e.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "snapshot-complete marker never sent")
		cancel()
		<-done
		return stream.sent
	}

	// The service CATALOG (every service's name) is replayed to every
	// watcher regardless of filter; endpoint snapshots stay filtered.
	countByType := func(sent []*registrarv1.WatchEndpointsResponse, t registrarv1.WatchEndpointsResponse_EventType) int {
		n := 0
		for _, e := range sent {
			if e.GetType() == t {
				n++
			}
		}
		return n
	}

	// Scoped to svc-a: one FULL_SNAPSHOT (svc-a) + full catalog + marker.
	sent := run(&registrarv1.WatchEndpointsRequest{
		Filter: &registrarv1.ServiceFilter{Services: []string{"svc-a"}},
	})
	require.Len(t, sent, 4)
	assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, sent[0].GetType())
	assert.Equal(t, "svc-a", sent[0].GetServiceName())
	assert.Equal(t, 2, countByType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED), "catalog bypasses the filter")
	assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, sent[3].GetType())

	// Explicitly empty filter: catalog + marker only (node with no mesh pods).
	sent = run(&registrarv1.WatchEndpointsRequest{
		Filter: &registrarv1.ServiceFilter{},
	})
	require.Len(t, sent, 3)
	assert.Equal(t, 0, countByType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
	assert.Equal(t, 2, countByType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED))

	// No filter: full snapshot (both services) + catalog + marker.
	sent = run(&registrarv1.WatchEndpointsRequest{})
	require.Len(t, sent, 5)
}

// fakeConfigRegistry embeds the (nil) registry.Registry interface to satisfy the type
// and implements registry.ConfigExporter; only ListConfig is exercised here.
type fakeConfigRegistry struct {
	registry.Registry
	projections []*registryv1.ServiceConfigProjection
}

func (f *fakeConfigRegistry) SetConfig(context.Context, *registryv1.ServiceConfigProjection) error {
	return nil
}
func (f *fakeConfigRegistry) UnsetConfig(context.Context, string) error { return nil }
func (f *fakeConfigRegistry) ListConfig(context.Context) ([]*registryv1.ServiceConfigProjection, error) {
	return f.projections, nil
}

// TestListAllConfig verifies the registrar surfaces config projections from a
// ConfigExporter backend (proposal 026) and degrades to empty when the backend has no
// cross-cluster config plane.
func TestListAllConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("no config plane -> empty", func(t *testing.T) {
		snap := NewSnapshot()
		s := NewRegistrarServer(nil, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
		resp, err := s.ListAllConfig(ctx, &registrarv1.ListAllConfigRequest{})
		require.NoError(t, err)
		assert.Empty(t, resp.GetProjections())
	})

	t.Run("ConfigExporter backend -> projections", func(t *testing.T) {
		reg := &fakeConfigRegistry{projections: []*registryv1.ServiceConfigProjection{
			{Service: "team-a/echo", OriginCluster: "cluster-a", Version: "v3"},
		}}
		snap := NewSnapshot()
		s := NewRegistrarServer(reg, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
		resp, err := s.ListAllConfig(ctx, &registrarv1.ListAllConfigRequest{})
		require.NoError(t, err)
		require.Len(t, resp.GetProjections(), 1)
		assert.Equal(t, "team-a/echo", resp.GetProjections()[0].GetService())
		assert.Equal(t, "cluster-a", resp.GetProjections()[0].GetOriginCluster())
	})
}
