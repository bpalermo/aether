package server

import (
	"context"
	"errors"
	"testing"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRegistry is a test double for the registry.Registry interface.
// All methods are no-ops except ListAllEndpoints, which delegates to a
// configurable function field.
type mockRegistry struct {
	listAllEndpointsFunc func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
}

func (m *mockRegistry) Initialize(_ context.Context) error { return nil }
func (m *mockRegistry) Close() error                       { return nil }
func (m *mockRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	return nil
}
func (m *mockRegistry) UnregisterEndpoint(_ context.Context, _ string, _ string) error { return nil }
func (m *mockRegistry) UnregisterEndpoints(_ context.Context, _ string, _ []string) error {
	return nil
}

func (m *mockRegistry) ListEndpoints(_ context.Context, _ string, _ registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func (m *mockRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	if m.listAllEndpointsFunc != nil {
		return m.listAllEndpointsFunc(ctx, protocol)
	}
	return nil, nil
}

// newTestSyncer creates a Syncer wired up with a fresh Snapshot and Broadcaster
// for use in tests. It returns the Syncer, Snapshot, and Broadcaster so tests
// can inspect post-sync state directly.
func newTestSyncer(reg *mockRegistry, interval time.Duration) (*Syncer, *Snapshot, *Broadcaster) {
	log := logr.Discard()
	snap := NewSnapshot()
	bc := NewBroadcaster(log, nil)
	s := NewSyncer(reg, snap, bc, interval, log, nil)
	return s, snap, bc
}

// TestSyncer_Start_InitialSync verifies that Start performs a sync immediately
// before the first ticker tick and that the snapshot is populated with the
// endpoints returned by the registry.
func TestSyncer_Start_InitialSync(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Port: 8080, Weight: 100}

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"frontend": {ep},
			}, nil
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 10*time.Second) // long interval so only initial sync fires

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Give the initial sync time to complete, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)

	// Snapshot must contain the endpoint that was returned by the registry.
	result := snap.GetAll(registryv1.Service_PROTOCOL_HTTP)
	require.Len(t, result, 1)
	require.Len(t, result["frontend"], 1)
	assert.Equal(t, "10.0.0.1", result["frontend"][0].GetIp())
}

// TestSyncer_Start_VersionAdvancesOnSync verifies that the snapshot version is
// incremented after the initial sync, indicating Replace was called.
func TestSyncer_Start_VersionAdvancesOnSync(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.0.2", Port: 8080, Weight: 100}

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc": {ep},
			}, nil
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 10*time.Second)

	versionBefore := snap.Version()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)

	assert.NotEqual(t, versionBefore, snap.Version(), "version should advance after initial sync")
}

// TestSyncer_Start_BroadcastsChangesOnSubsequentSync verifies that when the
// registry returns a different set of endpoints on a second sync, the Syncer
// broadcasts the corresponding events to subscribed watchers.
func TestSyncer_Start_BroadcastsChangesOnSubsequentSync(t *testing.T) {
	callCount := 0

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			callCount++
			if callCount == 1 {
				// First call: single endpoint.
				return map[string][]*registryv1.ServiceEndpoint{
					"svc": {{Ip: "10.0.1.1", Port: 8080, Weight: 100}},
				}, nil
			}
			// Second call: endpoint replaced with a new one, triggering events.
			return map[string][]*registryv1.ServiceEndpoint{
				"svc": {{Ip: "10.0.1.2", Port: 8080, Weight: 100}},
			}, nil
		},
	}

	// Use a short interval so the second sync fires quickly.
	syncer, _, bc := newTestSyncer(reg, 60*time.Millisecond)

	// Subscribe before starting so we catch broadcast events.
	eventCh := bc.Subscribe("test-watcher", nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Wait long enough for the initial sync and at least one ticker sync.
	time.Sleep(200 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)
	bc.Unsubscribe("test-watcher", eventCh)

	// Count buffered events. Use len() rather than ranging over the channel to
	// avoid blocking; the broadcaster channel is buffered so events already
	// delivered can be counted immediately without risk of hanging.
	received := len(eventCh)
	assert.Greater(t, received, 0, "expected at least one broadcast event due to endpoint change")
}

// TestSyncer_Start_EmptyRegistryPopulatesEmptySnapshot verifies that a registry
// returning no endpoints results in an empty but valid snapshot.
func TestSyncer_Start_EmptyRegistryPopulatesEmptySnapshot(t *testing.T) {
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)

	result := snap.GetAll(registryv1.Service_PROTOCOL_HTTP)
	assert.Empty(t, result)
	// Version is still bumped even for an empty sync.
	assert.NotEqual(t, "0", snap.Version())
}

// TestSyncer_Start_RegistryErrorDoesNotCrash verifies that a registry error
// on the initial sync is handled gracefully: Start must not return an error
// and must not panic. The snapshot remains in its prior state.
func TestSyncer_Start_RegistryErrorDoesNotCrash(t *testing.T) {
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return nil, errors.New("registry unavailable")
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	// Start must return nil even when registry calls fail.
	require.NoError(t, <-done)

	// Snapshot should remain empty since the sync was skipped on error.
	result := snap.GetAll(registryv1.Service_PROTOCOL_HTTP)
	assert.Empty(t, result)
}

// TestSyncer_Start_RegistryErrorOnSubsequentSyncDoesNotCrash verifies that a
// registry error on a periodic (non-initial) sync is handled gracefully, with
// the snapshot retaining its last known good state.
func TestSyncer_Start_RegistryErrorOnSubsequentSyncDoesNotCrash(t *testing.T) {
	callCount := 0

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			callCount++
			if callCount == 1 {
				return map[string][]*registryv1.ServiceEndpoint{
					"svc": {{Ip: "10.0.2.1", Port: 8080, Weight: 100}},
				}, nil
			}
			return nil, errors.New("transient registry error")
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 60*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Allow initial sync to complete and at least one failing periodic sync.
	time.Sleep(200 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)

	// Snapshot should still contain the endpoint from the first successful sync.
	result := snap.GetAll(registryv1.Service_PROTOCOL_HTTP)
	require.Len(t, result["svc"], 1)
	assert.Equal(t, "10.0.2.1", result["svc"][0].GetIp())
}

// TestSyncer_Start_StopsOnContextCancellation verifies that Start returns nil
// promptly when the context is cancelled.
func TestSyncer_Start_StopsOnContextCancellation(t *testing.T) {
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}

	syncer, _, _ := newTestSyncer(reg, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start did not stop within 500ms after context cancellation")
	}
}

// TestSyncer_Start_MultipleEndpointsAcrossServices verifies that when the
// registry returns endpoints for multiple services, all of them are stored in
// the snapshot after the initial sync.
func TestSyncer_Start_MultipleEndpointsAcrossServices(t *testing.T) {
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"frontend": {
					{Ip: "10.0.3.1", Port: 8080, Weight: 100},
					{Ip: "10.0.3.2", Port: 8080, Weight: 100},
				},
				"backend": {
					{Ip: "10.0.4.1", Port: 9090, Weight: 100},
				},
			}, nil
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)

	result := snap.GetAll(registryv1.Service_PROTOCOL_HTTP)
	assert.Len(t, result, 2)
	assert.Len(t, result["frontend"], 2)
	assert.Len(t, result["backend"], 1)
}

// TestSyncer_Start_NoEventsAfterInitialSync verifies that no events are
// broadcast on subsequent syncs when the registry state is unchanged.
func TestSyncer_Start_NoEventsAfterInitialSync(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.5.1", Port: 8080, Weight: 100}

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc": {ep},
			}, nil
		},
	}

	syncer, _, bc := newTestSyncer(reg, 60*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Let initial sync complete, then subscribe to capture only subsequent events.
	time.Sleep(50 * time.Millisecond)
	eventCh := bc.Subscribe("no-change-watcher", nil)

	// Let at least two more syncs fire.
	time.Sleep(200 * time.Millisecond)
	cancel()

	require.NoError(t, <-done)
	bc.Unsubscribe("no-change-watcher", eventCh)

	// Because the state never changed after the first sync, no events should
	// have been broadcast to the watcher.
	assert.Equal(t, 0, len(eventCh), "expected no events when state is unchanged between syncs")
}
