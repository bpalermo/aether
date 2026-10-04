// Package server implements the Registrar gRPC service, including endpoint
// snapshot management, change broadcasting, and external registry synchronization.
package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
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
	// digest is the sha256 of the endpoint's deterministic proto encoding,
	// computed once when the entry is stored and folded into the content hash.
	digest [sha256.Size]byte
}

func newEntry(serviceName string, protocol registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) *snapshotEntry {
	return &snapshotEntry{ServiceName: serviceName, Protocol: protocol, Endpoint: ep, digest: endpointDigest(ep)}
}

// deterministic is the proto encoding the content hash is computed over: map
// fields in key order, so equal endpoints always hash equal.
var deterministic = proto.MarshalOptions{Deterministic: true}

func endpointDigest(ep *registryv1.ServiceEndpoint) [sha256.Size]byte {
	b, err := deterministic.Marshal(ep)
	if err != nil {
		// Unreachable for a well-formed message; hash the error text so the
		// digest is still a function of the input rather than a constant.
		b = []byte("marshal-error:" + err.Error())
	}
	return sha256.Sum256(b)
}

// Version format (issues #1193, #1203). The version is the resume token an
// agent presents on reconnect; the server skips the snapshot when the token
// names the contents it holds now. It is therefore a NAME OF THE CONTENTS, not
// a count of writes, and it is comparable across replicas:
//
//   - "<rev>": the contents are exactly the external registry's listing at
//     store revision rev (a backend implementing registry.RevisionedLister:
//     etcd). Two replicas that listed the same revision hold the same contents.
//   - "<rev>+<hash>": the contents started from the listing at rev but deviate
//     from it -- a write-behind intent was overlaid, or an agent RPC was
//     applied since. The suffix is the content hash, so it can never equal
//     another replica's clean "<rev>".
//   - "hash:<hash>": the backend has no store revision (kubernetes, where a
//     listing is not a function of the list's resourceVersion: health depends
//     on the clock and locality on a separate node list). The version is
//     content-addressed. The prefix keeps it from ever parsing as a revision.
//
// <hash> is contentHashLen hex digits of a sha256 over the canonical contents
// (sorted keys + deterministic proto encoding of each endpoint).
const (
	versionHashPrefix = "hash:"
	versionDirtySep   = "+"
	contentHashLen    = 16
)

// formatVersion renders the version for the given state; see the format above.
func formatVersion(revision int64, dirty bool, contentHash string) string {
	if revision <= 0 {
		return versionHashPrefix + contentHash
	}
	rev := strconv.FormatInt(revision, 10)
	if dirty {
		return rev + versionDirtySep + contentHash
	}
	return rev
}

// versionContentHash returns the content hash a version embeds, if it embeds
// one ("hash:<h>" or "<rev>+<h>"). A clean "<rev>" (and any pre-#1193 counter
// version) embeds none.
func versionContentHash(version string) (string, bool) {
	if h, ok := strings.CutPrefix(version, versionHashPrefix); ok {
		return h, h != ""
	}
	if i := strings.LastIndex(version, versionDirtySep); i >= 0 {
		h := version[i+len(versionDirtySep):]
		return h, h != ""
	}
	return "", false
}

// Origin describes where a replacement state came from.
type Origin struct {
	// Revision is the store revision the listing was taken at (etcd header
	// revision); 0 when the backend has none, which makes the version
	// content-addressed.
	Revision int64
	// Overlaid reports that the listing was patched before installation (the
	// write-behind overlay shielded at least one pending intent), so the
	// contents are not exactly the store's at Revision.
	Overlaid bool
}

// Snapshot is a thread-safe, versioned in-memory store of all service endpoints.
// It supports computing diffs between states and applying incremental changes.
type Snapshot struct {
	mu      sync.RWMutex
	entries map[serviceKey]*snapshotEntry
	// serviceCounts is the per-service endpoint count derived from entries,
	// maintained incrementally so the 0<->1 catalog transitions cost a map
	// lookup instead of a full scan per event. INVARIANT: a service is present
	// here iff it holds at least one entry (a count never rests at 0), so the
	// key set is exactly the service catalog. Guarded by mu.
	serviceCounts map[string]int

	// revision is the store revision of the listing the contents were last
	// replaced from (0 = the backend has none). Guarded by mu.
	revision int64
	// dirty: the contents deviate from the listing at revision (an overlaid
	// write-behind intent, or an RPC Apply since the last replace). Guarded by mu.
	dirty bool
	// contentHash is the hash over the current contents, recomputed in the
	// critical section of every mutation. Guarded by mu.
	contentHash string
	// generation counts content changes: it moves only when contentHash does.
	// Guarded by mu.
	generation uint64
	// version is the cached formatVersion of the fields above. Guarded by mu.
	version string
	// cleanHashes remembers the content hash of the last cleanHistory clean
	// revisions this replica installed, oldest first in cleanOrder, so a client
	// holding a clean "<rev>" token is recognized as current after the revision
	// moved without a content change (an agent's re-assert re-Puts what etcd
	// already holds). Guarded by mu.
	cleanHashes map[int64]string
	cleanOrder  []int64
}

// cleanHistory bounds Snapshot.cleanHashes.
const cleanHistory = 64

// NewSnapshot creates an empty Snapshot at generation 0.
func NewSnapshot() *Snapshot {
	s := &Snapshot{
		entries:       make(map[serviceKey]*snapshotEntry),
		serviceCounts: make(map[string]int),
		cleanHashes:   make(map[int64]string),
	}
	s.contentHash = s.computeContentHashLocked()
	s.version = formatVersion(0, false, s.contentHash)
	return s
}

// computeContentHashLocked hashes the canonical contents: every entry's key in
// sorted order followed by its endpoint digest. It depends on the entries only,
// never on the revision. Caller must hold mu.
func (s *Snapshot) computeContentHashLocked() string {
	keys := make([]serviceKey, 0, len(s.entries))
	for k := range s.entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.ServiceName != b.ServiceName {
			return a.ServiceName < b.ServiceName
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.IP < b.IP
	})
	h := sha256.New()
	var protoBuf [4]byte
	for _, k := range keys {
		e := s.entries[k]
		h.Write([]byte(k.ServiceName))
		h.Write([]byte{0})
		binary.BigEndian.PutUint32(protoBuf[:], uint32(k.Protocol))
		h.Write(protoBuf[:])
		h.Write([]byte(k.IP))
		h.Write([]byte{0})
		h.Write(e.digest[:])
	}
	return hex.EncodeToString(h.Sum(nil))[:contentHashLen]
}

// refreshLocked recomputes the content hash after a mutation, advances the
// generation iff the contents changed, and re-renders the version. A mutation
// that changes the contents marks a revisioned snapshot dirty when markDirty is
// set (Apply); Replace sets dirty itself. Caller must hold mu for writing.
func (s *Snapshot) refreshLocked(markDirty bool) {
	h := s.computeContentHashLocked()
	if h != s.contentHash {
		s.contentHash = h
		s.generation++
		if markDirty {
			s.dirty = true
		}
	}
	s.version = formatVersion(s.revision, s.dirty, s.contentHash)
}

// serviceCountLocked returns the number of endpoints stored for a service
// across protocols. Caller must hold mu (read or write).
func (s *Snapshot) serviceCountLocked(serviceName string) int {
	return s.serviceCounts[serviceName]
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

	names := make([]string, 0, len(s.serviceCounts))
	for name := range s.serviceCounts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Version returns the current snapshot version (see the format above).
func (s *Snapshot) Version() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// State is a consistent read of the snapshot's identity.
type State struct {
	// Version is the resume token (see the format above).
	Version string
	// Revision is the store revision of the last installed listing; 0 = none.
	Revision int64
	// Dirty reports that the contents deviate from the listing at Revision.
	Dirty bool
	// ContentHash names the contents; equal on two replicas iff they serve the
	// same endpoints.
	ContentHash string
	// Generation counts content changes in this process.
	Generation uint64
}

// State returns the snapshot's version, revision, content hash and generation,
// read in one critical section.
func (s *Snapshot) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stateLocked()
}

func (s *Snapshot) stateLocked() State {
	return State{
		Version:     s.version,
		Revision:    s.revision,
		Dirty:       s.dirty,
		ContentHash: s.contentHash,
		Generation:  s.generation,
	}
}

// currentLocked reports whether a client presenting token holds the current
// contents: the token is the current version; or it embeds the current content
// hash (a "<rev>+<h>" or "hash:<h>" token taken before a sync re-derived the
// same contents at a newer revision); or it is a clean "<rev>" this replica
// installed with the current content hash. A clean revision names one listing
// on every replica, so a token issued by another replica at that revision is
// recognized too, provided this one installed it. Caller must hold mu.
func (s *Snapshot) currentLocked(token string) bool {
	if token == "" {
		return false
	}
	if token == s.version {
		return true
	}
	if h, ok := versionContentHash(token); ok {
		return h == s.contentHash
	}
	if rev, err := strconv.ParseInt(token, 10, 64); err == nil {
		h, ok := s.cleanHashes[rev]
		return ok && h == s.contentHash
	}
	return false
}

// rememberCleanLocked records that the listing at rev has hash h. Caller must
// hold mu for writing.
func (s *Snapshot) rememberCleanLocked(rev int64, h string) {
	if _, seen := s.cleanHashes[rev]; seen {
		return
	}
	s.cleanHashes[rev] = h
	s.cleanOrder = append(s.cleanOrder, rev)
	if len(s.cleanOrder) > cleanHistory {
		delete(s.cleanHashes, s.cleanOrder[0])
		s.cleanOrder = s.cleanOrder[1:]
	}
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
	return result, s.version
}

// Diff compares a new set of endpoints against the current snapshot and returns
// the events needed to transition from the current state to the new state.
// It does not modify the snapshot.
//
// A caller that goes on to Replace with the same state must use DiffAndReplace
// instead — see the TOCTOU note there.
func (s *Snapshot) Diff(newEndpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) []*registrarv1.WatchEndpointsResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diffLocked(newEndpoints)
}

// diffLocked is Diff's body. Caller must hold mu (read or write).
func (s *Snapshot) diffLocked(newEndpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) []*registrarv1.WatchEndpointsResponse {
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
// endpoints, from a listing without a store revision. It returns the new
// version string plus the service-catalog transitions (see Apply) between the
// old and new contents; the caller stamps and broadcasts them. The version
// moves only when the contents do.
func (s *Snapshot) Replace(endpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) (string, []*registrarv1.WatchEndpointsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replaceLocked(endpoints, Origin{})
}

// DiffAndReplace computes the diff against the current contents and installs
// the new contents in ONE critical section, returning the diff events, the new
// version, and the catalog transitions.
//
// The two halves must not be separate lock acquisitions (#772, S13). The sync
// loop is not the only writer: an agent's RegisterEndpoint lands via Apply on a
// gRPC handler goroutine. One arriving between a separate Diff and Replace is
// broadcast as ENDPOINT_ADDED by Apply and then silently erased by Replace,
// which installs a state computed before that endpoint existed — with no
// compensating REMOVED event, so every watcher keeps an endpoint the snapshot
// no longer has until a later sync cycle happens to re-derive it.
//
// Today that hole is masked by a different component: Syncer.writeBehind
// overlays the pending intents onto the new state before this call. But then
// the invariant is held by the write-behind queue rather than by the snapshot,
// and the legacy constructor path leaves writeBehind nil (NewRegistrarServer
// without a queue), where nothing masks it. Holding the lock across both halves
// makes the snapshot self-consistent on its own.
func (s *Snapshot) DiffAndReplace(endpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) ([]*registrarv1.WatchEndpointsResponse, string, []*registrarv1.WatchEndpointsResponse) {
	return s.DiffAndReplaceAt(endpoints, Origin{})
}

// DiffAndReplaceAt is DiffAndReplace for a listing with a known origin: the
// store revision it was taken at and whether it was overlaid. The resulting
// version is "<rev>" for a clean revisioned listing, "<rev>+<hash>" for an
// overlaid one, and "hash:<hash>" without a revision.
func (s *Snapshot) DiffAndReplaceAt(endpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, origin Origin) ([]*registrarv1.WatchEndpointsResponse, string, []*registrarv1.WatchEndpointsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.diffLocked(endpoints)
	version, transitions := s.replaceLocked(endpoints, origin)
	return events, version, transitions
}

// replaceLocked is Replace's body. Caller must hold mu for writing.
func (s *Snapshot) replaceLocked(endpoints map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, origin Origin) (string, []*registrarv1.WatchEndpointsResponse) {
	// The count map's key set is the service catalog (see the invariant on
	// serviceCounts), so the old catalog is read straight off it.
	oldServices := make(map[string]struct{}, len(s.serviceCounts))
	for name := range s.serviceCounts {
		oldServices[name] = struct{}{}
	}

	s.entries = make(map[serviceKey]*snapshotEntry)
	s.serviceCounts = make(map[string]int)
	for svcName, protocols := range endpoints {
		for protocol, eps := range protocols {
			for _, ep := range eps {
				key := serviceKey{ServiceName: svcName, Protocol: protocol, IP: ep.GetIp()}
				if _, dup := s.entries[key]; !dup {
					s.serviceCounts[svcName]++
				}
				s.entries[key] = newEntry(svcName, protocol, ep)
			}
		}
	}

	newServices := make(map[string]struct{}, len(s.serviceCounts))
	for name := range s.serviceCounts {
		newServices[name] = struct{}{}
	}

	s.revision = origin.Revision
	s.dirty = origin.Revision > 0 && origin.Overlaid
	s.refreshLocked(false)
	if s.revision > 0 && !s.dirty {
		s.rememberCleanLocked(s.revision, s.contentHash)
	}
	return s.version, computeTransitions(oldServices, newServices)
}

// computeTransitions builds the sorted list of SERVICE_ADDED/SERVICE_REMOVED
// catalog events that describe the diff between oldServices and newServices.
func computeTransitions(oldServices, newServices map[string]struct{}) []*registrarv1.WatchEndpointsResponse {
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
	return transitions
}

// Apply applies a set of events to the snapshot, updating it in place.
// The version moves only when the contents do; on a revisioned snapshot it
// becomes "<rev>+<hash>" until the next sync re-derives the contents from the
// store. It returns the new version string plus the service-catalog transitions the
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

		var tr *registrarv1.WatchEndpointsResponse
		switch event.GetType() {
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
			tr = s.applyUpsertLocked(key, event)
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
			tr = s.applyRemoveLocked(key)
		}
		if tr != nil {
			transitions = append(transitions, tr)
		}
	}

	s.refreshLocked(true)
	return s.version, transitions
}

// applyUpsertLocked stores an added/updated endpoint, maintaining serviceCounts,
// and returns the SERVICE_ADDED transition when this is the service's first
// endpoint (nil otherwise). Caller must hold mu for writing.
func (s *Snapshot) applyUpsertLocked(key serviceKey, event *registrarv1.WatchEndpointsResponse) *registrarv1.WatchEndpointsResponse {
	before := s.serviceCountLocked(key.ServiceName)
	// An UPDATED event replacing an existing entry must not raise the count:
	// only a key that was absent adds an endpoint.
	_, existed := s.entries[key]
	s.entries[key] = newEntry(event.GetServiceName(), event.GetProtocol(), event.GetEndpoint())
	if !existed {
		s.serviceCounts[key.ServiceName]++
	}
	if before == 0 {
		return serviceTransition(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED, key.ServiceName)
	}
	return nil
}

// applyRemoveLocked deletes an endpoint if present, maintaining serviceCounts,
// and returns the SERVICE_REMOVED transition when the service's last endpoint
// went away (nil otherwise). Caller must hold mu for writing.
func (s *Snapshot) applyRemoveLocked(key serviceKey) *registrarv1.WatchEndpointsResponse {
	if _, existed := s.entries[key]; !existed {
		return nil
	}
	delete(s.entries, key)
	if n := s.serviceCounts[key.ServiceName] - 1; n > 0 {
		s.serviceCounts[key.ServiceName] = n
		return nil
	}
	// Never leave a zero resting in the map: its key set is the service catalog.
	delete(s.serviceCounts, key.ServiceName)
	return serviceTransition(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED, key.ServiceName)
}

// FullSnapshotEvents returns the current contents of the snapshot as a slice of
// FULL_SNAPSHOT events, plus the version they amount to. This is used to send
// the initial state to a new watcher.
//
// The events carry NO version (#1203): the agent adopts every non-empty
// version it receives as its resume token, and a stream cut after the first of
// them would leave it presenting the full snapshot's version while holding a
// fraction of it. The version rides only on SNAPSHOT_COMPLETE, the one point at
// which the receiver holds everything it names.
//
// A non-nil filter scopes the result to those service names (a nil filter is
// the unfiltered, cluster-wide snapshot): a demand-scoped watcher discards
// everything outside its filter anyway, so the out-of-scope events are skipped
// before their protos are ever built.
func (s *Snapshot) FullSnapshotEvents(filter map[string]struct{}) ([]*registrarv1.WatchEndpointsResponse, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fullSnapshotEventsLocked(filter), s.version
}

// WatchStart is the snapshot half of a new watch, read in one critical
// section: given the client's resume token, it reports whether the client is
// current (then events and services are nil) and otherwise returns the
// unversioned FULL_SNAPSHOT events and the service catalog to replay. version
// is what the SNAPSHOT_COMPLETE marker must carry: the client's own token when
// it is current under another name (its embedded content hash matches), so a
// client never sees a marker version differ from its token without a resend.
func (s *Snapshot) WatchStart(token string, filter map[string]struct{}) (events []*registrarv1.WatchEndpointsResponse, services []string, version string, current bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.currentLocked(token) {
		return nil, nil, token, true
	}
	services = make([]string, 0, len(s.serviceCounts))
	for name := range s.serviceCounts {
		services = append(services, name)
	}
	sort.Strings(services)
	return s.fullSnapshotEventsLocked(filter), services, s.version, false
}

func (s *Snapshot) fullSnapshotEventsLocked(filter map[string]struct{}) []*registrarv1.WatchEndpointsResponse {
	// A filtered watcher keeps at most its filter's services; sizing on the
	// whole snapshot would reserve the very memory the filter exists to avoid.
	capacity := len(s.entries)
	if filter != nil {
		capacity = min(capacity, len(filter))
	}
	events := make([]*registrarv1.WatchEndpointsResponse, 0, capacity)

	for _, entry := range s.entries {
		if filter != nil {
			if _, inScope := filter[entry.ServiceName]; !inScope {
				continue
			}
		}
		events = append(events, &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
			ServiceName: entry.ServiceName,
			Protocol:    entry.Protocol,
			Endpoint:    entry.Endpoint,
		})
	}
	return events
}
