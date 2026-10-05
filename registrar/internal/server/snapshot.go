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
//   - "<rev>.<hash>": the contents are exactly the external registry's
//     listing at store revision rev (a backend implementing
//     registry.RevisionedLister: etcd).
//   - "<rev>+<hash>": the contents started from the listing at rev but deviate
//     from it -- a write-behind intent was overlaid, or an agent RPC was
//     applied since.
//   - "hash:<hash>": the backend has no store revision (kubernetes, where a
//     listing is not a function of the list's resourceVersion: health depends
//     on the clock and locality on a separate node list).
//
// <hash> is contentHashLen hex digits of a sha256 over the canonical contents
// (sorted keys + deterministic proto encoding of each endpoint), and it ALONE
// decides whether a client is current: a token is current iff its hash part
// equals the current content hash. The revision is carried for the lag metrics
// and for operators, never trusted for equality -- a rebuilt etcd restarts its
// revisions at 1, and a binary that decodes a stored value differently derives
// different contents at the same revision. No form parses as a bare integer, so
// a pre-#1193 counter token never matches.
const (
	versionHashPrefix = "hash:"
	versionCleanSep   = "."
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
	return rev + versionCleanSep + contentHash
}

// versionContentHash returns the content hash a version embeds: the part after
// "hash:", or after the "." / "+" that follows the revision. A pre-#1193
// counter version embeds none.
func versionContentHash(version string) (string, bool) {
	if h, ok := strings.CutPrefix(version, versionHashPrefix); ok {
		return h, h != ""
	}
	if i := strings.IndexAny(version, versionCleanSep+versionDirtySep); i >= 0 {
		h := version[i+1:]
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
}

// NewSnapshot creates an empty Snapshot at generation 0.
func NewSnapshot() *Snapshot {
	s := &Snapshot{
		entries:       make(map[serviceKey]*snapshotEntry),
		serviceCounts: make(map[string]int),
	}
	s.contentHash = s.computeContentHashLocked()
	s.version = formatVersion(0, false, s.contentHash)
	return s
}

// computeContentHashLocked hashes the canonical contents: every entry's key in
// sorted order followed by its endpoint digest. It depends on the entries only,
// never on the revision. Caller must hold mu.
//
// Cost, all under the write lock: a replace re-marshals and re-digests EVERY
// endpoint (newEntry), O(N) per sync; every mutation, Apply included, re-sorts
// all N keys, O(N log N). Negligible at talos scale (hundreds of endpoints);
// revisit -- an incremental (e.g. additive multiset) hash -- before ~10k.
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

// Resume is the outcome of a watch start (Snapshot.WatchStart).
type Resume int

const (
	// ResumeResend: the client's token does not name the current contents; it
	// gets the full snapshot and the catalog.
	ResumeResend Resume = iota
	// ResumeCurrent: the token is the current version; the marker alone.
	ResumeCurrent
	// ResumeRenamed: the token names the current contents under an older name
	// (same content hash, another revision or another form). No endpoint
	// events; the catalog and a marker carrying the CURRENT version, so the
	// client swaps in an identical catalog and adopts the fresh token.
	ResumeRenamed
)

// resumeLocked classifies a client's token against the current contents. Only
// the embedded content hash decides (see the version format). Caller must hold
// mu.
func (s *Snapshot) resumeLocked(token string) Resume {
	if token == "" {
		return ResumeResend
	}
	if token == s.version {
		return ResumeCurrent
	}
	if h, ok := versionContentHash(token); ok && h == s.contentHash {
		return ResumeRenamed
	}
	return ResumeResend
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
// version is "<rev>.<hash>" for a clean revisioned listing, "<rev>+<hash>" for an
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

// RemoveIPs removes the given IPs of a service under every protocol the
// snapshot holds them, returning one ENDPOINT_REMOVED event per removed entry
// (carrying its protocol and stored endpoint), the new version, and the catalog
// transitions. An IP the snapshot does not hold yields no event (#1206).
//
// UnregisterEndpointRequest names no protocol, but the snapshot and every
// agent cache key an endpoint by (service, protocol, ip): a REMOVED event
// without a protocol matches nothing on either side, so the removal must be
// resolved against what is stored, under the same lock that removes it.
func (s *Snapshot) RemoveIPs(serviceName string, ips []string) ([]*registrarv1.WatchEndpointsResponse, string, []*registrarv1.WatchEndpointsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var events, transitions []*registrarv1.WatchEndpointsResponse
	for _, ip := range ips {
		for _, protocol := range allProtocols {
			key := serviceKey{ServiceName: serviceName, Protocol: protocol, IP: ip}
			entry, ok := s.entries[key]
			if !ok {
				continue
			}
			events = append(events, &registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
				ServiceName: serviceName,
				Protocol:    protocol,
				Endpoint:    entry.Endpoint,
			})
			if tr := s.applyRemoveLocked(key); tr != nil {
				transitions = append(transitions, tr)
			}
		}
	}
	if len(events) > 0 {
		s.refreshLocked(true)
	}
	return events, s.version, transitions
}

// allProtocols is every Service_Protocol value, in enum order: the protocols an
// endpoint may be stored under (RegisterEndpoint stores whatever it is given).
var allProtocols = func() []registryv1.Service_Protocol {
	out := make([]registryv1.Service_Protocol, 0, len(registryv1.Service_Protocol_name))
	for v := range registryv1.Service_Protocol_name {
		out = append(out, registryv1.Service_Protocol(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}()

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
// section. Given the client's resume token it returns the resume outcome, the
// unversioned FULL_SNAPSHOT events (ResumeResend only), the service catalog to
// replay (all but ResumeCurrent), and the current version, which the
// SNAPSHOT_COMPLETE marker carries in every case.
//
// have is the request's partial_resume (#1239): nil for an ordinary token, else
// the services the client's cache holds at token. When the token names the
// current contents, the client is EXTENDED instead of resent: events are the
// ENDPOINT_ADDED events of filter's services outside have, and extended is
// true. A token that does not name the current contents is resent in full, as
// for an ordinary token: the client's held services may be stale too.
//
// Extending is exact only because the content hash is over the WHOLE snapshot:
// a token naming the current contents means every service the client holds is
// current, whatever its filter was when it earned the token.
func (s *Snapshot) WatchStart(token string, filter, have map[string]struct{}) (events []*registrarv1.WatchEndpointsResponse, services []string, version string, resume Resume, extended bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	resume = s.resumeLocked(token)
	extended = have != nil && resume != ResumeResend
	if extended {
		events = s.missingEventsLocked(filter, have)
	}
	if resume == ResumeCurrent {
		return events, nil, s.version, resume, extended
	}
	services = make([]string, 0, len(s.serviceCounts))
	for name := range s.serviceCounts {
		services = append(services, name)
	}
	sort.Strings(services)
	if resume == ResumeResend {
		events = s.fullSnapshotEventsLocked(filter)
	}
	return events, services, s.version, resume, extended
}

// missingEventsLocked returns, as unversioned ENDPOINT_ADDED events, the
// endpoints of the services in filter (nil = every service) that are not in
// have: what an extended client lacks. Not FULL_SNAPSHOT: a client clears its
// whole cache on the first of those. Caller must hold mu.
func (s *Snapshot) missingEventsLocked(filter, have map[string]struct{}) []*registrarv1.WatchEndpointsResponse {
	var events []*registrarv1.WatchEndpointsResponse
	for _, entry := range s.entries {
		if _, held := have[entry.ServiceName]; held {
			continue
		}
		if filter != nil {
			if _, inScope := filter[entry.ServiceName]; !inScope {
				continue
			}
		}
		events = append(events, &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: entry.ServiceName,
			Protocol:    entry.Protocol,
			Endpoint:    entry.Endpoint,
		})
	}
	return events
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
