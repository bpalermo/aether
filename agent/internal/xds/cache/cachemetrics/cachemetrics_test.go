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
	m.UpstreamTTLRefreshed(context.Background(), 3)
	m.UpstreamsRestored(context.Background(), 3)
}

// TestCacheMetrics_UpstreamsRestored verifies the restore counter records the
// restored count and nothing on a cold start.
func TestCacheMetrics_UpstreamsRestored(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.UpstreamsRestored(ctx, 0)
	if _, found := metricValue(t, reader, "aether.agent.upstreams.restored"); found {
		t.Error("a cold start must record nothing")
	}

	m.UpstreamsRestored(ctx, 2)
	if got, _ := metricValue(t, reader, "aether.agent.upstreams.restored"); got != 2 {
		t.Errorf("restored = %d, want 2", got)
	}
}

// TestCacheMetrics_UpstreamTTLRefreshed verifies the in-use exemption counter
// sums across prune passes and records nothing when no entry was exempted.
func TestCacheMetrics_UpstreamTTLRefreshed(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.UpstreamTTLRefreshed(ctx, 0)
	if _, found := metricValue(t, reader, "aether.agent.upstreams.ttl_refreshed"); found {
		t.Error("a prune pass that exempted nothing must record nothing")
	}

	m.UpstreamTTLRefreshed(ctx, 2)
	m.UpstreamTTLRefreshed(ctx, 3)
	if got, _ := metricValue(t, reader, "aether.agent.upstreams.ttl_refreshed"); got != 5 {
		t.Errorf("ttl_refreshed = %d, want 5", got)
	}
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

// The #638 discriminator counters must exist (at zero) before anything is ever
// counted; otherwise their absence in Prometheus reads as a false zero.
func TestCacheMetrics_IdentityCountersSeededAtZero(t *testing.T) {
	_, reader := newTestMetrics(t)
	for _, name := range []string{
		"aether.agent.identity.outbound_binding_mismatch",
		"aether.agent.identity.inbound_binding_mismatch",
	} {
		v, ok := metricValue(t, reader, name)
		if !ok {
			t.Fatalf("%s not exported before first increment", name)
		}
		if v != 0 {
			t.Fatalf("%s = %d, want 0", name, v)
		}
	}
}
