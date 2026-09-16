package registrar

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The watch-apply path mutates a service's endpoint slice IN PLACE — an update
// overwrites eps[i], a removal left-shifts the tail over the hole. Any slice a
// reader still holds shares that backing array, so the two races here are real
// even without the detector: a reader ranging the array while the left-shift
// runs can see one endpoint twice and miss another, which is corrupt EDS
// content, not just a detector report.
//
// These tests interleave the two goroutines the way production does:
// agent/internal/xds/cache/cluster.go reads through ListEndpoints /
// ListAllEndpoints on the xDS rebuild goroutine (and appends cold-filled
// entries into what it reads) while the watch stream applies events on its own
// goroutine. Both fail under --config=race before the readers clone (#772, S2).

const aliasIterations = 200

// applyChurn drives the in-place-mutating half of the cache: an UPDATE that
// overwrites eps[i] followed by a REMOVE that left-shifts the tail.
func applyChurn(r *RegistrarRegistry, service string) {
	r.applyEvent(context.Background(), &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED,
		ServiceName: service,
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    makeEndpoint("10.0.0.1", 9090),
	})
	r.applyEvent(context.Background(), &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
		ServiceName: service,
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    makeEndpoint("10.0.0.1", 9090),
	})
	// Restore the removed endpoint so the next iteration has something to
	// overwrite and left-shift again.
	r.applyEvent(context.Background(), &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: service,
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    makeEndpoint("10.0.0.1", 8080),
	})
}

func TestListEndpoints_ReturnedSliceIsCallerOwned(t *testing.T) {
	const service = "default/svc-a"
	r := newTestRegistry()
	r.cache[registryv1.Service_PROTOCOL_HTTP] = map[string][]*registryv1.ServiceEndpoint{
		service: {makeEndpoint("10.0.0.1", 8080), makeEndpoint("10.0.0.2", 8080)},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range aliasIterations {
			applyChurn(r, service)
		}
	}()
	go func() {
		defer wg.Done()
		for range aliasIterations {
			eps, err := r.ListEndpoints(context.Background(), service, registryv1.Service_PROTOCOL_HTTP)
			assert.NoError(t, err)
			// The cold path appends to what it read (cluster.go's RPC-fill
			// shape); a caller-owned slice makes that safe.
			eps = append(eps, makeEndpoint("10.0.0.9", 8080))
			for _, ep := range eps {
				_ = ep.GetIp()
			}
		}
	}()
	wg.Wait()

	// The consumer's append must never have landed in the cache.
	r.mu.RLock()
	cached := r.cache[registryv1.Service_PROTOCOL_HTTP][service]
	r.mu.RUnlock()
	for _, ep := range cached {
		assert.NotEqual(t, "10.0.0.9", ep.GetIp(), "consumer append leaked into the cache")
	}
}

func TestListAllEndpoints_ReturnedSlicesAreCallerOwned(t *testing.T) {
	const service = "default/svc-a"
	r := newTestRegistry()
	r.cache[registryv1.Service_PROTOCOL_HTTP] = map[string][]*registryv1.ServiceEndpoint{
		service: {makeEndpoint("10.0.0.1", 8080), makeEndpoint("10.0.0.2", 8080)},
	}
	close(r.ready) // serve from the cache, not the RPC fallback

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range aliasIterations {
			applyChurn(r, service)
		}
	}()
	go func() {
		defer wg.Done()
		for range aliasIterations {
			all, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
			assert.NoError(t, err)
			for name, eps := range all {
				all[name] = append(eps, makeEndpoint("10.0.0.9", 8080))
				for _, ep := range all[name] {
					_ = ep.GetIp()
				}
			}
		}
	}()
	wg.Wait()

	r.mu.RLock()
	cached := r.cache[registryv1.Service_PROTOCOL_HTTP][service]
	r.mu.RUnlock()
	for _, ep := range cached {
		assert.NotEqual(t, "10.0.0.9", ep.GetIp(), "consumer append leaked into the cache")
	}
}

// TestListEndpoints_CloneIsShallow pins the documented contract: the slice is
// the caller's, the *ServiceEndpoint elements are shared and immutable.
func TestListEndpoints_CloneIsShallow(t *testing.T) {
	const service = "default/svc-a"
	r := NewRegistrarRegistry(slog.New(slog.DiscardHandler), Config{Address: "test"})
	ep := makeEndpoint("10.0.0.1", 8080)
	r.cache[registryv1.Service_PROTOCOL_HTTP] = map[string][]*registryv1.ServiceEndpoint{service: {ep}}

	got, err := r.ListEndpoints(context.Background(), service, registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Same(t, ep, got[0], "elements are shared by reference (no deep clone)")

	r.mu.RLock()
	cached := r.cache[registryv1.Service_PROTOCOL_HTTP][service]
	r.mu.RUnlock()
	require.Len(t, cached, 1)
	assert.NotSame(t, &cached[0], &got[0], "the slice header/backing array must not be shared")
}
