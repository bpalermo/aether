package registrar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/snapshotversion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// Issue #1269: a watch stream cut in the middle of a batch leaves the batch's
// unversioned prefix applied to the cache while the token still names the
// previous version (#1203: only the batch's last event carries the new one).
// If the registrar's contents then return to the hash that token names (a
// change and its exact reversal, or a peer replica that never received the
// change), a reconnect presenting the token is answered current/renamed, or
// extended for a partial resume, and the prefix is never undone.

// modelRegistrar answers WatchEndpoints the way the registrar does over
// mutable contents named by a "<rev>.<hash>" version: current/renamed when the
// token's content hash is the current one, extended for a matching
// partial_resume, a full filtered resend otherwise. After the marker it sends
// the batches the test pushes, versioned only on the last event (#1203), and
// can cut a batch after any of its events.
type modelRegistrar struct {
	registrarv1.UnimplementedRegistrarServiceServer

	mu       sync.Mutex
	rev      int64
	contents map[string]map[string]struct{} // service -> IPs
	outcomes []string
	// requests are the watch requests received, in order (index = stream).
	requests []*registrarv1.WatchEndpointsRequest

	// beforeMarker, when set, runs after stream n's (1-based) initial events
	// are sent and before its SNAPSHOT_COMPLETE: a test holds the initial
	// exchange open there (#1334). Set before the server starts.
	beforeMarker func(ctx context.Context, n int)

	live chan modelStep
}

// modelStep is one batch pushed onto the live stream.
type modelStep struct {
	events []*registrarv1.WatchEndpointsResponse
	// cutAfter ends the stream after this many of the batch's events (< 0: the
	// whole batch is sent).
	cutAfter int
	// onCut runs before the stream ends at the cut, so it is ordered before the
	// client's reconnect.
	onCut func()
	// holdOnCut keeps the stream open at the cut until the client cancels it,
	// instead of ending it (a dependency-set change re-opening the stream).
	holdOnCut bool
}

func newModelRegistrar(rev int64, contents map[string][]string) *modelRegistrar {
	m := &modelRegistrar{rev: rev, live: make(chan modelStep)}
	m.reset(rev, contents)
	return m
}

// reset replaces the contents and the revision: a peer replica serving an
// older state.
func (m *modelRegistrar) reset(rev int64, contents map[string][]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rev = rev
	m.contents = map[string]map[string]struct{}{}
	for svc, ips := range contents {
		m.contents[svc] = map[string]struct{}{}
		for _, ip := range ips {
			m.contents[svc][ip] = struct{}{}
		}
	}
}

// versionLocked names the contents. Caller holds mu.
func (m *modelRegistrar) versionLocked() string {
	var lines []string
	for svc, ips := range m.contents {
		for ip := range ips {
			lines = append(lines, svc+"|"+ip)
		}
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return snapshotversion.Format(m.rev, false, hex.EncodeToString(sum[:])[:snapshotversion.HashLen])
}

// view is the contents as service -> sorted IPs, scoped to filter (nil = all):
// what a full filtered resend leaves in a client's cache.
func (m *modelRegistrar) view(filter map[string]struct{}) map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for svc, ips := range m.contents {
		if !inScope(filter, svc) {
			continue
		}
		for ip := range ips {
			out[svc] = append(out[svc], ip)
		}
		slices.Sort(out[svc])
	}
	return out
}

// commit applies a batch to the contents at the next revision and stamps the
// new version on its last event, as the registrar's Broadcast does.
func (m *modelRegistrar) commit(events []*registrarv1.WatchEndpointsResponse) []*registrarv1.WatchEndpointsResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rev++
	for _, e := range events {
		svc, ip := e.GetServiceName(), e.GetEndpoint().GetIp()
		switch e.GetType() {
		case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
			delete(m.contents[svc], ip)
			if len(m.contents[svc]) == 0 {
				delete(m.contents, svc)
			}
		default:
			if m.contents[svc] == nil {
				m.contents[svc] = map[string]struct{}{}
			}
			m.contents[svc][ip] = struct{}{}
		}
	}
	events[len(events)-1].Version = m.versionLocked()
	return events
}

func (m *modelRegistrar) outcomeLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.outcomes)
}

func (m *modelRegistrar) requestLog() []*registrarv1.WatchEndpointsRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

func (m *modelRegistrar) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	var filter map[string]struct{}
	if req.GetFilter() != nil {
		filter = serviceSet(req.GetFilter().GetServices())
		if filter == nil {
			filter = map[string]struct{}{}
		}
	}

	m.mu.Lock()
	current := m.versionLocked()
	outcome, typ, extended, have := "resent", registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, false, map[string]struct{}{}
	switch token := req.GetLastVersion(); {
	case token != "":
		switch snapshotversion.Compare(token, current) {
		case snapshotversion.Same:
			outcome = "current"
		case snapshotversion.Renamed:
			outcome = "renamed"
		case snapshotversion.Different:
		}
	case req.GetPartialResume().GetVersion() != "":
		if snapshotversion.Compare(req.GetPartialResume().GetVersion(), current) != snapshotversion.Different {
			outcome, typ, extended = "extended", registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, true
			have = serviceSet(req.GetPartialResume().GetServices())
		}
	}
	m.outcomes = append(m.outcomes, outcome)
	m.requests = append(m.requests, req)
	n := len(m.outcomes)
	var sends []*registrarv1.WatchEndpointsResponse
	if outcome == "resent" || outcome == "extended" {
		for svc, ips := range m.contents {
			if _, held := have[svc]; held || !inScope(filter, svc) {
				continue
			}
			for ip := range ips {
				sends = append(sends, &registrarv1.WatchEndpointsResponse{
					Type: typ, ServiceName: svc, Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: makeEndpoint(ip, 8080),
				})
			}
		}
	}
	m.mu.Unlock()
	for _, e := range sends {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	if m.beforeMarker != nil {
		m.beforeMarker(stream.Context(), n)
	}
	if err := stream.Send(&registrarv1.WatchEndpointsResponse{
		Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: current, Extended: extended,
	}); err != nil {
		return err
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case step := <-m.live:
			for i, e := range step.events {
				if i == step.cutAfter {
					if step.onCut != nil {
						step.onCut()
					}
					if step.holdOnCut {
						<-stream.Context().Done()
						return stream.Context().Err()
					}
					return nil // the client sees a clean EOF and reconnects at once
				}
				if err := stream.Send(e); err != nil {
					return err
				}
			}
		}
	}
}

func startModelRegistrar(t *testing.T, m *modelRegistrar) []grpc.DialOption {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	registrarv1.RegisterRegistrarServiceServer(srv, m)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	}
}

func modelEvent(typ registrarv1.WatchEndpointsResponse_EventType, svc, ip string) *registrarv1.WatchEndpointsResponse {
	return &registrarv1.WatchEndpointsResponse{
		Type: typ, ServiceName: svc, Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: makeEndpoint(ip, 8080),
	}
}

// reversal is the batch that undoes events, in reverse order.
func reversal(events []*registrarv1.WatchEndpointsResponse) []*registrarv1.WatchEndpointsResponse {
	out := make([]*registrarv1.WatchEndpointsResponse, 0, len(events))
	for _, e := range slices.Backward(events) {
		typ := registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED
		if e.GetType() != registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED {
			typ = registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED
		}
		out = append(out, modelEvent(typ, e.GetServiceName(), e.GetEndpoint().GetIp()))
	}
	return out
}

// TestWatchLoop_MidBatchCutThenContentsReturn drives the real watch loop
// against modelRegistrar through the #1269 interleaving and asserts that the
// cache ends equal to a full filtered resend of the registrar's contents:
//
//  1. The client is resent C0 = {a: .0.1, b: .1.1, c: .2.1} and holds token V0.
//  2. Batch B1 = [REMOVED a/10.0.0.1, ADDED b/10.0.1.2] is committed (V1) and
//     the stream is cut after its first event: the client applied REMOVED a,
//     unversioned, and still holds V0.
//  3. Before the reconnect the contents return to C0's hash.
//  4. The client reconnects presenting V0.
//
// Red before #1269's fix: the registrar answers renamed/current/extended,
// nothing is resent, and a stays missing from the cache for good.
func TestWatchLoop_MidBatchCutThenContentsReturn(t *testing.T) {
	c0 := map[string][]string{
		"default/a": {"10.0.0.1"},
		"default/b": {"10.0.1.1"},
		"default/c": {"10.0.2.1"},
	}
	batch := func() []*registrarv1.WatchEndpointsResponse {
		return []*registrarv1.WatchEndpointsResponse{
			modelEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED, "default/a", "10.0.0.1"),
			modelEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/b", "10.0.1.2"),
		}
	}

	type tc struct {
		name string
		// filter is asserted after the first resend (nil = stay on the full watch).
		filter []string
		// revert returns the contents to C0's hash (m is the registrar the
		// client reconnects to).
		revert func(m *modelRegistrar, b1 []*registrarv1.WatchEndpointsResponse)
		// grow, when set, is the dependency-set growth that re-opens the stream
		// at the cut (the partial_resume path).
		grow     []string
		outcomes []string
	}
	cases := []tc{
		{
			name: "ABA: the change and its exact reversal",
			revert: func(m *modelRegistrar, b1 []*registrarv1.WatchEndpointsResponse) {
				m.commit(reversal(b1)) // C0 at a later revision: renamed
			},
			outcomes: []string{"resent", "resent"},
		},
		{
			name: "a peer replica that never received the change",
			revert: func(m *modelRegistrar, _ []*registrarv1.WatchEndpointsResponse) {
				m.reset(10, c0) // the very version the client holds: current
			},
			outcomes: []string{"resent", "resent"},
		},
		{
			name:   "partial resume: a dependency-set growth re-opens the stream at the cut",
			filter: []string{"default/a", "default/b"},
			revert: func(m *modelRegistrar, b1 []*registrarv1.WatchEndpointsResponse) {
				m.commit(reversal(b1))
			},
			grow:     []string{"default/a", "default/b", "default/c"},
			outcomes: []string{"resent", "current", "resent"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newModelRegistrar(10, c0)
			r, logs := newLoggingRegistry(t, startModelRegistrar(t, m))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			require.NoError(t, r.Initialize(ctx))
			defer func() { _ = r.Close() }()
			require.NoError(t, r.WaitReady(ctx))
			require.Equal(t, m.view(nil), cacheView(r))

			want := 1
			if c.filter != nil {
				r.SetServiceFilter(c.filter)
				want++
				require.Eventually(t, func() bool { return len(m.outcomeLog()) == want }, 10*time.Second, time.Millisecond)
			}

			b1 := m.commit(batch())
			step := modelStep{events: b1, cutAfter: 1}
			if c.grow == nil {
				step.onCut = func() { c.revert(m, b1) }
			} else {
				step.holdOnCut = true
			}
			m.live <- step
			if c.grow != nil {
				// The prefix is applied (a is gone, b has not grown yet) before
				// the dependency set changes. A stream that ends by itself
				// delivers its prefix before the EOF, and the reconnect follows
				// at once, so the other cases need no wait (and could miss it).
				require.Eventually(t, func() bool { _, ok := cacheView(r)["default/a"]; return !ok },
					10*time.Second, time.Millisecond, "the batch prefix never landed; logs:\n%s", logs)
				c.revert(m, b1)
				r.SetServiceFilter(c.grow)
			}

			final := serviceSet(c.filter)
			if c.grow != nil {
				final = serviceSet(c.grow)
			}
			require.Eventually(t, func() bool { return len(m.outcomeLog()) == want+1 }, 10*time.Second, time.Millisecond)
			assert.Eventuallyf(t, func() bool { return assert.ObjectsAreEqual(m.view(final), cacheView(r)) },
				2*time.Second, time.Millisecond, "the cache is not the registrar's contents: outcomes %v, cache %v, want %v; logs:\n%s",
				m.outcomeLog(), cacheView(r), m.view(final), logs)
			assert.Equal(t, c.outcomes, m.outcomeLog())
		})
	}
}

// TestConsumeStream_MidBatchToken pins when a stream's end keeps its token and
// when it drops it (#1269). Every stream resumes at resumeToken with the cache
// holding a and b, gets the marker (current), then the listed live events.
func TestConsumeStream_MidBatchToken(t *testing.T) {
	const v2 = "8.0123456789abcdef"
	const v3 = "9.0123456789abcdef"
	type ev = *registrarv1.WatchEndpointsResponse
	added := func(svc, ip, version string) ev {
		return epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, svc, ip, version)
	}
	marker := func(version string) ev {
		return &registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: version}
	}
	cases := []struct {
		name string
		live []ev
		end  error
		want string
	}{
		{name: "cut before any live event keeps the token", want: resumeToken},
		{
			name: "cut exactly at a batch's versioned last event keeps it",
			live: []ev{added("default/a", "10.0.0.7", ""), added("default/b", "10.0.1.7", v2)},
			want: v2,
		},
		{
			name: "cut after a batch's unversioned prefix drops it",
			live: []ev{added("default/a", "10.0.0.7", "")},
			want: "",
		},
		{
			name: "cut inside the second batch drops even the first batch's version",
			live: []ev{added("default/a", "10.0.0.7", v2), added("default/b", "10.0.1.7", "")},
			want: "",
		},
		{
			name: "a catalog event is part of the batch too",
			live: []ev{{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED, ServiceName: "default/a"}},
			want: "",
		},
		{
			name: "a version marker is versioned: it ends the batch",
			live: []ev{added("default/a", "10.0.0.7", v2), marker(v3)},
			want: v3,
		},
		{
			name: "an old registrar versions every event: never in a batch",
			live: []ev{added("default/a", "10.0.0.7", v2), added("default/b", "10.0.1.7", v3)},
			want: v3,
		},
		{
			name: "a dependency-set change cancelling the stream mid-batch drops it",
			live: []ev{added("default/a", "10.0.0.7", "")},
			end:  status.Error(codes.Canceled, "context canceled"),
			want: "",
		},
		{
			name: "a server drain mid-batch drops it",
			live: []ev{added("default/a", "10.0.0.7", "")},
			end:  status.Error(codes.Unavailable, "closing transport due to: EOF, "+goawayNoErrorDetail),
			want: "",
		},
		{
			name: "a failure mid-batch drops it",
			live: []ev{added("default/a", "10.0.0.7", "")},
			end:  status.Error(codes.Unavailable, "connection reset"),
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ab := []string{"default/a", "default/b"}
			r := heldAfterStart(t, ab...)
			var reader *sdkmetric.ManualReader
			r.metrics, reader = newTestClientMetrics(t)
			open := r.resumeFor(serviceSet(ab), resumeToken)
			require.Equal(t, resumeToken, open.lastVersion)
			end := c.end
			if end == nil {
				end = io.EOF
			}
			got, _ := r.consumeStream(context.Background(), &fakeWatchStream{
				events: append([]ev{marker(resumeToken)}, c.live...), err: end,
			}, resumeToken, open)
			assert.Equal(t, c.want, got)
			drops, _ := metricValue(t, reader, "aether.agent.registry.watch_token_drops")
			if c.want == "" {
				assert.Equal(t, int64(1), drops, "the drop is counted")
			} else {
				assert.Zero(t, drops, "a kept token is not counted")
			}

			// What the next stream presents follows from the token alone.
			next := r.resumeFor(serviceSet(ab), got)
			if c.want == "" {
				assert.True(t, next.noToken, "a dropped token resends")
				assert.Nil(t, next.partial, "a dropped token is never offered as a partial resume either")
				r.mu.RLock()
				assert.Empty(t, r.held, "nothing is held once the token is dropped")
				r.mu.RUnlock()
			} else {
				assert.Equal(t, c.want, next.lastVersion)
			}
		})
	}
}

// TestConsumeStream_MidBatchDropDoesNotLeak: the flag is per stream. A stream
// cut mid-batch drops the token; the resend that follows is a stream of its
// own, and once it completes its marker's version stands, as does a later
// batch's.
func TestConsumeStream_MidBatchDropDoesNotLeak(t *testing.T) {
	const v2 = "8.0123456789abcdef"
	const v3 = "9.0123456789abcdef"
	ab := []string{"default/a", "default/b"}
	r := heldAfterStart(t, ab...)

	got, err := r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: resumeToken},
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED, "default/a", "10.0.0.1", ""),
	}, err: io.EOF}, resumeToken, r.resumeFor(serviceSet(ab), resumeToken))
	require.NoError(t, err)
	require.Empty(t, got)

	open := r.resumeFor(serviceSet(ab), got)
	got, err = r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/a", "10.0.0.1", ""),
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/b", "10.0.0.2", ""),
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v2},
	}, err: io.EOF}, got, open)
	require.NoError(t, err)
	assert.Equal(t, v2, got, "the resend's own stream ends settled")
	assert.Equal(t, v2, r.resumeFor(serviceSet(ab), got).lastVersion, "and its services are held again")

	got, err = r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v2},
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/a", "10.0.0.3", ""),
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/b", "10.0.0.4", v3),
	}, err: io.EOF}, got, r.resumeFor(serviceSet(ab), got))
	require.NoError(t, err)
	assert.Equal(t, v3, got)
}

// TestConsumeStream_ExtensionPrefixVersusLiveBatch: an extension's own events
// are unversioned too, but they are for services outside held (resumeFor drops
// them again after a cut), so a cut before its marker keeps the token
// (TestConsumeStream_CutExtendedStartKeepsTheToken). A live batch's prefix after
// the marker does not.
func TestConsumeStream_ExtensionPrefixVersusLiveBatch(t *testing.T) {
	const v2 = "8.0123456789abcdef"
	r := heldAfterStart(t, "default/a")
	ab := []string{"default/a", "default/b"}
	r.SetServiceFilter(ab)
	open := r.resumeFor(serviceSet(ab), resumeToken)
	require.NotNil(t, open.partial)
	done := &registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v2, Extended: true}

	got, err := r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/b", "10.0.1.1", ""),
		done,
		epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/a", "10.0.0.9", ""),
	}, err: io.EOF}, resumeToken, open)
	require.NoError(t, err)
	assert.Empty(t, got, "the live prefix after the extension's marker drops the token")
}
