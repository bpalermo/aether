package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// catalogWatchServer answers WatchEndpoints with a one-service catalog and its
// SNAPSHOT_COMPLETE marker, then holds the stream open — the shape of a live
// registrar watch. Two of these stand in for the two registrar replicas a roll
// hands the agent between.
type catalogWatchServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	service string
	version string

	mu       sync.Mutex
	requests int
}

func (s *catalogWatchServer) WatchEndpoints(_ *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	if err := stream.Send(&registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED,
		ServiceName: s.service,
		Version:     s.version,
	}); err != nil {
		return err
	}
	if err := stream.Send(&registrarv1.WatchEndpointsResponse{
		Type:    registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
		Version: s.version,
	}); err != nil {
		return err
	}

	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *catalogWatchServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// swappableListener is the dial target the client keeps re-dialing: a test
// swaps in the surviving replica's listener before draining the current one,
// exactly as a Deployment roll leaves a second registrar Pod behind the
// Service VIP.
type swappableListener struct {
	mu  sync.Mutex
	lis *bufconn.Listener
}

func (s *swappableListener) set(lis *bufconn.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lis = lis
}

func (s *swappableListener) dial(ctx context.Context, _ string) (net.Conn, error) {
	s.mu.Lock()
	lis := s.lis
	s.mu.Unlock()
	if lis == nil {
		return nil, errors.New("no registrar listening")
	}
	return lis.DialContext(ctx)
}

// serveCatalog stands a catalogWatchServer up on a fresh bufconn listener and
// returns both, plus the gRPC server so the test can drain or kill it.
func serveCatalog(t *testing.T, service, version string) (*grpc.Server, *bufconn.Listener, *catalogWatchServer) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	fake := &catalogWatchServer{service: service, version: version}
	registrarv1.RegisterRegistrarServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return srv, lis, fake
}

// newSwappableRegistry wires a logging registry (and a ManualReader-backed
// metric set) onto the swappable dial target.
func newSwappableRegistry(t *testing.T, target *swappableListener) (*RegistrarRegistry, *lockedBuffer, *sdkmetric.ManualReader) {
	t.Helper()

	r, logs := newLoggingRegistry(t, []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(target.dial),
	})
	metrics, reader := newTestClientMetrics(t)
	r.metrics = metrics
	return r, logs, reader
}

// logRecords parses the JSON slog output into records.
func logRecords(t *testing.T, logs *lockedBuffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		out = append(out, rec)
	}
	return out
}

// findRecord returns the first record whose msg matches.
func findRecord(records []map[string]any, msg string) map[string]any {
	for _, rec := range records {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

// TestWatchLoop_ServerDrainGoawayIsNotAnError reproduces a registrar Deployment
// roll: the replica the agent is streaming from drains (GracefulStop sends a
// graceful GOAWAY, then the Pod goes away with the stream still open) while a
// second replica is already serving. The client must treat that as a handoff —
// INFO with server_drain=true, no ERROR, no watch_errors, no backoff — and pick
// the next snapshot up from the survivor (issue #718).
func TestWatchLoop_ServerDrainGoawayIsNotAnError(t *testing.T) {
	target := &swappableListener{}
	srv1, lis1, fake1 := serveCatalog(t, "default/svc-before-roll", "1")
	target.set(lis1)

	r, logs, reader := newSwappableRegistry(t, target)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	require.NoError(t, r.WaitReady(readyCtx))
	require.True(t, r.HasService("default/svc-before-roll"))

	// The surviving replica is up behind the Service VIP before the drained one
	// finishes going away.
	_, lis2, fake2 := serveCatalog(t, "default/svc-after-roll", "2")
	target.set(lis2)

	// Drain replica 1: GracefulStop writes the GOAWAY (NO_ERROR) immediately and
	// then blocks on the still-open watch handler; Stop tears the transport down
	// with the stream open, which is what turns the GOAWAY into the client-side
	// Unavailable status the classifier reads.
	graceDone := make(chan struct{})
	go func() {
		defer close(graceDone)
		srv1.GracefulStop()
	}()
	time.Sleep(100 * time.Millisecond)
	srv1.Stop()
	<-graceDone

	// The client reconnects to the survivor and applies its snapshot.
	require.Eventually(t, func() bool {
		return fake2.requestCount() > 0 && r.HasService("default/svc-after-roll")
	}, 30*time.Second, 10*time.Millisecond, "client must resume against the surviving replica; logs:\n%s", logs.String())
	require.Equal(t, 1, fake1.requestCount())

	records := logRecords(t, logs)
	for _, rec := range records {
		require.NotEqual(t, "ERROR", rec["level"], "a registrar drain must not log at ERROR; got:\n%s", logs.String())
	}

	drain := findRecord(records, "watch stream closed by server drain; reconnecting")
	require.NotNil(t, drain, "the drain handoff must be announced at INFO; got:\n%s", logs.String())
	require.Equal(t, "INFO", drain["level"])
	require.Equal(t, true, drain["server_drain"])

	// Pin the classifier against real grpc-go output rather than a hand-written
	// string: this is the status message the agent saw on talos-main (#718).
	gotErr, ok := drain["error"].(string)
	require.True(t, ok, "the INFO line must carry the same error attr the ERROR line did; got %v", drain)
	require.Contains(t, gotErr, "rpc error: code = Unavailable")
	require.Contains(t, gotErr, "received prior goaway: code: NO_ERROR")

	// No failure was counted: watch_errors must not exist at all.
	_, found := metricValue(t, reader, "aether.agent.registry.watch_errors")
	require.False(t, found, "a server drain must not count as a watch error")
}

// TestWatchLoop_NonGoawayDisconnectStillErrors is the negative half: a replica
// that dies without a graceful GOAWAY is still a failure — ERROR, watch_errors,
// and the reconnect backoff.
func TestWatchLoop_NonGoawayDisconnectStillErrors(t *testing.T) {
	target := &swappableListener{}
	srv1, lis1, _ := serveCatalog(t, "default/svc-before-kill", "1")
	target.set(lis1)

	r, logs, reader := newSwappableRegistry(t, target)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	require.NoError(t, r.WaitReady(readyCtx))

	// Killed, not drained: Stop closes the transport without a GOAWAY.
	srv1.Stop()

	require.Eventually(t, func() bool {
		return findRecord(logRecords(t, logs), "watch stream disconnected, retrying") != nil
	}, 30*time.Second, 10*time.Millisecond, "an ungraceful disconnect must still log at ERROR; logs:\n%s", logs.String())

	disconnect := findRecord(logRecords(t, logs), "watch stream disconnected, retrying")
	require.Equal(t, "ERROR", disconnect["level"])
	require.NotContains(t, disconnect["error"], "received prior goaway: code: NO_ERROR",
		"this case must not carry a graceful GOAWAY, or the test proves nothing")
	require.Contains(t, disconnect, "backoff", "the failure path must back off before reconnecting")

	require.Eventually(t, func() bool {
		got, found := metricValue(t, reader, "aether.agent.registry.watch_errors")
		return found && got >= 1
	}, 10*time.Second, 10*time.Millisecond, "an ungraceful disconnect must count a watch error")
}
