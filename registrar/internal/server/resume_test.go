package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// Issue #1193 / #1203: the snapshot version is a resume token. It must name the
// snapshot's CONTENTS (so it survives a no-op sync and is comparable across
// replicas), and the server may only hand it out at a point where the receiver
// holds the complete state it names.

// listing builds a sync-shaped state: service -> protocol -> endpoints (HTTP).
func listing(svcIPs map[string][]string) map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint {
	out := make(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, len(svcIPs))
	for svc, ips := range svcIPs {
		eps := make([]*registryv1.ServiceEndpoint, 0, len(ips))
		for _, ip := range ips {
			eps = append(eps, &registryv1.ServiceEndpoint{Ip: ip, Port: 8080})
		}
		out[svc] = map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{registryv1.Service_PROTOCOL_HTTP: eps}
	}
	return out
}

// TestSnapshot_NoOpSyncKeepsVersion: two syncs that list the same world leave
// the version where it was. Red before #1193: every Replace bumped a counter,
// so a reconnecting agent's last_version went stale on every poll.
func TestSnapshot_NoOpSyncKeepsVersion(t *testing.T) {
	s := NewSnapshot()
	state := listing(map[string][]string{"ns/a": {"10.0.0.1", "10.0.0.2"}})
	_, v1, _ := s.DiffAndReplace(state)
	_, v2, _ := s.DiffAndReplace(listing(map[string][]string{"ns/a": {"10.0.0.2", "10.0.0.1"}}))
	_, v3, _ := s.DiffAndReplace(state)
	assert.Equal(t, v1, v2, "a no-op sync must not move the version")
	assert.Equal(t, v1, v3, "a no-op sync must not move the version")

	_, v4, _ := s.DiffAndReplace(listing(map[string][]string{"ns/a": {"10.0.0.1"}}))
	assert.NotEqual(t, v1, v4, "a real change must move the version")
}

// TestSnapshot_VersionIsComparableAcrossReplicas: two replicas holding the same
// contents report the same version however many syncs each has run, and two
// replicas holding different contents never do. Red before #1193: each replica
// counted its own syncs, so equal counters said nothing about equal contents and
// a reconnect landing on the other replica could skip a snapshot it needed.
func TestSnapshot_VersionIsComparableAcrossReplicas(t *testing.T) {
	a, b := NewSnapshot(), NewSnapshot()
	state := listing(map[string][]string{"ns/a": {"10.0.0.1"}})

	_, va, _ := a.DiffAndReplace(state)
	b.DiffAndReplace(state)
	_, vb, _ := b.DiffAndReplace(state) // b has polled once more
	assert.Equal(t, va, vb, "same contents must carry the same version on every replica")

	c := NewSnapshot()
	_, vc, _ := c.DiffAndReplace(listing(map[string][]string{"ns/a": {"10.0.0.9"}}))
	assert.NotEqual(t, va, vc, "different contents must never share a version")
}

// truncatingStream is a WatchEndpoints stream whose transport dies after limit
// sends: the send that would exceed it fails, as a registrar roll or a reset
// connection does mid-stream.
type truncatingStream struct {
	grpc.ServerStream
	ctx   context.Context
	limit int

	mu   sync.Mutex
	sent []*registrarv1.WatchEndpointsResponse
}

var errTransportGone = errors.New("transport is closing")

func (f *truncatingStream) Send(e *registrarv1.WatchEndpointsResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) >= f.limit {
		return errTransportGone
	}
	f.sent = append(f.sent, e)
	return nil
}
func (f *truncatingStream) Context() context.Context { return f.ctx }

func (f *truncatingStream) received() []*registrarv1.WatchEndpointsResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*registrarv1.WatchEndpointsResponse(nil), f.sent...)
}

// agentResumeToken applies the agent's rule (registry/internal/registrar
// processStream): the resume token is the last non-empty version received.
func agentResumeToken(start string, events []*registrarv1.WatchEndpointsResponse) string {
	token := start
	for _, e := range events {
		if e.GetVersion() != "" {
			token = e.GetVersion()
		}
	}
	return token
}

func countType(events []*registrarv1.WatchEndpointsResponse, t registrarv1.WatchEndpointsResponse_EventType) int {
	n := 0
	for _, e := range events {
		if e.GetType() == t {
			n++
		}
	}
	return n
}

// reconnect opens a fresh watch with token and returns what the server sent up
// to and including SNAPSHOT_COMPLETE.
func reconnect(t *testing.T, s *RegistrarServer, token string) []*registrarv1.WatchEndpointsResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeWatchServerStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{NodeName: "n2", LastVersion: token}, stream)
	}()
	require.Eventually(t, func() bool {
		return countType(stream.snapshot(), registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE) > 0
	}, 5*time.Second, 10*time.Millisecond, "snapshot-complete marker never sent")
	cancel()
	<-done
	return stream.snapshot()
}

func newResumeTestServer(t *testing.T, ips ...string) *RegistrarServer {
	t.Helper()
	snap := NewSnapshot()
	snap.DiffAndReplace(listing(map[string][]string{"ns/a": ips}))
	s := NewRegistrarServer(&flakyRegistry{}, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)
	return s
}

// TestWatchEndpoints_TruncatedSnapshotIsResentOnReconnect (#1203): a stream
// that dies after the first FULL_SNAPSHOT event leaves the agent with a cleared,
// one-endpoint cache. Its reconnect must get the full snapshot again.
//
// Red before #1203: every FULL_SNAPSHOT event carried the version, so the agent
// adopted it from the first one, and the reconnect -- the contents unchanged --
// matched and was told it was current.
func TestWatchEndpoints_TruncatedSnapshotIsResentOnReconnect(t *testing.T) {
	s := newResumeTestServer(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")

	stream := &truncatingStream{ctx: context.Background(), limit: 1}
	err := s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{NodeName: "n1"}, stream)
	require.ErrorIs(t, err, errTransportGone)
	got := stream.received()
	require.Len(t, got, 1)
	require.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, got[0].GetType())

	sent := reconnect(t, s, agentResumeToken("", got))
	assert.Equal(t, 3, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT),
		"a reconnect after a truncated snapshot must receive the whole snapshot")
}

// TestWatchEndpoints_TruncatedBatchIsResentOnReconnect (#1203): the stream dies
// between two events of one broadcast batch (here ENDPOINT_ADDED and the
// SERVICE_ADDED it caused). The agent missed part of the batch, so its
// reconnect must not be treated as current.
//
// Red before #1203: every event of a batch carried the batch's version, so the
// agent adopted it from the first one.
func TestWatchEndpoints_TruncatedBatchIsResentOnReconnect(t *testing.T) {
	s := newResumeTestServer(t, "10.0.0.1")

	// Initial snapshot: 1 FULL_SNAPSHOT + 1 catalog SERVICE_ADDED + marker; then
	// exactly one incremental event gets through.
	stream := &truncatingStream{ctx: context.Background(), limit: 4}
	done := make(chan error, 1)
	go func() { done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{NodeName: "n1"}, stream) }()
	require.Eventually(t, func() bool { return s.broadcaster.WatcherCount() == 1 },
		5*time.Second, 10*time.Millisecond, "watcher never subscribed")

	_, err := s.RegisterEndpoint(context.Background(), &registrarv1.RegisterEndpointRequest{
		ServiceName: "ns/new",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.1.1", Port: 8080},
	})
	require.NoError(t, err)
	require.ErrorIs(t, <-done, errTransportGone)

	got := stream.received()
	require.Len(t, got, 4)
	require.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, got[3].GetType(),
		"the stream must die inside the batch, after its first event")

	sent := reconnect(t, s, agentResumeToken("", got))
	assert.Equal(t, 2, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT),
		"a reconnect after a truncated batch must receive the whole snapshot")
	assert.Equal(t, 2, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED),
		"...and the whole service catalog")
}

// TestWatchEndpoints_CompleteStreamResumesWithoutResend: the converse. An agent
// that received a whole snapshot and a whole batch reconnects without a resend,
// even after a no-op sync ran in between (#1193).
func TestWatchEndpoints_CompleteStreamResumesWithoutResend(t *testing.T) {
	s := newResumeTestServer(t, "10.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeWatchServerStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{NodeName: "n1"}, stream) }()
	require.Eventually(t, func() bool { return s.broadcaster.WatcherCount() == 1 },
		5*time.Second, 10*time.Millisecond, "watcher never subscribed")
	_, err := s.RegisterEndpoint(context.Background(), &registrarv1.RegisterEndpointRequest{
		ServiceName: "ns/a",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.2", Port: 8080},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(stream.snapshot()) == 4 },
		5*time.Second, 10*time.Millisecond, "batch never delivered")
	cancel()
	<-done

	// A sync lists exactly what the snapshot already holds.
	s.snapshot.DiffAndReplace(listing(map[string][]string{"ns/a": {"10.0.0.1", "10.0.0.2"}}))

	sent := reconnect(t, s, agentResumeToken("", stream.snapshot()))
	require.Len(t, sent, 1, "a current agent gets the marker alone")
	assert.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, sent[0].GetType())
}
