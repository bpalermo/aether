package server

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// revisionedRegistry is a registry.RevisionedLister test double: an HTTP-only
// store whose contents and revision the test sets. ListAllEndpoints (the
// per-protocol API) is counted so a test can assert the syncer did not use it.
type revisionedRegistry struct {
	mockRegistry
	derived bool

	mu       sync.Mutex
	rev      int64
	eps      map[string][]*registryv1.ServiceEndpoint
	revLists int
}

var _ registry.RevisionedLister = (*revisionedRegistry)(nil)

func (r *revisionedRegistry) set(rev int64, eps map[string][]*registryv1.ServiceEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rev, r.eps = rev, eps
}

func (r *revisionedRegistry) ListAllEndpointsRevisioned(_ context.Context, _ []registryv1.Service_Protocol) (map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revLists++
	out := make(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, len(r.eps))
	for svc, eps := range r.eps {
		cp := make([]*registryv1.ServiceEndpoint, 0, len(eps))
		for _, ep := range eps {
			cp = append(cp, proto.CloneOf(ep))
		}
		out[svc] = map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{registryv1.Service_PROTOCOL_HTTP: cp}
	}
	return out, r.rev, nil
}

func (r *revisionedRegistry) StoreRevision() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rev
}

// derivedRevisionedRegistry adds registry.DerivedEndpoints, to drive the
// write-behind queue's #1152 release rule through a revisioned sync.
type derivedRevisionedRegistry struct{ revisionedRegistry }

func (*derivedRevisionedRegistry) DerivesEndpoints() bool { return true }

// clean renders the clean version for revision rev and state st's hash.
func clean(rev int64, st State) string {
	return strconv.FormatInt(rev, 10) + "." + st.ContentHash
}

func ep(ip string) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{Ip: ip, Port: 8080}
}

func newRevisionedSyncer(reg registry.Registry) (*Syncer, *Snapshot) {
	log := slog.New(slog.DiscardHandler)
	snap := NewSnapshot()
	return NewSyncer(reg, snap, NewBroadcaster(log, nil), time.Hour, log, nil), snap
}

// TestSync_StoreRevisionIsTheVersion (#1193): on a revisioned backend the
// version is the listing's store revision, read in ONE call; the generation
// moves only with the contents.
func TestSync_StoreRevisionIsTheVersion(t *testing.T) {
	reg := &revisionedRegistry{}
	syncer, snap := newRevisionedSyncer(reg)
	ctx := context.Background()

	reg.set(7, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}})
	syncer.sync(ctx)
	first := snap.State()
	assert.Equal(t, clean(7, first), first.Version)
	assert.Equal(t, int64(7), first.Revision)
	assert.Equal(t, 1, reg.revLists, "one read per sync")
	assert.Zero(t, reg.listCalls.Load(), "the per-protocol listing must not be used")

	syncer.sync(ctx) // no-op: same revision, same contents
	assert.Equal(t, first, snap.State(), "a no-op sync changes nothing")

	// An unrelated store write moved the revision; the contents are the same.
	reg.set(8, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}})
	syncer.sync(ctx)
	moved := snap.State()
	assert.Equal(t, clean(8, moved), moved.Version)
	assert.Equal(t, first.Generation, moved.Generation, "same contents, same generation")
	assert.Equal(t, first.ContentHash, moved.ContentHash, "the hash is computed from the entries, not the revision")

	reg.set(9, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1"), ep("10.0.0.2")}})
	syncer.sync(ctx)
	changed := snap.State()
	assert.Equal(t, clean(9, changed), changed.Version)
	assert.Equal(t, first.Generation+1, changed.Generation)
	assert.NotEqual(t, first.ContentHash, changed.ContentHash)
}

// TestSnapshot_ReplicasAtOneRevision (#1193): two replicas fed the same listing
// at the same revision report the same version and hash; a replica whose
// contents differ at that revision (the divergence the content_hash gauge
// exists to expose) reports a different version, so no client is ever told it
// is current against contents it does not hold.
func TestSnapshot_ReplicasAtOneRevision(t *testing.T) {
	state := func(ips ...string) map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint {
		return listing(map[string][]string{"ns/a": ips})
	}
	a, b, c := NewSnapshot(), NewSnapshot(), NewSnapshot()
	a.DiffAndReplaceAt(state("10.0.0.1", "10.0.0.2"), Origin{Revision: 42})
	b.DiffAndReplaceAt(state("10.0.0.2", "10.0.0.1"), Origin{Revision: 41})
	b.DiffAndReplaceAt(state("10.0.0.2", "10.0.0.1"), Origin{Revision: 42})
	c.DiffAndReplaceAt(state("10.0.0.1"), Origin{Revision: 42})

	sa, sb, sc := a.State(), b.State(), c.State()
	assert.Equal(t, clean(42, sa), sa.Version)
	assert.Equal(t, sa.Version, sb.Version)
	assert.Equal(t, sa.ContentHash, sb.ContentHash)
	assert.Equal(t, sa.Revision, sc.Revision)
	assert.NotEqual(t, sa.ContentHash, sc.ContentHash, "same revision + different content_hash = divergence")
	assert.NotEqual(t, sa.Version, sc.Version, "the revision alone never names contents")
}

// TestSnapshot_ContentHashCoversEveryField: the hash is over the endpoints'
// encoding, so a change in any field (here only health) changes it, and the
// revision does not enter it at all.
func TestSnapshot_ContentHashCoversEveryField(t *testing.T) {
	healthy := map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		"ns/a": {registryv1.Service_PROTOCOL_HTTP: {{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}}},
	}
	draining := map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		"ns/a": {registryv1.Service_PROTOCOL_HTTP: {{Ip: "10.0.0.1", Health: registryv1.ServiceEndpoint_HEALTH_DRAINING}}},
	}
	x, y, z := NewSnapshot(), NewSnapshot(), NewSnapshot()
	x.DiffAndReplaceAt(healthy, Origin{Revision: 5})
	y.DiffAndReplaceAt(healthy, Origin{Revision: 900})
	z.DiffAndReplaceAt(draining, Origin{Revision: 5})
	assert.Equal(t, x.State().ContentHash, y.State().ContentHash, "the revision is not hashed")
	assert.NotEqual(t, x.State().ContentHash, z.State().ContentHash, "every endpoint field is hashed")
}

// TestSnapshot_ApplyDirtiesARevisionedVersion: an RPC applied on top of a
// listing makes the version "<rev>+<hash>" -- never another replica's clean
// "<rev>" -- and a no-op Apply leaves it clean.
func TestSnapshot_ApplyDirtiesARevisionedVersion(t *testing.T) {
	s := NewSnapshot()
	s.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 3})
	require.Equal(t, clean(3, s.State()), s.Version())

	s.Apply([]*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
		ServiceName: "ns/a",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.9.9.9"}, // absent
	}})
	assert.Equal(t, clean(3, s.State()), s.Version(), "an Apply that changes nothing keeps the clean version")

	s.Apply([]*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "ns/a",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    ep("10.0.0.2"),
	}})
	st := s.State()
	assert.Equal(t, "3+"+st.ContentHash, st.Version)
	assert.True(t, st.Dirty)
}

// TestSync_PendingOverlayDirtiesTheVersionUntilObserved is the etcd release
// path: an agent's register is applied and queued; a sync whose listing does
// not have it yet overlays it, so the version is "<rev>+<hash>"; once the write
// landed and a listing returns it, the intent is released and the version is
// the clean revision again.
func TestSync_PendingOverlayDirtiesTheVersionUntilObserved(t *testing.T) {
	reg := &revisionedRegistry{}
	syncer, snap := newRevisionedSyncer(reg)
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	syncer.UseWriteBehind(q)
	srv := NewRegistrarServer(reg, snap, syncer.broadcaster, "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
	srv.UseWriteBehind(q)
	ctx := context.Background()

	reg.set(10, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}})
	syncer.sync(ctx)
	require.Equal(t, clean(10, snap.State()), snap.Version())

	_, err := srv.RegisterEndpoint(ctx, &registrarv1.RegisterEndpointRequest{
		ServiceName: "ns/a", Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: ep("10.0.0.2"),
	})
	require.NoError(t, err)
	applied := snap.State()
	assert.Equal(t, "10+"+applied.ContentHash, applied.Version)

	// A sync before the write landed: the listing still lacks it.
	reg.set(11, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}})
	syncer.sync(ctx)
	overlaid := snap.State()
	assert.Equal(t, "11+"+overlaid.ContentHash, overlaid.Version, "a pending intent must not let the version read clean")
	assert.NotEqual(t, clean(11, overlaid), overlaid.Version)
	assert.Equal(t, applied.ContentHash, overlaid.ContentHash, "the overlay kept the applied contents")
	assert.Equal(t, applied.Generation, overlaid.Generation)

	// The write lands and the next listing returns it: released, clean.
	q.flushDue(ctx)
	reg.set(12, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1"), ep("10.0.0.2")}})
	syncer.sync(ctx)
	require.False(t, q.Shielding("ns/a", "10.0.0.2"), "an observed intent is released")
	released := snap.State()
	assert.Equal(t, clean(12, released), released.Version)
	assert.Equal(t, applied.ContentHash, released.ContentHash)
	assert.Equal(t, applied.Generation, released.Generation, "releasing an intent the store now holds is not a content change")
}

// TestSync_DerivedReleaseReturnsToTheCleanRevision is the #1152 release path
// (registry.DerivedEndpoints): an intent received before the listing started is
// released by it and the listing is taken as is, so the version is clean; one
// overlaid onto a listing that predates it keeps the version dirty until then.
func TestSync_DerivedReleaseReturnsToTheCleanRevision(t *testing.T) {
	reg := &derivedRevisionedRegistry{}
	syncer, snap := newRevisionedSyncer(reg)
	q := NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil)
	require.True(t, q.derived)
	syncer.UseWriteBehind(q)
	ctx := context.Background()

	reg.set(20, map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}})
	syncer.sync(ctx)
	require.Equal(t, clean(20, snap.State()), snap.Version())

	// A sync already in flight when the intent arrived: overlaid, dirty.
	listedAt := time.Now()
	drain := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Port: 8080, Health: registryv1.ServiceEndpoint_HEALTH_DRAINING}
	q.EnqueueRegister("ns/a", registryv1.Service_PROTOCOL_HTTP, drain)
	st, rev, err := reg.ListAllEndpointsRevisioned(ctx, syncedProtocols)
	require.NoError(t, err)
	n := q.Overlay(st, listedAt)
	require.Equal(t, 1, n)
	snap.DiffAndReplaceAt(st, Origin{Revision: rev, Overlaid: n > 0})
	dirty := snap.State()
	assert.Equal(t, "20+"+dirty.ContentHash, dirty.Version)

	// The next sync started after the intent: released, the listing decides.
	syncer.sync(ctx)
	assert.False(t, q.Shielding("ns/a", "10.0.0.1"))
	assert.Equal(t, clean(20, snap.State()), snap.Version())
}

// TestSync_NoRevisionIsContentAddressed: a backend without revisions (the
// kubernetes one) gets "hash:<content hash>", stable across no-op syncs and
// equal across replicas holding the same contents.
func TestSync_NoRevisionIsContentAddressed(t *testing.T) {
	list := func(_ context.Context, p registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		if p != registryv1.Service_PROTOCOL_HTTP {
			return nil, nil
		}
		return map[string][]*registryv1.ServiceEndpoint{"ns/a": {ep("10.0.0.1")}}, nil
	}
	s1, snap1, _ := newTestSyncer(&mockRegistry{listAllEndpointsFunc: list}, time.Hour)
	s2, snap2, _ := newTestSyncer(&mockRegistry{listAllEndpointsFunc: list}, time.Hour)
	ctx := context.Background()
	s1.sync(ctx)
	v := snap1.Version()
	s1.sync(ctx)
	s2.sync(ctx)
	s2.sync(ctx)
	s2.sync(ctx)

	assert.Equal(t, "hash:"+snap1.State().ContentHash, v)
	assert.Equal(t, v, snap1.Version(), "stable across no-op syncs")
	assert.Equal(t, v, snap2.Version(), "equal across replicas with equal contents")
	_, err := strconv.ParseInt(v, 10, 64)
	assert.Error(t, err, "a content-addressed version must never parse as a revision")
}

// watchOnce opens a watch with token and returns what was sent up to and
// including SNAPSHOT_COMPLETE.
func watchOnce(t *testing.T, snap *Snapshot, token string) []*registrarv1.WatchEndpointsResponse {
	t.Helper()
	s := NewRegistrarServer(&flakyRegistry{}, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)
	return reconnect(t, s, token)
}

// TestWatchEndpoints_ResumeDecision (#1193): only the token's content hash
// decides. The current version gets the marker alone; the current contents
// under another name get the catalog and a marker carrying the CURRENT version
// (no endpoint events); anything else gets the snapshot and the catalog.
func TestWatchEndpoints_ResumeDecision(t *testing.T) {
	two := listing(map[string][]string{"ns/a": {"10.0.0.1", "10.0.0.2"}})
	one := listing(map[string][]string{"ns/a": {"10.0.0.1"}})
	hashOf := func(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) string {
		s := NewSnapshot()
		s.DiffAndReplace(state)
		return s.State().ContentHash
	}
	// renamed asserts the rename shape: catalog + marker(current), no endpoints.
	renamed := func(t *testing.T, sent []*registrarv1.WatchEndpointsResponse, current string) {
		t.Helper()
		assert.Zero(t, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT), "same contents: no endpoint resend")
		assert.Equal(t, 1, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED), "the identical catalog precedes the new name")
		last := sent[len(sent)-1]
		assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, last.GetType())
		assert.Equal(t, current, last.GetVersion(), "the marker hands the client the current name")
	}

	t.Run("current version: marker only", func(t *testing.T) {
		snap := NewSnapshot()
		snap.DiffAndReplaceAt(two, Origin{Revision: 30})
		sent := watchOnce(t, snap, snap.Version())
		require.Len(t, sent, 1)
		assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, sent[0].GetType())
		assert.Equal(t, snap.Version(), sent[0].GetVersion())
	})

	t.Run("other contents: full snapshot and catalog", func(t *testing.T) {
		snap := NewSnapshot()
		snap.DiffAndReplaceAt(two, Origin{Revision: 30})
		sent := watchOnce(t, snap, "29."+hashOf(one))
		assert.Equal(t, 2, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
		assert.Equal(t, 1, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED))
		last := sent[len(sent)-1]
		assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, last.GetType())
		assert.Equal(t, snap.Version(), last.GetVersion())
		for _, e := range sent[:len(sent)-1] {
			assert.Empty(t, e.GetVersion(), "only the marker carries the version (#1203)")
		}
	})

	t.Run("same revision, other contents: full snapshot", func(t *testing.T) {
		// A rebuilt etcd restarting its revisions, or a binary decoding a stored
		// value differently: the revision is never trusted for equality.
		snap := NewSnapshot()
		snap.DiffAndReplaceAt(two, Origin{Revision: 30})
		sent := watchOnce(t, snap, "30."+hashOf(one))
		assert.Equal(t, 2, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
	})

	t.Run("no revision: the content hash decides", func(t *testing.T) {
		snap := NewSnapshot()
		snap.DiffAndReplace(two)
		require.Len(t, watchOnce(t, snap, snap.Version()), 1)
		sent := watchOnce(t, snap, "hash:"+hashOf(one))
		assert.Equal(t, 2, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
	})

	t.Run("dirty token re-derived cleanly: renamed", func(t *testing.T) {
		snap := NewSnapshot()
		snap.DiffAndReplaceAt(one, Origin{Revision: 30})
		snap.Apply([]*registrarv1.WatchEndpointsResponse{{
			Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "ns/a",
			Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: ep("10.0.0.2"),
		}})
		token := snap.Version()
		require.Contains(t, token, "30+")

		snap.DiffAndReplaceAt(two, Origin{Revision: 31}) // the write landed
		require.Equal(t, clean(31, snap.State()), snap.Version())
		renamed(t, watchOnce(t, snap, token), snap.Version())
	})

	t.Run("clean token, revision moved without a content change: renamed", func(t *testing.T) {
		// An agent's re-assert re-Puts endpoints etcd already holds, or config
		// export / 006 mirroring writes elsewhere under the prefix: the revision
		// moves, the contents do not -- however far, and whichever replica
		// issued the token.
		snap := NewSnapshot()
		snap.DiffAndReplaceAt(two, Origin{Revision: 30})
		token := snap.Version()
		snap.DiffAndReplaceAt(two, Origin{Revision: 5000})
		renamed(t, watchOnce(t, snap, token), snap.Version())

		other := NewSnapshot() // a replica that never installed revision 30
		other.DiffAndReplaceAt(two, Origin{Revision: 5001})
		renamed(t, watchOnce(t, other, token), other.Version())

		// ...but not once the contents changed since.
		snap.DiffAndReplaceAt(one, Origin{Revision: 5002})
		sent := watchOnce(t, snap, token)
		assert.Equal(t, 1, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
	})

	t.Run("pre-#1193 counter token: full snapshot", func(t *testing.T) {
		snap := NewSnapshot()
		snap.DiffAndReplaceAt(two, Origin{Revision: 7})
		sent := watchOnce(t, snap, "7")
		assert.Equal(t, 2, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT),
			"a bare integer embeds no hash and never matches, even equal to the revision")
	})
}

// TestBroadcast_VersionOnEachWatchersLastEvent (#1203): a batch reaches each
// watcher with the version on the last event THAT watcher receives; a filtered
// watcher whose subset ends earlier gets it there.
func TestBroadcast_VersionOnEachWatchersLastEvent(t *testing.T) {
	b := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
	full := b.Subscribe("full", nil)
	onlyA := b.Subscribe("a", []string{"ns/a"})

	batch := []*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "ns/a", Endpoint: ep("10.0.0.1"), Version: "v"},
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "ns/a", Endpoint: ep("10.0.0.2"), Version: "v"},
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "ns/b", Endpoint: ep("10.0.1.1"), Version: "v"},
	}
	b.Broadcast(batch)

	drain := func(ch <-chan *registrarv1.WatchEndpointsResponse, n int) []string {
		out := make([]string, 0, n)
		for range n {
			out = append(out, (<-ch).GetVersion())
		}
		return out
	}
	assert.Equal(t, []string{"", "", "v"}, drain(full, 3))
	assert.Equal(t, []string{"", "v"}, drain(onlyA, 2))
	for _, e := range batch {
		assert.Equal(t, "v", e.GetVersion(), "the caller's events must not be mutated")
	}
}
