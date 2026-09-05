package registrar

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// lockedBuffer is a concurrency-safe sink for the slog handler: the watch loop
// writes from its own goroutine while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeWatchServer is an in-process RegistrarService that answers WatchEndpoints
// with an immediate SNAPSHOT_COMPLETE and then holds the stream open, recording
// every request it received so a test can assert which filter was asserted.
type fakeWatchServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	active atomic.Int64

	mu       sync.Mutex
	requests []*registrarv1.WatchEndpointsRequest
}

func (s *fakeWatchServer) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	s.active.Add(1)
	defer s.active.Add(-1)

	if err := stream.Send(&registrarv1.WatchEndpointsResponse{
		Type:    registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
		Version: "1",
	}); err != nil {
		return err
	}

	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *fakeWatchServer) lastRequest() *registrarv1.WatchEndpointsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

func (s *fakeWatchServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// startFakeRegistrar serves fakeWatchServer over a bufconn listener and returns
// a dial option set whose dialer blocks until gate is closed (nil gate = dial
// immediately). Gating the dial is what makes the startup race deterministic:
// opening a stream blocks in the gRPC picker until the connection is READY, so
// the first WatchEndpoints call is still in flight when the test asserts the
// service filter — exactly what the agent does on a clean start.
func startFakeRegistrar(t *testing.T, gate <-chan struct{}) (*fakeWatchServer, []grpc.DialOption) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := grpc.NewServer()
	fake := &fakeWatchServer{}
	registrarv1.RegisterRegistrarServiceServer(grpcSrv, fake)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return lis.DialContext(ctx)
	}

	return fake, []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	}
}

func newLoggingRegistry(t *testing.T, opts []grpc.DialOption) (*RegistrarRegistry, *lockedBuffer) {
	t.Helper()

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return NewRegistrarRegistry(log, Config{
		// passthrough so the custom dialer (bufconn) is used verbatim.
		Address:     "passthrough:///registrar-test",
		ClusterName: "test-cluster",
		NodeName:    "test-node",
		DialOptions: opts,
	}), logs
}

// TestWatchLoop_StartupFilterAssertIsNotAnError mirrors the agent's real startup
// sequence — Initialize opens the unfiltered watch, then the xDS PreListen
// asserts the node's demand-scoped filter while the first stream is still
// dialing — and asserts the client logs nothing at ERROR on that clean start
// (issue #700).
func TestWatchLoop_StartupFilterAssertIsNotAnError(t *testing.T) {
	gate := make(chan struct{})
	fake, dialOpts := startFakeRegistrar(t, gate)
	r, logs := newLoggingRegistry(t, dialOpts)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	// Wait until the loop published its per-stream cancel: the first
	// WatchEndpoints is now in flight against the still-gated connection.
	require.Eventually(t, func() bool {
		r.filterMu.Lock()
		defer r.filterMu.Unlock()
		return r.streamCancel != nil
	}, 5*time.Second, time.Millisecond)

	// The startup filter assert (agent/internal/xds/server/server.go PreListen).
	r.SetServiceFilter([]string{"default/svc-a"})

	// Let the dial complete; the loop must reconnect with the new filter.
	close(gate)

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	require.NoError(t, r.WaitReady(readyCtx))

	out := logs.String()
	require.NotContains(t, out, `"level":"ERROR"`, "clean start must not log at ERROR; got:\n%s", out)
	require.Contains(t, out, "watch stream superseded by a service-filter change",
		"the superseded first stream must be announced, not swallowed; got:\n%s", out)
	require.Contains(t, out, `"startup":true`,
		"the first stream's handoff must carry the startup marker; got:\n%s", out)

	// The reconnect asserted the new filter, and it happened without paying the
	// reconnect backoff (the backoff path logs at ERROR, asserted absent above).
	last := fake.lastRequest()
	require.NotNil(t, last)
	require.Equal(t, []string{"default/svc-a"}, last.GetFilter().GetServices())
	require.Equal(t, 1, fake.requestCount(), "the superseded stream never reached the server")
}

// TestWatchLoop_ShutdownIsNotAnError asserts that tearing the client down
// (Close cancels the watch context) leaves no ERROR behind either: the
// cancellation is the shutdown, not a stream failure.
func TestWatchLoop_ShutdownIsNotAnError(t *testing.T) {
	fake, dialOpts := startFakeRegistrar(t, nil)
	r, logs := newLoggingRegistry(t, dialOpts)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	require.NoError(t, r.WaitReady(readyCtx))

	require.NoError(t, r.Close())

	require.Eventually(t, func() bool { return fake.active.Load() == 0 }, 10*time.Second, 5*time.Millisecond)

	out := logs.String()
	require.NotContains(t, out, `"level":"ERROR"`, "shutdown must not log at ERROR; got:\n%s", out)
}
