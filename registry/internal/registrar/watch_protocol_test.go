package registrar

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// protocolFakeServer answers WatchEndpoints the way the registrar does, over a
// fixed set of contents at one version: current on last_version == version, an
// extension on a matching partial_resume, a full (filtered) resend otherwise.
// It records each outcome so a test can assert both what the client asked for
// and what its cache ends up holding (#1239 review).
type protocolFakeServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	version  string
	contents map[string][]string // service -> IPs

	mu       sync.Mutex
	outcomes []string
}

func (s *protocolFakeServer) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	var filter map[string]struct{}
	if req.GetFilter() != nil {
		filter = serviceSet(req.GetFilter().GetServices())
		if filter == nil {
			filter = map[string]struct{}{}
		}
	}
	have := map[string]struct{}{}
	outcome, typ, extended := "resent", registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, false
	switch {
	case req.GetLastVersion() == s.version:
		outcome = "current"
	case req.GetLastVersion() == "" && req.GetPartialResume().GetVersion() == s.version:
		outcome, typ, extended = "extended", registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, true
		have = serviceSet(req.GetPartialResume().GetServices())
	}
	s.mu.Lock()
	s.outcomes = append(s.outcomes, outcome)
	s.mu.Unlock()

	var sends []*registrarv1.WatchEndpointsResponse
	if outcome != "current" {
		for svc, ips := range s.contents {
			if _, held := have[svc]; held || !inScope(filter, svc) {
				continue
			}
			for _, ip := range ips {
				sends = append(sends, &registrarv1.WatchEndpointsResponse{
					Type: typ, ServiceName: svc, Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: makeEndpoint(ip, 8080),
				})
			}
		}
	}
	sends = append(sends, &registrarv1.WatchEndpointsResponse{
		Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: s.version, Extended: extended,
	})
	for _, e := range sends {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *protocolFakeServer) outcomeLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outcomes)
}

func startProtocolFake(t *testing.T, fake *protocolFakeServer) []grpc.DialOption {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	registrarv1.RegisterRegistrarServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	}
}

// cacheView is the cache as service -> sorted IPs.
func cacheView(r *RegistrarRegistry) map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string][]string{}
	for _, byName := range r.cache {
		for svc, eps := range byName {
			for _, ep := range eps {
				out[svc] = append(out[svc], ep.GetIp())
			}
			slices.Sort(out[svc])
		}
	}
	return out
}

// TestWatchLoop_DependencySetChangeCacheContents is the sibling of
// TestWatchLoop_DependencySetChangeKeepsTheToken against a fake that answers as
// the registrar does: through narrow, shrink and grow the client is resent
// exactly once (its first, tokenless stream), and its cache holds exactly the
// filter's endpoints after each change.
func TestWatchLoop_DependencySetChangeCacheContents(t *testing.T) {
	fake := &protocolFakeServer{
		version: "40.0123456789abcdef",
		contents: map[string][]string{
			"default/a": {"10.0.0.1"},
			"default/b": {"10.0.1.1", "10.0.1.2"},
			"default/c": {"10.0.2.1"},
		},
	}
	r, logs := newLoggingRegistry(t, startProtocolFake(t, fake))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()
	require.NoError(t, r.WaitReady(ctx))

	step := func(filter []string, want map[string][]string, outcomes []string) {
		t.Helper()
		r.SetServiceFilter(filter)
		require.Eventuallyf(t, func() bool {
			return slices.Equal(fake.outcomeLog(), outcomes) && assert.ObjectsAreEqual(want, cacheView(r))
		}, 10*time.Second, time.Millisecond, "filter %v: outcomes %v cache %v; logs:\n%s",
			filter, fake.outcomeLog(), cacheView(r), logs)
	}

	require.Equal(t, map[string][]string{
		"default/a": {"10.0.0.1"}, "default/b": {"10.0.1.1", "10.0.1.2"}, "default/c": {"10.0.2.1"},
	}, cacheView(r))
	step([]string{"default/a", "default/b"},
		map[string][]string{"default/a": {"10.0.0.1"}, "default/b": {"10.0.1.1", "10.0.1.2"}},
		[]string{"resent", "current"})
	step([]string{"default/a"},
		map[string][]string{"default/a": {"10.0.0.1"}},
		[]string{"resent", "current", "current"})
	step([]string{"default/a", "default/c"},
		map[string][]string{"default/a": {"10.0.0.1"}, "default/c": {"10.0.2.1"}},
		[]string{"resent", "current", "current", "extended"})
	step([]string{"default/a", "default/b", "default/c"},
		map[string][]string{"default/a": {"10.0.0.1"}, "default/b": {"10.0.1.1", "10.0.1.2"}, "default/c": {"10.0.2.1"}},
		[]string{"resent", "current", "current", "extended", "extended"})
}
