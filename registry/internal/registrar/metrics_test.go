package registrar

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestClientMetrics(t *testing.T) (*clientMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newClientMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatalf("newClientMetrics() error = %v", err)
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

func TestClientMetrics_NilReceiverSafe(t *testing.T) {
	var m *clientMetrics
	ctx := context.Background()
	m.streamReconnected(ctx)
	m.streamFailed(ctx)
	m.versionApplied(ctx, "3")
}

func TestClientMetrics_Recording(t *testing.T) {
	m, reader := newTestClientMetrics(t)
	ctx := context.Background()

	m.streamReconnected(ctx)
	m.streamReconnected(ctx)
	m.streamFailed(ctx)
	m.versionApplied(ctx, "12")
	m.versionApplied(ctx, "not-a-number") // ignored

	if got, _ := metricValue(t, reader, "aether.agent.registry.reconnects"); got != 2 {
		t.Errorf("reconnects = %d, want 2", got)
	}
	if got, _ := metricValue(t, reader, "aether.agent.registry.watch_errors"); got != 1 {
		t.Errorf("watch_errors = %d, want 1", got)
	}
	if got, _ := metricValue(t, reader, "aether.agent.registry.last_version"); got != 12 {
		t.Errorf("last_version = %d, want 12", got)
	}
}
