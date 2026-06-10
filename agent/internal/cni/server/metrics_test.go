package server

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestCNIMetrics(t *testing.T) (*cniMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newCNIMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatalf("newCNIMetrics() error = %v", err)
	}
	return m, reader
}

func metricSum(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	var total int64
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			found = true
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					total = dp.Value
				}
			default:
				t.Fatalf("metric %s is %T, want Sum[int64] or Gauge[int64]", name, m.Data)
			}
		}
	}
	return total, found
}

func TestCNIMetrics_NilReceiverSafe(t *testing.T) {
	var m *cniMetrics
	ctx := context.Background()
	m.sweepCompleted(ctx, 1, 1, 1, nil)
	m.sweepCompleted(ctx, 0, 0, 0, errors.New("boom"))
	m.healthTransition(ctx, "HEALTH_UNHEALTHY", "HEALTH_HEALTHY")
}

func TestCNIMetrics_SweepCompleted(t *testing.T) {
	m, reader := newTestCNIMetrics(t)
	ctx := context.Background()

	m.sweepCompleted(ctx, 2, 1, 7, nil)

	if got, _ := metricSum(t, reader, "aether.agent.ghost_sweep.ghosts_removed"); got != 2 {
		t.Errorf("ghosts_removed = %d, want 2", got)
	}
	if got, _ := metricSum(t, reader, "aether.agent.ghost_sweep.missing_registered"); got != 1 {
		t.Errorf("missing_registered = %d, want 1", got)
	}
	if got, _ := metricSum(t, reader, "aether.agent.storage.pods"); got != 7 {
		t.Errorf("storage.pods = %d, want 7", got)
	}
}

func TestCNIMetrics_SweepFailed(t *testing.T) {
	m, reader := newTestCNIMetrics(t)

	m.sweepCompleted(context.Background(), 0, 0, 0, errors.New("registry down"))

	if got, _ := metricSum(t, reader, "aether.agent.ghost_sweep.errors"); got != 1 {
		t.Errorf("sweep errors = %d, want 1", got)
	}
	// A failed sweep reconciled nothing: correction counters must stay at zero
	// and the stale pod gauge must not be (mis)recorded as 0.
	if _, found := metricSum(t, reader, "aether.agent.ghost_sweep.ghosts_removed"); found {
		t.Error("ghosts_removed recorded on failed sweep")
	}
	if _, found := metricSum(t, reader, "aether.agent.storage.pods"); found {
		t.Error("storage.pods recorded on failed sweep")
	}
}

func TestCNIMetrics_HealthTransition(t *testing.T) {
	m, reader := newTestCNIMetrics(t)
	ctx := context.Background()

	m.healthTransition(ctx, "HEALTH_UNHEALTHY", "HEALTH_HEALTHY")
	m.healthTransition(ctx, "HEALTH_HEALTHY", "HEALTH_UNHEALTHY")

	if got, _ := metricSum(t, reader, "aether.agent.liveness.health_transitions"); got != 2 {
		t.Errorf("health_transitions = %d, want 2", got)
	}
}
