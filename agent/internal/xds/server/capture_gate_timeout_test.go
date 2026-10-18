package server

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// captureGateTimeoutsByInput reads the capture-gate timeout counter by its
// "input" attribute.
func captureGateTimeoutsByInput(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != CaptureGateTimeoutsMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value("input")
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestPreListen_CaptureGateTimeoutNamesTheMissingInput (proposal 044): when
// the capture gate's bound passes, the warning and the counter name the input
// that had not arrived: here the Service range, the projection having landed.
func TestPreListen_CaptureGateTimeoutNamesTheMissingInput(t *testing.T) {
	srv, registryCalls := newHoldServer(t, nil)
	logs := &bytes.Buffer{}
	srv.log = slog.New(slog.NewTextHandler(logs, nil))
	reader := sdkmetric.NewManualReader()
	counter, err := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test").Int64Counter(CaptureGateTimeoutsMetric)
	require.NoError(t, err)
	srv.captureGateTimeouts = counter
	srv.SetCaptureGate(srv.cache.CaptureInputsReady())
	srv.SetCaptureGateInputs(srv.cache.MissingCaptureInputs)
	srv.captureTimeout = 100 * time.Millisecond

	srv.cache.SetCaptureTCPServices(nil)
	require.NoError(t, srv.PreListen(t.Context()))
	assert.Positive(t, registryCalls.Load(), "the bound passed: the serve goes ahead")

	assert.Equal(t, map[string]int64{cache.CaptureInputServiceRange: 1}, captureGateTimeoutsByInput(t, reader))
	assert.Contains(t, logs.String(), "missing=["+cache.CaptureInputServiceRange+"]")
	assert.NotContains(t, logs.String(), cache.CaptureInputProjection, "the projection arrived: it is not named")
}

// TestPreListen_CaptureGateTimeoutWithoutInputs: a gate set without
// SetCaptureGateInputs is the projection alone, and a timeout names that.
func TestPreListen_CaptureGateTimeoutWithoutInputs(t *testing.T) {
	srv, _ := newHoldServer(t, nil)
	reader := sdkmetric.NewManualReader()
	counter, err := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test").Int64Counter(CaptureGateTimeoutsMetric)
	require.NoError(t, err)
	srv.captureGateTimeouts = counter
	srv.SetCaptureGate(make(chan struct{}))
	srv.captureTimeout = 50 * time.Millisecond
	require.NoError(t, srv.PreListen(t.Context()))
	assert.Equal(t, map[string]int64{cache.CaptureInputProjection: 1}, captureGateTimeoutsByInput(t, reader))
}
