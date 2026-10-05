package server

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// hookStream is a WatchEndpoints stream that runs onSend after recording each
// event, on the WatchEndpoints goroutine and outside its own lock: a test uses
// it to land a registry change at an exact point of the initial exchange.
type hookStream struct {
	grpc.ServerStream
	ctx    context.Context
	onSend func(*registrarv1.WatchEndpointsResponse)

	mu   sync.Mutex
	sent []*registrarv1.WatchEndpointsResponse
}

func (f *hookStream) Send(e *registrarv1.WatchEndpointsResponse) error {
	f.mu.Lock()
	f.sent = append(f.sent, e)
	f.mu.Unlock()
	if f.onSend != nil {
		f.onSend(e)
	}
	return nil
}
func (f *hookStream) Context() context.Context { return f.ctx }

func (f *hookStream) received() []*registrarv1.WatchEndpointsResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// agentCache replays a watch stream the way the agent's cache does
// (registry/internal/registrar applyEvent/upsertLocked/removeLocked): the first
// FULL_SNAPSHOT clears the cache, endpoints are keyed by (protocol, service, ip),
// and a REMOVED event removes only under the protocol it names. It also returns
// the resume token the agent ends up holding (agentResumeToken).
func agentCache(events []*registrarv1.WatchEndpointsResponse) (map[registryv1.Service_Protocol]map[string]map[string]bool, string) {
	cache := map[registryv1.Service_Protocol]map[string]map[string]bool{}
	cleared := false
	for _, e := range events {
		switch e.GetType() {
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
			registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
			if e.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT && !cleared {
				cache, cleared = map[registryv1.Service_Protocol]map[string]map[string]bool{}, true
			}
			if cache[e.GetProtocol()] == nil {
				cache[e.GetProtocol()] = map[string]map[string]bool{}
			}
			if cache[e.GetProtocol()][e.GetServiceName()] == nil {
				cache[e.GetProtocol()][e.GetServiceName()] = map[string]bool{}
			}
			cache[e.GetProtocol()][e.GetServiceName()][e.GetEndpoint().GetIp()] = true
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
			delete(cache[e.GetProtocol()][e.GetServiceName()], e.GetEndpoint().GetIp())
		}
	}
	return cache, agentResumeToken("", events)
}

func cacheHas(cache map[registryv1.Service_Protocol]map[string]map[string]bool, protocol registryv1.Service_Protocol, svc, ip string) bool {
	return cache[protocol][svc][ip]
}

func newOrderingTestServer() *RegistrarServer {
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/base": {"10.0.0.1"}}), Origin{Revision: 40})
	reg := &flakyRegistry{}
	s := NewRegistrarServer(reg, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), nil)
	// Snapshot-first, as deployed (the queue is never started: the external
	// writes are not under test).
	s.UseWriteBehind(NewWriteBehindQueue(reg, slog.New(slog.DiscardHandler), nil))
	return s
}

func addReq(svc, ip string, protocol registryv1.Service_Protocol) *registrarv1.RegisterEndpointRequest {
	return &registrarv1.RegisterEndpointRequest{ServiceName: svc, Protocol: protocol, Endpoint: &registryv1.ServiceEndpoint{Ip: ip, Port: 8080}}
}

// TestWatchEndpoints_ChangeDuringInitialExchangeIsDelivered (#1205): a batch
// applied and broadcast while the initial exchange is on the wire -- here,
// right as SNAPSHOT_COMPLETE goes out, and mid-snapshot -- must reach the
// stream. Red before #1205: WatchEndpoints subscribed only after the marker, so
// the broadcast found no subscription and the change was lost on that stream
// until an unrelated event touched the service.
func TestWatchEndpoints_ChangeDuringInitialExchangeIsDelivered(t *testing.T) {
	for _, at := range []registrarv1.WatchEndpointsResponse_EventType{
		registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
		registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
	} {
		t.Run(at.String(), func(t *testing.T) {
			s := newOrderingTestServer()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var once sync.Once
			stream := &hookStream{ctx: ctx}
			stream.onSend = func(e *registrarv1.WatchEndpointsResponse) {
				if e.GetType() != at {
					return
				}
				once.Do(func() {
					_, err := s.RegisterEndpoint(context.Background(), addReq("ns/late", "10.0.0.7", registryv1.Service_PROTOCOL_HTTP))
					assert.NoError(t, err)
				})
			}
			done := make(chan error, 1)
			go func() {
				done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n"}, stream)
			}()

			assert.Eventually(t, func() bool {
				cache, _ := agentCache(stream.received())
				return cacheHas(cache, registryv1.Service_PROTOCOL_HTTP, "ns/late", "10.0.0.7")
			}, 2*time.Second, 5*time.Millisecond, "a change broadcast during the initial exchange never reached the stream")

			_, token := agentCache(stream.received())
			assert.Equal(t, s.snapshot.Version(), token, "the stream must end on the version naming the contents it delivered")
			cancel()
			<-done
		})
	}
}

// TestWatchEndpoints_PreSnapshotBatchCannotRegressTheToken (#1205): batch B is
// applied to the snapshot but not yet broadcast when a later batch C lands and
// a watch starts. B's broadcast carries B's version, which names contents
// WITHOUT C. Had the watch subscribed and read the snapshot (B and C) before
// B's broadcast, B would reach the stream after SNAPSHOT_COMPLETE and move the
// agent's token back to a version its cache (which holds C) does not match.
//
// The rule that prevents it: a batch's apply and its broadcast are one
// publication (Broadcaster.Publish), and a watch's subscribe and snapshot read
// happen with publications excluded (Broadcaster.SubscribeWith). B is therefore
// either wholly before the watch start (in the snapshot, not delivered) or
// wholly after (delivered, not in the snapshot) -- never both.
func TestWatchEndpoints_PreSnapshotBatchCannotRegressTheToken(t *testing.T) {
	s := newOrderingTestServer()

	applied := make(chan struct{})
	release := make(chan struct{})
	publishedB := make(chan struct{})
	go func() {
		defer close(publishedB)
		s.broadcaster.Publish(func() []*registrarv1.WatchEndpointsResponse {
			events := []*registrarv1.WatchEndpointsResponse{{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
				ServiceName: "ns/b",
				Protocol:    registryv1.Service_PROTOCOL_HTTP,
				Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.2"},
			}}
			version, transitions := s.snapshot.Apply(events)
			events = append(events, transitions...)
			for _, e := range events {
				e.Version = version
			}
			close(applied)
			<-release // B is in the snapshot; its broadcast is still pending
			return events
		})
	}()
	<-applied

	// C lands while B's publication is open. Since publications are serialized
	// (#1239 review, F2) it waits for B instead of overtaking it; before that it
	// was published in full here. Either way the watch below must end on the
	// snapshot's version.
	publishedC := make(chan error, 1)
	go func() {
		_, err := s.RegisterEndpoint(context.Background(), addReq("ns/c", "10.0.0.3", registryv1.Service_PROTOCOL_HTTP))
		publishedC <- err
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &hookStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n"}, stream)
	}()

	// Give the watch every chance to start before B's broadcast goes out.
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-publishedB
	require.NoError(t, <-publishedC)

	require.Eventually(t, func() bool {
		for _, e := range stream.received() {
			if e.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "marker never sent")
	// Let anything buffered for the stream drain onto it.
	time.Sleep(50 * time.Millisecond)

	cache, token := agentCache(stream.received())
	assert.True(t, cacheHas(cache, registryv1.Service_PROTOCOL_HTTP, "ns/b", "10.0.0.2"))
	assert.True(t, cacheHas(cache, registryv1.Service_PROTOCOL_HTTP, "ns/c", "10.0.0.3"))
	assert.Equal(t, s.snapshot.Version(), token,
		"a batch applied before the snapshot was read must not move the token off the snapshot's version")
	cancel()
	<-done
}

// TestUnregisterEndpoint_RemovesUnderTheRegisteredProtocol (#1206): an endpoint
// registered and then unregistered through the RPCs is gone from the snapshot
// and from a watching agent's cache at once, without waiting for a sync, for
// every protocol. Red before #1206: UnregisterEndpointRequest carries no
// protocol and the REMOVED events were built without one, so the snapshot
// removal keyed (service, PROTOCOL_UNSPECIFIED, ip) and matched nothing, and the
// agent's removal (keyed by protocol too) matched nothing either.
func TestUnregisterEndpoint_RemovesUnderTheRegisteredProtocol(t *testing.T) {
	for _, protocol := range []registryv1.Service_Protocol{
		registryv1.Service_PROTOCOL_HTTP,
		registryv1.Service_PROTOCOL_TCP,
		registryv1.Service_PROTOCOL_UDP,
	} {
		t.Run(protocol.String(), func(t *testing.T) {
			s := newOrderingTestServer()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &hookStream{ctx: ctx}
			done := make(chan error, 1)
			go func() {
				done <- s.WatchEndpoints(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n"}, stream)
			}()
			require.Eventually(t, func() bool { return len(stream.received()) > 0 }, 2*time.Second, 5*time.Millisecond)

			before := s.snapshot.State()
			_, err := s.RegisterEndpoint(context.Background(), addReq("ns/svc", "10.0.0.5", protocol))
			require.NoError(t, err)
			_, err = s.RegisterEndpoint(context.Background(), addReq("ns/svc", "10.0.0.6", protocol))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				cache, _ := agentCache(stream.received())
				return cacheHas(cache, protocol, "ns/svc", "10.0.0.5")
			}, 2*time.Second, 5*time.Millisecond)

			_, err = s.UnregisterEndpoint(context.Background(), &registrarv1.UnregisterEndpointRequest{ServiceName: "ns/svc", Ips: []string{"10.0.0.5", "10.0.0.6"}})
			require.NoError(t, err)

			assert.NotContains(t, s.snapshot.GetAll(protocol), "ns/svc", "the snapshot must drop the endpoints at once, not at the next sync")
			assert.NotContains(t, s.snapshot.ServiceNames(), "ns/svc", "the catalog must drop the emptied service")
			assert.Equal(t, before.ContentHash, s.snapshot.State().ContentHash, "register+unregister must return to the same contents")

			assert.Eventually(t, func() bool {
				cache, token := agentCache(stream.received())
				return !cacheHas(cache, protocol, "ns/svc", "10.0.0.5") &&
					!cacheHas(cache, protocol, "ns/svc", "10.0.0.6") &&
					token == s.snapshot.Version()
			}, 2*time.Second, 5*time.Millisecond, "the watching agent's cache must drop the endpoints, ending on the snapshot's version")
			cancel()
			<-done
		})
	}
}
