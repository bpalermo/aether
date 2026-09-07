package registrar

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newIdentityGatedRegistry wires a registry whose watch stream cannot connect
// (nothing is listening) and whose identity readiness the test controls.
func newIdentityGatedRegistry(t *testing.T, ready func() bool) (*RegistrarRegistry, *lockedBuffer, *sdkmetric.ManualReader) {
	t.Helper()

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := NewRegistrarRegistry(log, Config{
		Address:     "passthrough:///registrar-test",
		ClusterName: "test-cluster",
		NodeName:    "test-node",
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer((&swappableListener{}).dial), // nobody is listening
		},
		IdentityReady: ready,
	})
	metrics, reader := newTestClientMetrics(t)
	r.metrics = metrics
	return r, logs, reader
}

// TestWatchLoop_DefersWhileIdentityNotReady covers the agent's new startup
// shape (#740): the process comes up before SPIRE has issued its SVID, so the
// watch stream cannot handshake yet. That is a deferral, not a failure — INFO,
// no watch_errors, and no escalating backoff — because the ERROR path here
// exists to report a registrar that will not serve the stream (#700, #712), and
// burying that under a boot's worth of identity noise is exactly what #712
// removed.
func TestWatchLoop_DefersWhileIdentityNotReady(t *testing.T) {
	var ready atomic.Bool // starts false: no SVID yet
	r, logs, reader := newIdentityGatedRegistry(t, ready.Load)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	require.Eventually(t, func() bool {
		return findRecord(logRecords(t, logs), "watch stream deferred until this agent has an SVID") != nil
	}, 30*time.Second, 10*time.Millisecond, "the pre-identity wait must be announced; logs:\n%s", logs.String())

	deferred := findRecord(logRecords(t, logs), "watch stream deferred until this agent has an SVID")
	require.Equal(t, "INFO", deferred["level"])
	require.Contains(t, deferred, "backoff", "the deferral still waits before retrying")

	for _, rec := range logRecords(t, logs) {
		require.NotEqual(t, "ERROR", rec["level"],
			"waiting for our own identity must not be reported as a registrar failure; logs:\n%s", logs.String())
	}
	_, found := metricValue(t, reader, "aether.agent.registry.watch_errors")
	require.False(t, found, "a deferral must not count as a watch error")

	// The negative control: once the SVID exists, a stream that still will not
	// open is a real failure again, with the ERROR and the counter back.
	ready.Store(true)

	require.Eventually(t, func() bool {
		return findRecord(logRecords(t, logs), "failed to start watch stream, retrying") != nil
	}, 30*time.Second, 10*time.Millisecond, "with an identity in hand the failure path must be restored; logs:\n%s", logs.String())

	failed := findRecord(logRecords(t, logs), "failed to start watch stream, retrying")
	require.Equal(t, "ERROR", failed["level"])

	require.Eventually(t, func() bool {
		got, ok := metricValue(t, reader, "aether.agent.registry.watch_errors")
		return ok && got >= 1
	}, 10*time.Second, 10*time.Millisecond, "a real stream failure must still count")
}
