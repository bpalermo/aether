package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry"
	"aethermesh.dev/registry/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyRegistry fails registry writes until healed.
type flakyRegistry struct {
	registry.Registry // nil-embedded; only the methods below are used

	mu        sync.Mutex
	failing   bool
	registers []string // "svc/ip"
	removes   []string
}

func (f *flakyRegistry) RegisterEndpoint(_ context.Context, svc string, _ registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("InstanceNotFound: simulated external-registry failure")
	}
	f.registers = append(f.registers, svc+"/"+ep.GetIp())
	return nil
}

func (f *flakyRegistry) UnregisterEndpoint(_ context.Context, svc, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("simulated failure")
	}
	f.removes = append(f.removes, svc+"/"+ip)
	return nil
}

func (f *flakyRegistry) setFailing(v bool) {
	f.mu.Lock()
	f.failing = v
	f.mu.Unlock()
}

func (f *flakyRegistry) registered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.registers...)
}

func state(svcEps map[string][]*registryv1.ServiceEndpoint) map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint {
	out := make(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint)
	for svc, eps := range svcEps {
		out[svc] = map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
			registryv1.Service_PROTOCOL_HTTP: eps,
		}
	}
	return out
}

// TestWriteBehindFlushRetriesUntilSuccess verifies a failing external write is
// retried (not failed through to the caller) and eventually flushed.
func TestWriteBehindFlushRetriesUntilSuccess(t *testing.T) {
	reg := &flakyRegistry{failing: true}
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, &registryv1.ServiceEndpoint{Ip: "10.0.0.1"})

	q.flushDue(context.Background()) // fails; rescheduled with backoff
	require.Empty(t, reg.registered())
	assert.True(t, q.Shielding("svc-a", "10.0.0.1"), "unflushed op must shield")

	reg.setFailing(false)
	// Force the op due despite backoff, then flush.
	q.mu.Lock()
	for _, op := range q.ops {
		op.nextAttempt = time.Now().Add(-time.Second)
	}
	q.mu.Unlock()
	q.flushDue(context.Background())
	require.Equal(t, []string{"svc-a/10.0.0.1"}, reg.registered())
	assert.True(t, q.Shielding("svc-a", "10.0.0.1"), "flushed op shields until observed")
}

// TestWriteBehindOverlayShieldsAndReleases is the pending-shielding contract:
// a sync state missing a pending register gets the intent overlaid (no
// regression), and once the external registry reflects a flushed intent the
// op is released.
func TestWriteBehindOverlayShieldsAndReleases(t *testing.T) {
	reg := &flakyRegistry{failing: true}
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, ep)

	// Sync fetched a stale view without the endpoint: overlay must inject it.
	st := state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {}})
	q.Overlay(st, time.Now())
	require.Len(t, st["svc-a"][registryv1.Service_PROTOCOL_HTTP], 1, "pending register must be overlaid")

	// External write succeeds; next sync still stale: keep shielding.
	reg.setFailing(false)
	q.mu.Lock()
	for _, op := range q.ops {
		op.nextAttempt = time.Now().Add(-time.Second)
	}
	q.mu.Unlock()
	q.flushDue(context.Background())
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {}})
	q.Overlay(st, time.Now())
	require.Len(t, st["svc-a"][registryv1.Service_PROTOCOL_HTTP], 1, "flushed-but-unobserved register must still be overlaid")

	// Sync finally observes the intent: released, no further overlay.
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep}})
	q.Overlay(st, time.Now())
	assert.False(t, q.Shielding("svc-a", "10.0.0.1"), "observed intent must be released")
}

// TestWriteBehindUnregisterTombstone verifies a pending unregister removes the
// endpoint from the fetched state (no resurrection) and releases once the
// external registry reflects the removal.
func TestWriteBehindUnregisterTombstone(t *testing.T) {
	reg := &flakyRegistry{failing: true}
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.0.2"}
	q.EnqueueUnregister("svc-a", registryv1.Service_PROTOCOL_HTTP, "10.0.0.2")

	st := state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep}})
	q.Overlay(st, time.Now())
	assert.Empty(t, st["svc-a"][registryv1.Service_PROTOCOL_HTTP], "pending unregister must tombstone the fetched endpoint")

	reg.setFailing(false)
	q.mu.Lock()
	for _, op := range q.ops {
		op.nextAttempt = time.Now().Add(-time.Second)
	}
	q.mu.Unlock()
	q.flushDue(context.Background())
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {}})
	q.Overlay(st, time.Now())
	assert.False(t, q.Shielding("svc-a", "10.0.0.2"), "observed removal must release the tombstone")
}

// TestWriteBehindSupersede verifies a newer op for the same key replaces an
// older one (register then unregister -> only the unregister flushes).
func TestWriteBehindSupersede(t *testing.T) {
	reg := &flakyRegistry{}
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, &registryv1.ServiceEndpoint{Ip: "10.0.0.3"})
	q.EnqueueUnregister("svc-a", registryv1.Service_PROTOCOL_HTTP, "10.0.0.3")

	q.flushDue(context.Background())
	assert.Empty(t, reg.registered(), "superseded register must never flush")
	reg.mu.Lock()
	removes := append([]string(nil), reg.removes...)
	reg.mu.Unlock()
	assert.Equal(t, []string{"svc-a/10.0.0.3"}, removes)
}

// TestWriteBehindFlushesANewIntentWithoutWaitingForTheTick pins issue #1103's
// registrar half. A peer replica learns of an endpoint change (a drain mark
// above all) only once the external registry holds it, through its etcd
// watch. The flush loop used to write a fresh intent at the next wbTick, so
// every cross-replica drain mark waited up to 500 ms before it even left this
// replica: 0.5-0.9 s from mark to the last request a source selected the
// endpoint for, against 0.2 s when source and destination agents shared a
// replica (kind, e2e/drain-propagation.sh).
//
// Red on main: nothing is written until the first tick, 500 ms after Start.
func TestWriteBehindFlushesANewIntentWithoutWaitingForTheTick(t *testing.T) {
	reg := &flakyRegistry{}
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = q.Start(ctx) }()

	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, &registryv1.ServiceEndpoint{
		Ip:     "10.0.0.1",
		Health: registryv1.ServiceEndpoint_HEALTH_DRAINING,
	})
	require.Eventually(t, func() bool { return len(reg.registered()) == 1 }, wbTick/2, 5*time.Millisecond,
		"a fresh intent must reach the external registry well inside one wbTick (%s)", wbTick)
}

// derivedRegistry is a backend that ignores writes and derives its listing
// (registry.DerivedEndpoints), as the kubernetes backend does.
type derivedRegistry struct{ flakyRegistry }

func (*derivedRegistry) DerivesEndpoints() bool { return true }

// markFlushed sets every op flushed, as a successful external write would.
func markFlushed(q *WriteBehindQueue) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, op := range q.ops {
		op.flushed = true
	}
}

// TestWriteBehindEtcdBackendStillShieldsUntilObserved pins the etcd backend's
// release rule, which aether#1145 must not change: etcd stores what the agent
// wrote, so a flushed register is released only once a listing returns the
// agent's endpoint exactly. A listing that is newer than the intent but still
// differs (the write has not landed in what this replica read) keeps the
// intent overlaid.
func TestWriteBehindEtcdBackendStillShieldsUntilObserved(t *testing.T) {
	reg, err := backend.New(context.Background(), slog.New(slog.DiscardHandler), "etcd", backend.Config{
		EtcdEndpoints: []string{"127.0.0.1:1"}, // never dialled: no Initialize
	})
	require.NoError(t, err)
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	require.False(t, q.derived, "the etcd backend stores writes; it must not take the derived release rule")

	mark := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_DRAINING}
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, mark)
	markFlushed(q)

	// A listing taken well after the intent, still showing the old endpoint.
	stale := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}
	st := state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {stale}})
	q.Overlay(st, time.Now().Add(time.Hour))
	require.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, st["svc-a"][registryv1.Service_PROTOCOL_HTTP][0].GetHealth(),
		"etcd: an unobserved intent stays overlaid however new the listing is")
	require.True(t, q.Shielding("svc-a", "10.0.0.1"))

	// The listing returns exactly what was written: released.
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {mark}})
	q.Overlay(st, time.Now().Add(time.Hour))
	assert.False(t, q.Shielding("svc-a", "10.0.0.1"), "etcd: an observed intent is released")
}

// TestWriteBehindDerivedBackendReleasesAtTheFirstNewerListing is aether#1145's
// release rule on a derived backend (kubernetes): the listing never reflects
// the agent's endpoint field for field, so the first listing that started after
// the intent arrived releases it and is taken as is, for registers and
// unregisters alike. A listing that started BEFORE the intent (a sync in flight
// when the RPC landed) may predate the state the agent reacted to, so the
// intent is overlaid onto it exactly as before.
func TestWriteBehindDerivedBackendReleasesAtTheFirstNewerListing(t *testing.T) {
	reg, err := backend.New(context.Background(), slog.New(slog.DiscardHandler), "kubernetes", backend.Config{})
	require.NoError(t, err)
	require.True(t, NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil).derived,
		"the kubernetes backend derives every endpoint from its Pod")

	q := NewWriteBehindQueue(&derivedRegistry{}, slog.New(slog.DiscardHandler), nil)
	before := time.Now()
	agentView := &registryv1.ServiceEndpoint{
		Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_DRAINING,
		HealthCheckMode: registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS,
	}
	podView := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, agentView)
	q.EnqueueUnregister("svc-a", registryv1.Service_PROTOCOL_HTTP, "10.0.0.2")
	q.flushDue(context.Background())

	// A listing that started before the intents: overlaid, as on every backend.
	gone := &registryv1.ServiceEndpoint{Ip: "10.0.0.2"}
	st := state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {podView, gone}})
	q.Overlay(st, before)
	eps := st["svc-a"][registryv1.Service_PROTOCOL_HTTP]
	require.Len(t, eps, 1, "a pending unregister tombstones a listing that predates it")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, eps[0].GetHealth(),
		"a listing that predates the mark must not regress it")
	require.True(t, q.Shielding("svc-a", "10.0.0.1"))
	require.True(t, q.Shielding("svc-a", "10.0.0.2"))

	// The first listing that started after them: released, the Pod decides.
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {podView, gone}})
	q.Overlay(st, time.Now())
	eps = st["svc-a"][registryv1.Service_PROTOCOL_HTTP]
	require.Len(t, eps, 2)
	assert.Same(t, podView, eps[0], "the Pod-derived endpoint is served as listed")
	assert.False(t, q.Shielding("svc-a", "10.0.0.1"))
	assert.False(t, q.Shielding("svc-a", "10.0.0.2"))
}
