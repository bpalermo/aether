package registrar

import (
	"context"
	"io"
	"testing"
	"time"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func TestApplyEvent_SignalsChange(t *testing.T) {
	r := newTestRegistry()

	r.applyEvent(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-a",
		Endpoint:    makeEndpoint("10.0.0.1", 8080),
	})

	select {
	case <-r.Changes():
	default:
		t.Fatal("expected a change signal after applyEvent")
	}
}

func TestSignalChange_Coalesces(t *testing.T) {
	r := newTestRegistry()

	// A burst of events must collapse into a single pending signal (the notify
	// channel is buffered with capacity 1 and written non-blocking).
	for range 5 {
		r.applyEvent(&registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: "svc-a",
			Endpoint:    makeEndpoint("10.0.0.1", 8080),
		})
	}

	<-r.Changes() // exactly one signal is pending
	select {
	case <-r.Changes():
		t.Fatal("expected change signals to coalesce into one")
	default:
	}
}

// fakeWatchStream feeds canned events to processStream, then returns err.
// Only Recv is used by processStream; the embedded nil interface panics on
// anything else, which would indicate a test assumption broke.
type fakeWatchStream struct {
	registrarv1.RegistrarService_WatchEndpointsClient
	events []*registrarv1.WatchEndpointsResponse
	err    error
}

func (f *fakeWatchStream) Recv() (*registrarv1.WatchEndpointsResponse, error) {
	if len(f.events) > 0 {
		e := f.events[0]
		f.events = f.events[1:]
		return e, nil
	}
	return nil, f.err
}

func TestProcessStream_TracksLastVersion(t *testing.T) {
	r := newTestRegistry()
	stream := &fakeWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
				ServiceName: "svc-a",
				Endpoint:    makeEndpoint("10.0.0.1", 8080),
				Version:     "7",
			},
		},
		err: io.EOF,
	}

	got := r.processStream(context.Background(), stream, "5")
	assert.Equal(t, "7", got, "lastVersion must advance to the newest event version")
}

// TestProcessStream_DataLossClearsResumeToken verifies the force-resync
// contract: when the registrar ends the stream with DataLoss (event buffer
// overflowed), the client must NOT resume from its last seen version —
// events within a batch share one version, so lastVersion can equal the
// registrar's current version while events were still missed. Clearing the
// token makes the reconnect fetch a full snapshot.
func TestProcessStream_DataLossClearsResumeToken(t *testing.T) {
	r := newTestRegistry()
	stream := &fakeWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
				ServiceName: "svc-a",
				Endpoint:    makeEndpoint("10.0.0.1", 8080),
				Version:     "7",
			},
		},
		err: status.Error(codes.DataLoss, "watch stream overflowed; reconnect for a full snapshot"),
	}

	got := r.processStream(context.Background(), stream, "5")
	assert.Equal(t, "", got, "DataLoss must clear the resume token to force a full snapshot")
}

func TestProcessStream_OtherErrorsKeepResumeToken(t *testing.T) {
	r := newTestRegistry()
	stream := &fakeWatchStream{
		err: status.Error(codes.Unavailable, "connection reset"),
	}

	got := r.processStream(context.Background(), stream, "5")
	assert.Equal(t, "5", got, "transient errors must keep the resume token")
}

// TestSnapshotCompleteClosesReady verifies WaitReady unblocks once
// processStream sees the SNAPSHOT_COMPLETE marker (Gap 2 of the rev-66 404
// fix): consumers deriving config from the watch cache wait until it holds a
// complete world view, the marker advances the resume token, and it mutates
// no cache state.
func TestSnapshotCompleteClosesReady(t *testing.T) {
	r := newTestRegistry()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	require.Error(t, r.WaitReady(ctx), "WaitReady must block before SNAPSHOT_COMPLETE")
	cancel()

	stream := &fakeWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
				ServiceName: "svc-a",
				Endpoint:    makeEndpoint("10.0.0.1", 8080),
				Version:     "7",
			},
			{
				Type:    registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
				Version: "7",
			},
			// A second marker (reconnect) must be harmless.
			{
				Type:    registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
				Version: "8",
			},
		},
		err: io.EOF,
	}

	got := r.processStream(context.Background(), stream, "")
	assert.Equal(t, "8", got, "marker version must advance the resume token")
	require.NoError(t, r.WaitReady(context.Background()), "ready must be closed after the marker")

	eps, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	assert.Len(t, eps, 1, "marker must not mutate the cache")
}

// TestReconnectSignal verifies the watch loop's reconnect notification fires
// (coalesced) so the agent can re-assert local registrations a failed-over
// registrar replica may have lost from its write-behind queue.
func TestReconnectSignal(t *testing.T) {
	r := newTestRegistry()
	select {
	case <-r.Reconnects():
		t.Fatal("no reconnect signal expected before any connection")
	default:
	}
	r.signalReconnect()
	r.signalReconnect() // coalesces, must not block
	select {
	case <-r.Reconnects():
	default:
		t.Fatal("reconnect signal expected")
	}
}

// TestSetServiceFilter_ReassertsOnChangeOnly verifies the filter setter
// cancels the active stream only when the effective set changes (order
// -insensitive), and that the generation bump clears the resume token path.
func TestSetServiceFilter_ReassertsOnChangeOnly(t *testing.T) {
	r := NewRegistrarRegistry(logr.Discard(), Config{Address: "test"})

	cancels := 0
	r.filterMu.Lock()
	r.streamCancel = func() { cancels++ }
	r.filterMu.Unlock()

	r.SetServiceFilter([]string{"svc-a", "svc-b"})
	assert.Equal(t, 1, cancels, "first filter must cancel the stream")

	// Same set, different order: no re-assert.
	r.SetServiceFilter([]string{"svc-b", "svc-a"})
	assert.Equal(t, 1, cancels, "unchanged filter must not cancel")

	// Shrink: re-assert.
	r.SetServiceFilter([]string{"svc-a"})
	assert.Equal(t, 2, cancels)

	// Empty non-nil (watch nothing) differs from nil (full watch).
	r.SetServiceFilter([]string{})
	assert.Equal(t, 3, cancels)
	r.SetServiceFilter(nil)
	assert.Equal(t, 4, cancels)

	services, gen := r.currentFilter()
	assert.Nil(t, services)
	assert.Equal(t, uint64(4), gen)
}
