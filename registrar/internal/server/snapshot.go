// Package server implements the Registrar gRPC service, including endpoint
// snapshot management, change broadcasting, and external registry synchronization.
package server

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"google.golang.org/protobuf/proto"
)

// serviceKey uniquely identifies an endpoint within the snapshot.
type serviceKey struct {
	ServiceName string
	Protocol    registryv1.Service_Protocol
	IP          string
}

// snapshotEntry stores an endpoint along with its service metadata.
type snapshotEntry struct {
	ServiceName string
	Protocol    registryv1.Service_Protocol
	Endpoint    *registryv1.ServiceEndpoint
}

// Snapshot is a thread-safe, versioned in-memory store of all service endpoints.
// It supports computing diffs between states and applying incremental changes.
type Snapshot struct {
	mu      sync.RWMutex
	entries map[serviceKey]*snapshotEntry
	version atomic.Uint64
}

// NewSnapshot creates an empty Snapshot starting at version 0.
func NewSnapshot() *Snapshot {
	return &Snapshot{
		entries: make(map[serviceKey]*snapshotEntry),
	}
}

// serviceCountLocked returns the number of endpoints stored for a service
// across protocols. Caller must hold mu (read or write).
func (s *Snapshot) serviceCountLocked(serviceName string) int {
	n := 0
	for key := range s.entries {
		if key.ServiceName == serviceName {
			n++
		}
	}
	return n
}

// serviceTransition builds a catalog event (SERVICE_ADDED/SERVICE_REMOVED).
func serviceTransition(t registrarv1.WatchEndpointsResponse_EventType, serviceName string) *registrarv1.WatchEndpointsResponse {
	return &registrarv1.WatchEndpointsResponse{Type: t, ServiceName: serviceName}
}

// ServiceNames returns the sorted names of all services currently holding at
// least one endpoint — the service catalog replayed to every new watcher.
func (s *Snapshot) ServiceNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	set := make(map[string]struct{})
	for key := range s.entries {
		set[key.ServiceName] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Version returns the current snapshot version as a string.
func (s *Snapshot) Version() string {
	return fmt.Sprintf("%d", s.version.Load())
}

// nextVersion increments the version counter and returns the new version string.
func (s *Snapshot) nextVersion() string {
	return fmt.Sprintf("%d", s.version.Add(1))
}

// GetAll returns all endpoints organized by service name. The caller receives
// a copy that is safe to mutate.
func (s *Snapshot) GetAll(protocol registryv1.Service_Protocol) map[string][]*registryv1.ServiceEndpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string][]*registryv1.ServiceEndpoint)
	for _, entry := range s.entries {
		if entry.Protocol != protocol {
			continue
		}
		result[entry.ServiceName] = append(result[entry.ServiceName], entry.Endpoint)
	}
	return result
}

// GetAllWithVersion returns all endpoints and the current version.
func (s *Snapshot) GetAllWithVersion(protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string][]*registryv1.ServiceEndpoint)
	for _, entry := range s.entries {
		if entry.Protocol != protocol {
			continue
		}
		result[entry.ServiceName] = append(result[entry.ServiceName], entry.Endpoint)
	}
	return result, s.Version()
}

// Diff compares a new set of endpoints against the current snapshot and returns
// the events needed to transition from the current state to the new state.
// It does not modify the snapshot.
func (s *Snapshot) Diff(newEndpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) []*registrarv1.WatchEndpointsResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var events []*registrarv1.WatchEndpointsResponse

	// Build a set of new keys for efficient lookup.
	newKeys := make(map[serviceKey]*snapshotEntry)
	for svcName, protocols := range newEndpoints {
		for protocol, endpoints := range protocols {
			for _, ep := range endpoints {
				key := serviceKey{ServiceName: svcName, Protocol: protocol, IP: ep.GetIp()}
				newKeys[key] = &snapshotEntry{
					ServiceName: svcName,
					Protocol:    protocol,
					Endpoint:    ep,
				}
			}
		}
	}

	// Detect removed and updated endpoints.
	for key, oldEntry := range s.entries {
		newEntry, exists := newKeys[key]
		if !exists {
			events = append(events, &registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
				ServiceName: oldEntry.ServiceName,
				Protocol:    oldEntry.Protocol,
				Endpoint:    oldEntry.Endpoint,
			})
		} else if !proto.Equal(oldEntry.Endpoint, newEntry.Endpoint) {
			events = append(events, &registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED,
				ServiceName: newEntry.ServiceName,
				Protocol:    newEntry.Protocol,
				Endpoint:    newEntry.Endpoint,
			})
		}
	}

	// Detect added endpoints.
	for key, newEntry := range newKeys {
		if _, exists := s.entries[key]; !exists {
			events = append(events, &registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
				ServiceName: newEntry.ServiceName,
				Protocol:    newEntry.Protocol,
				Endpoint:    newEntry.Endpoint,
			})
		}
	}

	return events
}

// Replace atomically replaces the entire snapshot contents with the provided
// endpoints and bumps the version. It returns the new version string plus
// the service-catalog transitions (see Apply) between the old and new
// contents; the caller stamps and broadcasts them.
func (s *Snapshot) Replace(endpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) (string, []*registrarv1.WatchEndpointsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldServices := make(map[string]struct{})
	for key := range s.entries {
		oldServices[key.ServiceName] = struct{}{}
	}

	s.entries = make(map[serviceKey]*snapshotEntry)
	for svcName, protocols := range endpoints {
		for protocol, eps := range protocols {
			for _, ep := range eps {
				key := serviceKey{ServiceName: svcName, Protocol: protocol, IP: ep.GetIp()}
				s.entries[key] = &snapshotEntry{
					ServiceName: svcName,
					Protocol:    protocol,
					Endpoint:    ep,
				}
			}
		}
	}

	newServices := make(map[string]struct{})
	for key := range s.entries {
		newServices[key.ServiceName] = struct{}{}
	}
	var transitions []*registrarv1.WatchEndpointsResponse
	for name := range newServices {
		if _, ok := oldServices[name]; !ok {
			transitions = append(transitions, serviceTransition(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED, name))
		}
	}
	for name := range oldServices {
		if _, ok := newServices[name]; !ok {
			transitions = append(transitions, serviceTransition(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED, name))
		}
	}
	// Deterministic broadcast order (map iteration above is random).
	sort.Slice(transitions, func(i, j int) bool {
		if transitions[i].GetType() != transitions[j].GetType() {
			return transitions[i].GetType() < transitions[j].GetType()
		}
		return transitions[i].GetServiceName() < transitions[j].GetServiceName()
	})

	return s.nextVersion(), transitions
}

// Apply applies a set of events to the snapshot, updating it in place.
// It returns the new version string plus the service-catalog transitions the
// events caused (a service's endpoint count crossing 0<->1 emits
// SERVICE_ADDED/SERVICE_REMOVED): deriving transitions inside Apply makes
// the catalog impossible to desync from the endpoint data it summarizes.
// Transitions are unversioned; the caller stamps and broadcasts them with
// the batch.
func (s *Snapshot) Apply(events []*registrarv1.WatchEndpointsResponse) (string, []*registrarv1.WatchEndpointsResponse) {
	if len(events) == 0 {
		return s.Version(), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var transitions []*registrarv1.WatchEndpointsResponse
	for _, event := range events {
		key := serviceKey{
			ServiceName: event.GetServiceName(),
			Protocol:    event.GetProtocol(),
			IP:          event.GetEndpoint().GetIp(),
		}

		switch event.GetType() {
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
			before := s.serviceCountLocked(key.ServiceName)
			s.entries[key] = &snapshotEntry{
				ServiceName: event.GetServiceName(),
				Protocol:    event.GetProtocol(),
				Endpoint:    event.GetEndpoint(),
			}
			if before == 0 {
				transitions = append(transitions, serviceTransition(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED, key.ServiceName))
			}
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
			if _, existed := s.entries[key]; existed {
				delete(s.entries, key)
				if s.serviceCountLocked(key.ServiceName) == 0 {
					transitions = append(transitions, serviceTransition(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED, key.ServiceName))
				}
			}
		}
	}

	return s.nextVersion(), transitions
}

// FullSnapshotEvents returns the current contents of the snapshot as a slice of
// FULL_SNAPSHOT events. This is used to send the initial state to a new watcher.
func (s *Snapshot) FullSnapshotEvents() ([]*registrarv1.WatchEndpointsResponse, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]*registrarv1.WatchEndpointsResponse, 0, len(s.entries))
	version := s.Version()

	for _, entry := range s.entries {
		events = append(events, &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
			ServiceName: entry.ServiceName,
			Protocol:    entry.Protocol,
			Endpoint:    entry.Endpoint,
			Version:     version,
		})
	}

	return events, version
}
