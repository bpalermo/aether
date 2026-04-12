package server

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// makeEndpoint returns a minimal ServiceEndpoint with the given IP.
func makeEndpoint(ip string) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:          ip,
		ClusterName: "cluster-a",
		Port:        8080,
	}
}

func TestNewSnapshot(t *testing.T) {
	s := NewSnapshot()
	require.NotNil(t, s)
	assert.Equal(t, "0", s.Version())
	assert.Empty(t, s.GetAll(registryv1.Service_PROTOCOL_HTTP))
}

func TestVersion(t *testing.T) {
	tests := []struct {
		name            string
		applyOperations func(s *Snapshot)
		expectedVersion string
	}{
		{
			name:            "fresh snapshot has version 0",
			applyOperations: func(s *Snapshot) {},
			expectedVersion: "0",
		},
		{
			name: "version increments after Replace",
			applyOperations: func(s *Snapshot) {
				s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{})
			},
			expectedVersion: "1",
		},
		{
			name: "version increments after Apply with events",
			applyOperations: func(s *Snapshot) {
				s.Apply([]*registrarv1.WatchEndpointsResponse{
					{
						Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
						ServiceName: "svc",
						Protocol:    registryv1.Service_PROTOCOL_HTTP,
						Endpoint:    makeEndpoint("10.0.0.1"),
					},
				})
			},
			expectedVersion: "1",
		},
		{
			name: "version does not increment after Apply with empty events",
			applyOperations: func(s *Snapshot) {
				s.Apply([]*registrarv1.WatchEndpointsResponse{})
			},
			expectedVersion: "0",
		},
		{
			name: "version increments multiple times",
			applyOperations: func(s *Snapshot) {
				s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{})
				s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{})
				s.Replace(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{})
			},
			expectedVersion: "3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			tt.applyOperations(s)
			assert.Equal(t, tt.expectedVersion, s.Version())
		})
	}
}

func TestGetAll(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1")
	ep2 := makeEndpoint("10.0.0.2")
	ep3 := makeEndpoint("10.0.0.3")

	tests := []struct {
		name     string
		seed     map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		protocol registryv1.Service_Protocol
		expected map[string][]*registryv1.ServiceEndpoint
	}{
		{
			name:     "empty snapshot returns empty map",
			seed:     map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
		},
		{
			name: "returns only endpoints matching the requested protocol",
			seed: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1, ep2},
				},
				"svc-b": {
					registryv1.Service_PROTOCOL_UNSPECIFIED: {ep3},
				},
			},
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1, ep2},
			},
		},
		{
			name: "returns nothing when no endpoints match protocol",
			seed: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			protocol: registryv1.Service_PROTOCOL_UNSPECIFIED,
			expected: map[string][]*registryv1.ServiceEndpoint{},
		},
		{
			name: "returns endpoints from multiple services for matching protocol",
			seed: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
				"svc-b": {
					registryv1.Service_PROTOCOL_HTTP: {ep2},
				},
			},
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {ep1},
				"svc-b": {ep2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			s.Replace(tt.seed)

			got := s.GetAll(tt.protocol)

			require.Len(t, got, len(tt.expected))
			for svcName, expectedEps := range tt.expected {
				gotEps, ok := got[svcName]
				require.True(t, ok, "expected service %q in result", svcName)
				assert.ElementsMatch(t, expectedEps, gotEps)
			}
		})
	}
}

func TestGetAllWithVersion(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1")

	tests := []struct {
		name            string
		seed            map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		protocol        registryv1.Service_Protocol
		expectedVersion string
		expectedKeys    []string
	}{
		{
			name:            "empty snapshot returns empty map and version 1",
			seed:            map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			protocol:        registryv1.Service_PROTOCOL_HTTP,
			expectedVersion: "1",
			expectedKeys:    nil,
		},
		{
			name: "returns endpoints and current version",
			seed: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			protocol:        registryv1.Service_PROTOCOL_HTTP,
			expectedVersion: "1",
			expectedKeys:    []string{"svc-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			s.Replace(tt.seed)

			got, version := s.GetAllWithVersion(tt.protocol)

			assert.Equal(t, tt.expectedVersion, version)
			for _, key := range tt.expectedKeys {
				assert.Contains(t, got, key)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1")
	ep2 := makeEndpoint("10.0.0.2")
	ep1Updated := &registryv1.ServiceEndpoint{
		Ip:          "10.0.0.1",
		ClusterName: "cluster-a",
		Port:        9090, // changed port
	}

	tests := []struct {
		name         string
		initial      map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		newEndpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		wantTypes    []registrarv1.WatchEndpointsResponse_EventType
	}{
		{
			name:         "empty snapshot diff with empty new returns no events",
			initial:      map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			wantTypes:    nil,
		},
		{
			name:    "adding endpoint to empty snapshot produces ADDED event",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			wantTypes: []registrarv1.WatchEndpointsResponse_EventType{
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			},
		},
		{
			name: "removing all endpoints produces REMOVED event",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			wantTypes: []registrarv1.WatchEndpointsResponse_EventType{
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
			},
		},
		{
			name: "updating endpoint produces UPDATED event",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1Updated},
				},
			},
			wantTypes: []registrarv1.WatchEndpointsResponse_EventType{
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED,
			},
		},
		{
			name: "identical endpoint produces no events",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			wantTypes: nil,
		},
		{
			name: "mixed ADDED and REMOVED events",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep2},
				},
			},
			wantTypes: []registrarv1.WatchEndpointsResponse_EventType{
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			},
		},
		{
			name: "Diff does not modify snapshot version",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			newEndpoints: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep2},
				},
			},
			// Version stays at 1 from the initial Replace call.
			wantTypes: []registrarv1.WatchEndpointsResponse_EventType{
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
				registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			s.Replace(tt.initial)

			versionBefore := s.Version()
			events := s.Diff(tt.newEndpoints)
			versionAfter := s.Version()

			// Diff must never change the version.
			assert.Equal(t, versionBefore, versionAfter, "Diff must not modify snapshot version")

			if tt.wantTypes == nil {
				assert.Empty(t, events)
				return
			}

			require.Len(t, events, len(tt.wantTypes))
			gotTypes := make([]registrarv1.WatchEndpointsResponse_EventType, len(events))
			for i, ev := range events {
				gotTypes[i] = ev.GetType()
			}
			assert.ElementsMatch(t, tt.wantTypes, gotTypes)
		})
	}
}

func TestReplace(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1")
	ep2 := makeEndpoint("10.0.0.2")

	tests := []struct {
		name            string
		initial         map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		replacement     map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		protocol        registryv1.Service_Protocol
		expectedVersion string
		expectedKeys    []string
		expectedEmpty   bool
	}{
		{
			name:            "replace on empty snapshot bumps version",
			initial:         map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			replacement:     map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			protocol:        registryv1.Service_PROTOCOL_HTTP,
			expectedVersion: "2",
			expectedEmpty:   true,
		},
		{
			name:    "replace populates snapshot",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			replacement: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			protocol:        registryv1.Service_PROTOCOL_HTTP,
			expectedVersion: "2",
			expectedKeys:    []string{"svc-a"},
		},
		{
			name: "replace removes previous entries not in the new set",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-old": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			replacement: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-new": {
					registryv1.Service_PROTOCOL_HTTP: {ep2},
				},
			},
			protocol:        registryv1.Service_PROTOCOL_HTTP,
			expectedVersion: "2",
			expectedKeys:    []string{"svc-new"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			s.Replace(tt.initial)

			newVersion := s.Replace(tt.replacement)

			assert.Equal(t, tt.expectedVersion, newVersion)
			assert.Equal(t, tt.expectedVersion, s.Version())

			got := s.GetAll(tt.protocol)
			if tt.expectedEmpty {
				assert.Empty(t, got)
			} else {
				for _, key := range tt.expectedKeys {
					assert.Contains(t, got, key)
				}
				for key := range got {
					found := false
					for _, expectedKey := range tt.expectedKeys {
						if key == expectedKey {
							found = true
							break
						}
					}
					assert.True(t, found, "unexpected service %q in snapshot after Replace", key)
				}
			}
		})
	}
}

func TestApply(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1")
	ep2 := makeEndpoint("10.0.0.2")
	ep1Updated := &registryv1.ServiceEndpoint{
		Ip:          "10.0.0.1",
		ClusterName: "cluster-a",
		Port:        9090,
	}

	tests := []struct {
		name            string
		initial         map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		events          []*registrarv1.WatchEndpointsResponse
		expectedVersion string
		checkSnapshot   func(t *testing.T, s *Snapshot)
	}{
		{
			name:    "empty events returns current version without bump",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			events:  []*registrarv1.WatchEndpointsResponse{},
			// No Replace was called; version stays at 0.
			expectedVersion: "0",
			checkSnapshot:   func(t *testing.T, s *Snapshot) {},
		},
		{
			name:            "nil events returns current version without bump",
			initial:         map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			events:          nil,
			expectedVersion: "0",
			checkSnapshot:   func(t *testing.T, s *Snapshot) {},
		},
		{
			name:    "ENDPOINT_ADDED event inserts endpoint",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			events: []*registrarv1.WatchEndpointsResponse{
				{
					Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
					ServiceName: "svc-a",
					Protocol:    registryv1.Service_PROTOCOL_HTTP,
					Endpoint:    ep1,
				},
			},
			// No prior Replace; version starts at 0, Apply bumps to 1.
			expectedVersion: "1",
			checkSnapshot: func(t *testing.T, s *Snapshot) {
				got := s.GetAll(registryv1.Service_PROTOCOL_HTTP)
				require.Contains(t, got, "svc-a")
				assert.ElementsMatch(t, []*registryv1.ServiceEndpoint{ep1}, got["svc-a"])
			},
		},
		{
			name: "ENDPOINT_REMOVED event deletes endpoint",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			events: []*registrarv1.WatchEndpointsResponse{
				{
					Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
					ServiceName: "svc-a",
					Protocol:    registryv1.Service_PROTOCOL_HTTP,
					Endpoint:    ep1,
				},
			},
			// Replace bumps to 1, Apply bumps to 2.
			expectedVersion: "2",
			checkSnapshot: func(t *testing.T, s *Snapshot) {
				got := s.GetAll(registryv1.Service_PROTOCOL_HTTP)
				assert.Empty(t, got)
			},
		},
		{
			name: "ENDPOINT_UPDATED event replaces endpoint in place",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			events: []*registrarv1.WatchEndpointsResponse{
				{
					Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED,
					ServiceName: "svc-a",
					Protocol:    registryv1.Service_PROTOCOL_HTTP,
					Endpoint:    ep1Updated,
				},
			},
			expectedVersion: "2",
			checkSnapshot: func(t *testing.T, s *Snapshot) {
				got := s.GetAll(registryv1.Service_PROTOCOL_HTTP)
				require.Contains(t, got, "svc-a")
				assert.ElementsMatch(t, []*registryv1.ServiceEndpoint{ep1Updated}, got["svc-a"])
			},
		},
		{
			name:    "multiple events processed in order",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			events: []*registrarv1.WatchEndpointsResponse{
				{
					Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
					ServiceName: "svc-a",
					Protocol:    registryv1.Service_PROTOCOL_HTTP,
					Endpoint:    ep1,
				},
				{
					Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
					ServiceName: "svc-a",
					Protocol:    registryv1.Service_PROTOCOL_HTTP,
					Endpoint:    ep2,
				},
			},
			// No prior Replace; Apply bumps from 0 to 1.
			expectedVersion: "1",
			checkSnapshot: func(t *testing.T, s *Snapshot) {
				got := s.GetAll(registryv1.Service_PROTOCOL_HTTP)
				require.Contains(t, got, "svc-a")
				assert.ElementsMatch(t, []*registryv1.ServiceEndpoint{ep1, ep2}, got["svc-a"])
			},
		},
		{
			name: "ENDPOINT_REMOVED for nonexistent key is a no-op",
			initial: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			events: []*registrarv1.WatchEndpointsResponse{
				{
					Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
					ServiceName: "svc-a",
					Protocol:    registryv1.Service_PROTOCOL_HTTP,
					Endpoint:    ep2, // ep2 was never added
				},
			},
			expectedVersion: "2",
			checkSnapshot: func(t *testing.T, s *Snapshot) {
				got := s.GetAll(registryv1.Service_PROTOCOL_HTTP)
				require.Contains(t, got, "svc-a")
				assert.ElementsMatch(t, []*registryv1.ServiceEndpoint{ep1}, got["svc-a"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			if len(tt.initial) > 0 {
				s.Replace(tt.initial)
			}

			gotVersion := s.Apply(tt.events)

			assert.Equal(t, tt.expectedVersion, gotVersion)
			assert.Equal(t, tt.expectedVersion, s.Version())
			tt.checkSnapshot(t, s)
		})
	}
}

func TestFullSnapshotEvents(t *testing.T) {
	ep1 := makeEndpoint("10.0.0.1")
	ep2 := makeEndpoint("10.0.0.2")

	tests := []struct {
		name          string
		seed          map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		wantCount     int
		wantVersion   string
		wantEventType registrarv1.WatchEndpointsResponse_EventType
	}{
		{
			name:          "empty snapshot returns empty slice",
			seed:          map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{},
			wantCount:     0,
			wantVersion:   "1",
			wantEventType: registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
		},
		{
			name: "single endpoint produces one FULL_SNAPSHOT event",
			seed: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1},
				},
			},
			wantCount:     1,
			wantVersion:   "1",
			wantEventType: registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
		},
		{
			name: "multiple endpoints each produce a FULL_SNAPSHOT event",
			seed: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				"svc-a": {
					registryv1.Service_PROTOCOL_HTTP: {ep1, ep2},
				},
			},
			wantCount:     2,
			wantVersion:   "1",
			wantEventType: registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSnapshot()
			s.Replace(tt.seed)

			events, version := s.FullSnapshotEvents()

			assert.Equal(t, tt.wantVersion, version)
			assert.Len(t, events, tt.wantCount)

			for _, ev := range events {
				assert.Equal(t, tt.wantEventType, ev.GetType())
				assert.Equal(t, tt.wantVersion, ev.GetVersion())
				assert.NotEmpty(t, ev.GetServiceName())
				assert.NotNil(t, ev.GetEndpoint())
			}
		})
	}
}

func TestSnapshotThreadSafety(t *testing.T) {
	s := NewSnapshot()
	ep := makeEndpoint("10.0.0.1")

	seed := map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		"svc-a": {
			registryv1.Service_PROTOCOL_HTTP: {ep},
		},
	}

	addEvent := []*registrarv1.WatchEndpointsResponse{
		{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: "svc-b",
			Protocol:    registryv1.Service_PROTOCOL_HTTP,
			Endpoint:    makeEndpoint("10.0.0.2"),
		},
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			switch i % 5 {
			case 0:
				s.Replace(seed)
			case 1:
				s.Apply(addEvent)
			case 2:
				s.GetAll(registryv1.Service_PROTOCOL_HTTP)
			case 3:
				s.GetAllWithVersion(registryv1.Service_PROTOCOL_HTTP)
			case 4:
				s.FullSnapshotEvents()
			}
		}(i)
	}

	wg.Wait()
	// If we reach here without a panic or race condition, the test passes.
}
