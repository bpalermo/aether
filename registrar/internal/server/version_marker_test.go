package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1241: the registrar's version moves without any batch reaching a
// watcher -- a store revision that changes no contents, or a change outside a
// filtered watcher's filter -- and the agent's last_version (resume token and
// lag gauge) used to stay behind until an unrelated change. Each sync cycle now
// hands every such watcher the current version as a version-only
// SNAPSHOT_COMPLETE.

// openWatch starts a watch with req and returns its stream once the initial
// SNAPSHOT_COMPLETE has been sent; the stream ends with the test.
func openWatch(t *testing.T, s *RegistrarServer, req *registrarv1.WatchEndpointsRequest) *fakeWatchServerStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeWatchServerStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- s.WatchEndpoints(req, stream) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		return countType(stream.snapshot(), registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE) == 1
	}, 5*time.Second, 5*time.Millisecond, "initial marker never sent")
	return stream
}

// markersAfterStart returns the versions of the SNAPSHOT_COMPLETE events sent
// after the initial one.
func markersAfterStart(stream *fakeWatchServerStream) []string {
	var out []string
	seen := false
	for _, e := range stream.snapshot() {
		if e.GetType() != registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE {
			continue
		}
		if seen {
			out = append(out, e.GetVersion())
		}
		seen = true
	}
	return out
}

func newMarkerTestServer(t *testing.T) (*RegistrarServer, *Syncer, *revisionedRegistry) {
	t.Helper()
	reg := &revisionedRegistry{}
	reg.set(40, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}, "ns/b": {ep("10.0.1.1")}})
	syncer, snap := newRevisionedSyncer(reg)
	syncer.sync(context.Background())
	s := NewRegistrarServer(&flakyRegistry{}, snap, syncer.broadcaster, "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
	s.GateOnSync(syncer.Synced())
	return s, syncer, reg
}

// TestSync_NoOpRevisionMarksWatchers: a sync whose listing moved the store
// revision but not the contents sends each watcher the new version, once. A
// sync that moves nothing sends nothing.
func TestSync_NoOpRevisionMarksWatchers(t *testing.T) {
	s, syncer, reg := newMarkerTestServer(t)
	full := openWatch(t, s, &registrarv1.WatchEndpointsRequest{NodeName: "full"})
	scoped := openWatch(t, s, &registrarv1.WatchEndpointsRequest{NodeName: "scoped", Filter: &registrarv1.ServiceFilter{Services: []string{"ns/a"}}})

	reg.set(41, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}, "ns/b": {ep("10.0.1.1")}})
	syncer.sync(context.Background())
	want := clean(41, syncer.snapshot.State())
	for name, stream := range map[string]*fakeWatchServerStream{"full": full, "scoped": scoped} {
		require.Eventuallyf(t, func() bool { return len(markersAfterStart(stream)) == 1 },
			5*time.Second, 5*time.Millisecond, "%s: no version marker", name)
		assert.Equal(t, []string{want}, markersAfterStart(stream), name)
	}

	syncer.sync(context.Background())
	assert.Zero(t, s.broadcaster.MarkVersion(syncer.snapshot.Version), "every watcher is already at the version")
}

// TestMarkVersion_OutOfFilterChange: a change to b reaches b's consumer with
// the version on its batch; a's consumer is marked instead, and b's is not
// marked a second time.
func TestMarkVersion_OutOfFilterChange(t *testing.T) {
	s, syncer, _ := newMarkerTestServer(t)
	watchA := openWatch(t, s, &registrarv1.WatchEndpointsRequest{NodeName: "a", Filter: &registrarv1.ServiceFilter{Services: []string{"ns/a"}}})
	watchB := openWatch(t, s, &registrarv1.WatchEndpointsRequest{NodeName: "b", Filter: &registrarv1.ServiceFilter{Services: []string{"ns/b"}}})

	_, err := s.RegisterEndpoint(context.Background(), &registrarv1.RegisterEndpointRequest{
		ServiceName: "ns/b", Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: ep("10.0.1.2"),
	})
	require.NoError(t, err)
	version := syncer.snapshot.Version()

	assert.Equal(t, 1, s.broadcaster.MarkVersion(syncer.snapshot.Version), "only the watcher the batch did not reach")
	require.Eventually(t, func() bool { return len(markersAfterStart(watchA)) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{version}, markersAfterStart(watchA))
	require.Eventually(t, func() bool {
		return countType(watchB.snapshot(), registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED) == 1
	}, 5*time.Second, 5*time.Millisecond)
	assert.Empty(t, markersAfterStart(watchB), "the batch carried the version already")
}

// TestMarkVersion_WaitsForAnInFlightPublication is the lost-update negative
// (#1203/#1205): a publication has mutated the snapshot but not yet broadcast
// its batch. A marker carrying the new version must not reach the batch's
// consumer ahead of the batch, or a stream cut between the two leaves a token
// naming contents the cache lacks -- answered "current" on reconnect.
func TestMarkVersion_WaitsForAnInFlightPublication(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 5})
	b := NewBroadcaster(log, nil)
	ch := b.SubscribeWith("w", []string{"ns/a"}, snap.Version)

	mutated, release := make(chan struct{}), make(chan struct{})
	published := make(chan struct{})
	go func() {
		defer close(published)
		b.Publish(func() []*registrarv1.WatchEndpointsResponse {
			events := []*registrarv1.WatchEndpointsResponse{{
				Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "ns/a",
				Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: ep("10.0.0.2"),
			}}
			version, transitions := snap.Apply(events)
			close(mutated)
			<-release
			return stampVersion(append(events, transitions...), version)
		})
	}()
	<-mutated

	marked := make(chan int, 1)
	go func() { marked <- b.MarkVersion(snap.Version) }()
	select {
	case <-marked:
		t.Fatal("MarkVersion ran while a publication was between its mutation and its broadcast")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-published
	assert.Zero(t, <-marked, "the batch carried the version; no marker is due")

	first := <-ch
	assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, first.GetType(), "the batch comes first")
	assert.Equal(t, snap.Version(), first.GetVersion())
	select {
	case e := <-ch:
		t.Fatalf("unexpected event after the batch: %v", e)
	default:
	}
}

// TestMarkVersion_FullChannelIsSkipped: a watcher whose buffer is full misses
// the marker but is not force-resynced: a marker carries no event, so nothing
// was lost, and the next cycle retries.
func TestMarkVersion_FullChannelIsSkipped(t *testing.T) {
	b := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
	ch := b.SubscribeWith("w", []string{"ns/a"}, func() string { return "1.0123456789abcdef" })
	for range defaultChannelBuffer {
		b.Broadcast([]*registrarv1.WatchEndpointsResponse{{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "ns/a"}})
	}
	assert.Zero(t, b.MarkVersion(func() string { return "2.0123456789abcdef" }))
	assert.Equal(t, 1, b.WatcherCount(), "a skipped marker must not close the watcher")

	<-ch // room for one
	assert.Equal(t, 1, b.MarkVersion(func() string { return "2.0123456789abcdef" }), "retried on the next cycle")
	assert.Zero(t, b.MarkVersion(func() string { return "2.0123456789abcdef" }))
}
