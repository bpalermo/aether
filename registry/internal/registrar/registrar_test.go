package registrar

import (
	"context"
	"testing"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRegistry() *RegistrarRegistry {
	return NewRegistrarRegistry(logr.Discard(), Config{Address: "localhost:0"})
}

func makeEndpoint(ip string, port uint32) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:   ip,
		Port: port,
	}
}

func TestNewRegistrarRegistry_EmptyCache(t *testing.T) {
	r := newTestRegistry()
	assert.Empty(t, r.cache)
}

func TestApplyEvent_FullSnapshot_AddsToCache(t *testing.T) {
	ep := makeEndpoint("10.0.0.1", 8080)

	tests := []struct {
		name        string
		serviceName string
		endpoint    *registryv1.ServiceEndpoint
		expected    map[string][]*registryv1.ServiceEndpoint
	}{
		{
			name:        "full snapshot adds endpoint to empty cache",
			serviceName: "svc-a",
			endpoint:    ep,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry()
			r.applyEvent(&registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
				ServiceName: tt.serviceName,
				Endpoint:    tt.endpoint,
			})
			assert.Equal(t, tt.expected, r.cache)
		})
	}
}

func TestApplyEvent_EndpointAdded(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1", 8080)
	ep2 := makeEndpoint("10.0.0.2", 8080)

	tests := []struct {
		name         string
		initialCache map[string][]*registryv1.ServiceEndpoint
		serviceName  string
		endpoint     *registryv1.ServiceEndpoint
		expected     map[string][]*registryv1.ServiceEndpoint
	}{
		{
			name:         "adds endpoint to empty service",
			initialCache: map[string][]*registryv1.ServiceEndpoint{},
			serviceName:  "svc-a",
			endpoint:     ep1,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
			},
		},
		{
			name: "adds second endpoint to existing service",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
			},
			serviceName: "svc-a",
			endpoint:    ep2,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1, ep2},
			},
		},
		{
			name:         "adds endpoint to new service alongside existing service",
			initialCache: map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep1}},
			serviceName:  "svc-b",
			endpoint:     ep2,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
				"svc-b": {ep2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry()
			r.cache = tt.initialCache
			r.applyEvent(&registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
				ServiceName: tt.serviceName,
				Endpoint:    tt.endpoint,
			})
			assert.Equal(t, tt.expected, r.cache)
		})
	}
}

func TestApplyEvent_EndpointUpdated(t *testing.T) {
	original := makeEndpoint("10.0.0.1", 8080)
	updated := &registryv1.ServiceEndpoint{
		Ip:   "10.0.0.1",
		Port: 9090,
	}
	other := makeEndpoint("10.0.0.2", 8080)

	tests := []struct {
		name         string
		initialCache map[string][]*registryv1.ServiceEndpoint
		serviceName  string
		endpoint     *registryv1.ServiceEndpoint
		expected     map[string][]*registryv1.ServiceEndpoint
	}{
		{
			name: "updates existing endpoint by IP",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {original},
			},
			serviceName: "svc-a",
			endpoint:    updated,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {updated},
			},
		},
		{
			name: "updates correct endpoint when multiple exist",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {original, other},
			},
			serviceName: "svc-a",
			endpoint:    updated,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {updated, other},
			},
		},
		{
			name: "adds endpoint if IP not found during update",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {other},
			},
			serviceName: "svc-a",
			endpoint:    updated,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {other, updated},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry()
			r.cache = tt.initialCache
			r.applyEvent(&registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED,
				ServiceName: tt.serviceName,
				Endpoint:    tt.endpoint,
			})
			assert.Equal(t, tt.expected, r.cache)
		})
	}
}

func TestApplyEvent_EndpointRemoved(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1", 8080)
	ep2 := makeEndpoint("10.0.0.2", 8080)

	tests := []struct {
		name         string
		initialCache map[string][]*registryv1.ServiceEndpoint
		serviceName  string
		endpoint     *registryv1.ServiceEndpoint
		expected     map[string][]*registryv1.ServiceEndpoint
	}{
		{
			name: "removes the only endpoint and deletes service key",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
			},
			serviceName: "svc-a",
			endpoint:    ep1,
			expected:    map[string][]*registryv1.ServiceEndpoint{},
		},
		{
			name: "removes one of multiple endpoints",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1, ep2},
			},
			serviceName: "svc-a",
			endpoint:    ep1,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep2},
			},
		},
		{
			name: "remove on non-existent IP is a no-op",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep2},
			},
			serviceName: "svc-a",
			endpoint:    ep1,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep2},
			},
		},
		{
			name:         "remove on non-existent service is a no-op",
			initialCache: map[string][]*registryv1.ServiceEndpoint{},
			serviceName:  "svc-missing",
			endpoint:     ep1,
			expected:     map[string][]*registryv1.ServiceEndpoint{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry()
			r.cache = tt.initialCache
			r.applyEvent(&registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
				ServiceName: tt.serviceName,
				Endpoint:    tt.endpoint,
			})
			assert.Equal(t, tt.expected, r.cache)
		})
	}
}

func TestRemoveLocked_CleansUpEmptyServiceKey(t *testing.T) {
	ep := makeEndpoint("10.0.0.1", 8080)
	r := newTestRegistry()
	r.cache = map[string][]*registryv1.ServiceEndpoint{
		"svc-a": {ep},
	}

	r.mu.Lock()
	r.removeLocked("svc-a", "10.0.0.1")
	r.mu.Unlock()

	_, exists := r.cache["svc-a"]
	assert.False(t, exists, "service key should be removed from cache when no endpoints remain")
	assert.Empty(t, r.cache)
}

func TestListAllEndpoints_ReadsFromCache(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1", 8080)
	ep2 := makeEndpoint("10.0.0.2", 8080)

	tests := []struct {
		name         string
		initialCache map[string][]*registryv1.ServiceEndpoint
		expected     map[string][]*registryv1.ServiceEndpoint
	}{
		{
			name: "returns all services from populated cache",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
				"svc-b": {ep2},
			},
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
				"svc-b": {ep2},
			},
		},
		{
			name: "returns single service from cache",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1, ep2},
			},
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1, ep2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry()
			r.cache = tt.initialCache
			got, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_UNSPECIFIED)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestListEndpoints_ReadsFromCache(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1", 8080)
	ep2 := makeEndpoint("10.0.0.2", 8080)

	tests := []struct {
		name         string
		initialCache map[string][]*registryv1.ServiceEndpoint
		service      string
		expected     []*registryv1.ServiceEndpoint
	}{
		{
			name: "returns endpoints for matching service",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1, ep2},
				"svc-b": {ep2},
			},
			service:  "svc-a",
			expected: []*registryv1.ServiceEndpoint{ep1, ep2},
		},
		{
			name: "returns single endpoint for service",
			initialCache: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
			},
			service:  "svc-a",
			expected: []*registryv1.ServiceEndpoint{ep1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry()
			r.cache = tt.initialCache
			got, err := r.ListEndpoints(context.Background(), tt.service, registryv1.Service_PROTOCOL_UNSPECIFIED)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCache_MultipleServices(t *testing.T) {
	r := newTestRegistry()

	ep1 := makeEndpoint("10.0.0.1", 8080)
	ep2 := makeEndpoint("10.0.0.2", 8080)
	ep3 := makeEndpoint("10.0.0.3", 9090)

	// Add endpoints across two services.
	r.applyEvent(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-a",
		Endpoint:    ep1,
	})
	r.applyEvent(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-b",
		Endpoint:    ep2,
	})
	r.applyEvent(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-a",
		Endpoint:    ep3,
	})

	assert.Equal(t, map[string][]*registryv1.ServiceEndpoint{
		"svc-a": {ep1, ep3},
		"svc-b": {ep2},
	}, r.cache)

	// Remove svc-a entirely.
	r.applyEvent(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
		ServiceName: "svc-a",
		Endpoint:    ep1,
	})
	r.applyEvent(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
		ServiceName: "svc-a",
		Endpoint:    ep3,
	})

	assert.Equal(t, map[string][]*registryv1.ServiceEndpoint{
		"svc-b": {ep2},
	}, r.cache)

	_, exists := r.cache["svc-a"]
	assert.False(t, exists, "svc-a key should be absent after all endpoints removed")
}
