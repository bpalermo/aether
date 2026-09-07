package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errRegistrarNoIdentity is the failure a cluster reload sees while the
// registrar replica it dials is still waiting for SPIRE (rev210 roll,
// 2026-09-07 20:03:45Z). Before PR 4 of #740 it produced
// `failed to refresh clusters from registry` at ERROR plus a refresh_errors
// increment, for a transient that self-heals in seconds and leaves the previous
// snapshot in place.
var errRegistrarNoIdentity = status.Error(codes.Unavailable,
	`connection error: desc = "transport: authentication handshake failed: x509svid: could not get X509 bundle"`)

// newCountingRefresher returns a refresher whose log is captured as JSON and
// whose refresh_errors counter is readable.
func newCountingRefresher(t *testing.T) (*RegistryRefresher, *bytes.Buffer, *sdkmetric.ManualReader) {
	t.Helper()

	logs := &bytes.Buffer{}
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	counter, err := meter.Int64Counter("aether.agent.xds.refresh_errors")
	require.NoError(t, err)

	return &RegistryRefresher{
		log:           slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		refreshErrors: counter,
	}, logs, reader
}

// refreshErrorCount reads the counter, reporting whether it was recorded at all.
func refreshErrorCount(t *testing.T, reader *sdkmetric.ManualReader) (int64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aether.agent.xds.refresh_errors" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) == 0 {
				continue
			}
			return sum.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// lastRecord returns the last JSON log record emitted.
func lastRecord(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.NotEmpty(t, lines[0], "expected a log line, got none")

	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &rec))
	return rec
}

// TestReportReloadFailure_PeerIdentityIsNotAnError is PR 4 of #740 on the
// cluster-refresh path: a registrar that has not got its SVID yet is the
// server's startup, so it must not raise the alarm that a stale cluster
// snapshot raises.
func TestReportReloadFailure_PeerIdentityIsNotAnError(t *testing.T) {
	r, logs, reader := newCountingRefresher(t)

	r.reportReloadFailure(context.Background(), errRegistrarNoIdentity)

	rec := lastRecord(t, logs)
	assert.Equal(t, "WARN", rec["level"])
	assert.Equal(t, "cluster refresh deferred: the registrar has no identity yet, keeping the current snapshot", rec["msg"])

	_, found := refreshErrorCount(t, reader)
	assert.False(t, found, "the registrar's own startup must not count as a refresh error")
}

// TestReportReloadFailure_OtherFailuresStayErrors is the negative control: the
// condition refresh_errors exists for — a reload that leaves the cluster
// snapshot stale until the next change signal — keeps ERROR and the counter.
func TestReportReloadFailure_OtherFailuresStayErrors(t *testing.T) {
	r, logs, reader := newCountingRefresher(t)

	r.reportReloadFailure(context.Background(), errors.New("registry: connection refused"))

	rec := lastRecord(t, logs)
	assert.Equal(t, "ERROR", rec["level"])
	assert.Equal(t, "failed to refresh clusters from registry", rec["msg"])

	got, found := refreshErrorCount(t, reader)
	require.True(t, found, "a real reload failure must still count")
	assert.Equal(t, int64(1), got)
}

// TestReportReloadFailure_NilCounter keeps the instrumentation optional: with
// --otel-enabled=false the counter is nil and the report must still log.
func TestReportReloadFailure_NilCounter(t *testing.T) {
	logs := &bytes.Buffer{}
	r := &RegistryRefresher{log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	r.reportReloadFailure(context.Background(), errors.New("boom"))

	assert.Equal(t, "ERROR", lastRecord(t, logs)["level"])
}

// TestReportReloadFailure_ClassifiesInOrder is PR 5 of #740: the same error text
// arrives for three different conditions, and only local state tells them apart.
// On the rev211 deploy roll (2026-09-07) main-worker-02 logged the registrar as
// the culprit twice within 600ms — once while the AGENT had no SVID, once while
// its own connection was still re-establishing itself after acquiring one. The
// registrar had had its identity for a minute.
func TestReportReloadFailure_ClassifiesInOrder(t *testing.T) {
	t.Run("our own identity pending outranks the peer", func(t *testing.T) {
		r, logs, reader := newCountingRefresher(t)
		r.SetIdentityGate(newFakeIdentity()) // no SVID

		r.reportReloadFailure(context.Background(), errRegistrarNoIdentity)

		rec := lastRecord(t, logs)
		assert.Equal(t, "INFO", rec["level"])
		assert.Equal(t, "cluster refresh deferred until this agent has an SVID, keeping the current snapshot", rec["msg"])

		_, found := refreshErrorCount(t, reader)
		assert.False(t, found, "our own missing identity is not a refresh error")
	})

	t.Run("the reconnect window outranks the peer", func(t *testing.T) {
		r, logs, reader := newCountingRefresher(t)
		gate := newFakeIdentity()
		gate.arrive()
		r.SetIdentityGate(gate)
		r.watchIdentity(t.Context()) // identity is in hand as of now

		r.reportReloadFailure(context.Background(), errRegistrarNoIdentity)

		rec := lastRecord(t, logs)
		assert.Equal(t, "INFO", rec["level"])
		assert.Equal(t, "registrar connection not yet re-established after identity; retrying", rec["msg"])

		_, found := refreshErrorCount(t, reader)
		assert.False(t, found, "our own reconnect is not a refresh error")
	})

	t.Run("past the window it is the peer again", func(t *testing.T) {
		r, logs, _ := newCountingRefresher(t)
		gate := newFakeIdentity()
		gate.arrive()
		r.SetIdentityGate(gate)
		r.identityAt.Store(time.Now().Add(-time.Hour).UnixNano())

		r.reportReloadFailure(context.Background(), errRegistrarNoIdentity)

		rec := lastRecord(t, logs)
		assert.Equal(t, "WARN", rec["level"])
		assert.Equal(t, "cluster refresh deferred: the registrar has no identity yet, keeping the current snapshot", rec["msg"])
	})

	t.Run("a settled identity keeps real failures loud", func(t *testing.T) {
		r, logs, reader := newCountingRefresher(t)
		gate := newFakeIdentity()
		gate.arrive()
		r.SetIdentityGate(gate)
		r.identityAt.Store(time.Now().Add(-time.Hour).UnixNano())

		r.reportReloadFailure(context.Background(), errors.New("registry: malformed response"))

		assert.Equal(t, "ERROR", lastRecord(t, logs)["level"])
		got, found := refreshErrorCount(t, reader)
		require.True(t, found)
		assert.Equal(t, int64(1), got)
	})
}
