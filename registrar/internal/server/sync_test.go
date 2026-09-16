package server

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventuallyTimeout / eventuallyTick bound the condition waits below. They are
// generous upper bounds, not expected durations: every wait returns as soon as
// the condition holds, so a healthy run costs microseconds and only a genuine
// hang pays the timeout.
const (
	eventuallyTimeout = 5 * time.Second
	eventuallyTick    = time.Millisecond
)

// mockRegistry is a test double for the registry.Registry interface.
// All methods are no-ops except ListAllEndpoints, which delegates to a
// configurable function field.
type mockRegistry struct {
	listAllEndpointsFunc func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)

	// listCalls counts ListAllEndpoints invocations. Tests wait on it instead
	// of sleeping when the sync they need to observe has no other externally
	// visible effect (an erroring sync writes nothing to the snapshot).
	listCalls atomic.Int64
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
	m.listCalls.Add(1)
	if m.listAllEndpointsFunc != nil {
		return m.listAllEndpointsFunc(ctx, protocol)
	}
	return nil, nil
}

// newTestSyncer creates a Syncer wired up with a fresh Snapshot and Broadcaster
// for use in tests. It returns the Syncer, Snapshot, and Broadcaster so tests
// can inspect post-sync state directly.
func newTestSyncer(reg *mockRegistry, interval time.Duration) (*Syncer, *Snapshot, *Broadcaster) {
	log := slog.New(slog.DiscardHandler)
	snap := NewSnapshot()
	bc := NewBroadcaster(log, nil)
	s := NewSyncer(reg, snap, bc, interval, log, nil)
	return s, snap, bc
}

// requireSynced waits for the Syncer's first successful sync cycle using the
// Synced() channel the production code already exposes for exactly this
// purpose, rather than guessing at a duration.
func requireSynced(t *testing.T, s *Syncer) {
	t.Helper()
	select {
	case <-s.Synced():
	case <-time.After(eventuallyTimeout):
		t.Fatal("initial sync did not complete")
	}
}

// requireListCalls waits until the registry has served at least n
// ListAllEndpoints calls. Each sync cycle lists every protocol in
// syncedProtocols, so n counts calls, not cycles.
func requireListCalls(t *testing.T, reg *mockRegistry, n int64) {
	t.Helper()
	require.Eventually(t, func() bool { return reg.listCalls.Load() >= n },
		eventuallyTimeout, eventuallyTick,
		"registry never served at least %d ListAllEndpoints calls", n)
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

	// Wait for the initial sync to complete, then cancel.
	requireSynced(t, syncer)
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

	requireSynced(t, syncer)
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
	syncer, _, bc := newTestSyncer(reg, 20*time.Millisecond)

	// Subscribe before starting so we catch broadcast events.
	eventCh := bc.Subscribe("test-watcher", nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Wait for the endpoint change to reach the watcher instead of guessing how
	// long two sync cycles take. len() on a buffered channel is race-free.
	require.Eventually(t, func() bool { return len(eventCh) > 0 }, eventuallyTimeout, eventuallyTick,
		"expected at least one broadcast event due to endpoint change")
	cancel()

	require.NoError(t, <-done)
	bc.Unsubscribe("test-watcher", eventCh)

	assert.NotEmpty(t, eventCh, "expected at least one broadcast event due to endpoint change")
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

	requireSynced(t, syncer)
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

	// A failing cycle neither closes Synced() nor writes the snapshot, so the
	// registry call itself is the only observable signal that it ran.
	requireListCalls(t, reg, 1)
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
	// Each sync cycle now lists every protocol (HTTP then TCP). Count cycles by
	// the HTTP call so the first cycle succeeds (HTTP data + empty TCP) and every
	// subsequent cycle fails transiently. Atomic because the test goroutine reads
	// it to decide when enough cycles have run.
	var httpCalls atomic.Int64

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				if httpCalls.Load() <= 1 {
					return map[string][]*registryv1.ServiceEndpoint{}, nil
				}
				return nil, errors.New("transient registry error")
			}
			if httpCalls.Add(1) == 1 {
				return map[string][]*registryv1.ServiceEndpoint{
					"svc": {{Ip: "10.0.2.1", Port: 8080, Weight: 100}},
				}, nil
			}
			return nil, errors.New("transient registry error")
		},
	}

	syncer, snap, _ := newTestSyncer(reg, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Initial sync, then at least two failing periodic cycles.
	requireSynced(t, syncer)
	require.Eventually(t, func() bool { return httpCalls.Load() >= 3 }, eventuallyTimeout, eventuallyTick,
		"periodic syncs never ran after the initial one")
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

	// Cancel only once the loop is actually up, so this exercises the select's
	// ctx.Done() arm and not a cancellation observed before Start ran.
	requireSynced(t, syncer)
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

	requireSynced(t, syncer)
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

	syncer, _, bc := newTestSyncer(reg, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- syncer.Start(ctx)
	}()

	// Let the initial sync complete, then subscribe so only subsequent syncs
	// can contribute events.
	requireSynced(t, syncer)
	eventCh := bc.Subscribe("no-change-watcher", nil)

	// Let at least two more cycles fire (one list call per synced protocol).
	requireListCalls(t, reg, reg.listCalls.Load()+2*int64(len(syncedProtocols)))
	cancel()

	require.NoError(t, <-done)
	bc.Unsubscribe("no-change-watcher", eventCh)

	// Because the state never changed after the first sync, no events should
	// have been broadcast to the watcher.
	assert.Empty(t, eventCh, "expected no events when state is unchanged between syncs")
}

// notifyMockRegistry adds a controllable Changes() channel to mockRegistry to
// exercise the Syncer's watch-driven (vs poll-driven) path.
type notifyMockRegistry struct {
	*mockRegistry
	ch chan struct{}
}

func (n *notifyMockRegistry) Changes() <-chan struct{} { return n.ch }

// TestSyncer_ChangeDrivenSync verifies that a ChangeNotifier registry triggers
// a sync via the watch signal (debounced) well before the poll interval would.
func TestSyncer_ChangeDrivenSync(t *testing.T) {
	snap := NewSnapshot()
	bc := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
	var calls atomic.Int64
	base := &mockRegistry{listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		calls.Add(1)
		return map[string][]*registryv1.ServiceEndpoint{}, nil
	}}
	reg := &notifyMockRegistry{mockRegistry: base, ch: make(chan struct{}, 1)}

	// Long poll interval so any timely sync must come from the change signal.
	s := NewSyncer(reg, snap, bc, 1*time.Hour, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()

	// Wait for the initial sync.
	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	before := calls.Load()

	// Fire a change → expect a debounced sync within ~1s, not 1h.
	reg.ch <- struct{}{}
	require.Eventually(t, func() bool { return calls.Load() > before }, 2*time.Second, 10*time.Millisecond,
		"change signal must drive a sync ahead of the poll interval")
}
