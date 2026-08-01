package cachemetrics

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := New(provider.Meter("test"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m, reader
}

func metricValue(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				var total int64
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
				return total, true
			case metricdata.Gauge[int64]:
				if len(data.DataPoints) == 0 {
					return 0, false
				}
				return data.DataPoints[len(data.DataPoints)-1].Value, true
			default:
				t.Fatalf("metric %s is %T, want Sum[int64] or Gauge[int64]", name, m.Data)
			}
		}
	}
	return 0, false
}

func TestCacheMetrics_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	m.Generated(context.Background(), 0.01, 1, nil)
	m.Generated(context.Background(), 0.01, 1, errors.New("boom"))
}

func TestCacheMetrics_GeneratedSuccess(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.Generated(ctx, 0.01, 5, nil)
	m.Generated(ctx, 0.02, 6, nil)

	if got, _ := metricValue(t, reader, "aether.agent.snapshot.builds"); got != 2 {
		t.Errorf("builds = %d, want 2", got)
	}
	if got, _ := metricValue(t, reader, "aether.agent.snapshot.version"); got != 6 {
		t.Errorf("version = %d, want 6", got)
	}
	if _, found := metricValue(t, reader, "aether.agent.snapshot.errors"); found {
		t.Error("errors recorded on success")
	}
}

func TestCacheMetrics_GeneratedFailure(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Generated(context.Background(), 0.01, 5, errors.New("snapshot rejected"))

	if got, _ := metricValue(t, reader, "aether.agent.snapshot.errors"); got != 1 {
		t.Errorf("errors = %d, want 1", got)
	}
	if _, found := metricValue(t, reader, "aether.agent.snapshot.builds"); found {
		t.Error("builds recorded on failure")
	}
	// The version gauge must not advance on failure: Envoy is still on the
	// previous snapshot.
	if _, found := metricValue(t, reader, "aether.agent.snapshot.version"); found {
		t.Error("version recorded on failure")
	}
}
