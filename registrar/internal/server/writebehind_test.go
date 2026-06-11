package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
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
	q := NewWriteBehindQueue(reg, logr.Discard(), nil)
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
	q := NewWriteBehindQueue(reg, logr.Discard(), nil)
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, ep)

	// Sync fetched a stale view without the endpoint: overlay must inject it.
	st := state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {}})
	q.Overlay(st)
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
	q.Overlay(st)
	require.Len(t, st["svc-a"][registryv1.Service_PROTOCOL_HTTP], 1, "flushed-but-unobserved register must still be overlaid")

	// Sync finally observes the intent: released, no further overlay.
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep}})
	q.Overlay(st)
	assert.False(t, q.Shielding("svc-a", "10.0.0.1"), "observed intent must be released")
}

// TestWriteBehindUnregisterTombstone verifies a pending unregister removes the
// endpoint from the fetched state (no resurrection) and releases once the
// external registry reflects the removal.
func TestWriteBehindUnregisterTombstone(t *testing.T) {
	reg := &flakyRegistry{failing: true}
	q := NewWriteBehindQueue(reg, logr.Discard(), nil)
	ep := &registryv1.ServiceEndpoint{Ip: "10.0.0.2"}
	q.EnqueueUnregister("svc-a", registryv1.Service_PROTOCOL_HTTP, "10.0.0.2")

	st := state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep}})
	q.Overlay(st)
	assert.Empty(t, st["svc-a"][registryv1.Service_PROTOCOL_HTTP], "pending unregister must tombstone the fetched endpoint")

	reg.setFailing(false)
	q.mu.Lock()
	for _, op := range q.ops {
		op.nextAttempt = time.Now().Add(-time.Second)
	}
	q.mu.Unlock()
	q.flushDue(context.Background())
	st = state(map[string][]*registryv1.ServiceEndpoint{"svc-a": {}})
	q.Overlay(st)
	assert.False(t, q.Shielding("svc-a", "10.0.0.2"), "observed removal must release the tombstone")
}

// TestWriteBehindSupersede verifies a newer op for the same key replaces an
// older one (register then unregister -> only the unregister flushes).
func TestWriteBehindSupersede(t *testing.T) {
	reg := &flakyRegistry{}
	q := NewWriteBehindQueue(reg, logr.Discard(), nil)
	q.EnqueueRegister("svc-a", registryv1.Service_PROTOCOL_HTTP, &registryv1.ServiceEndpoint{Ip: "10.0.0.3"})
	q.EnqueueUnregister("svc-a", registryv1.Service_PROTOCOL_HTTP, "10.0.0.3")

	q.flushDue(context.Background())
	assert.Empty(t, reg.registered(), "superseded register must never flush")
	reg.mu.Lock()
	removes := append([]string(nil), reg.removes...)
	reg.mu.Unlock()
	assert.Equal(t, []string{"svc-a/10.0.0.3"}, removes)
}
