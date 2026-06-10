package hotrestart

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestSupervisorMetrics(t *testing.T) (*SupervisorMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := NewSupervisorMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatalf("NewSupervisorMetrics() error = %v", err)
	}
	return m, reader
}

// metricValue returns the summed counter value or last gauge value for the
// named metric, and whether it was found.
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
			case metricdata.Histogram[float64]:
				var count int64
				for _, dp := range data.DataPoints {
					count += int64(dp.Count)
				}
				return count, true
			default:
				t.Fatalf("metric %s has unexpected data type %T", name, m.Data)
			}
		}
	}
	return 0, false
}

func TestSupervisorMetrics_NilReceiverSafe(t *testing.T) {
	var m *SupervisorMetrics
	m.epochStarted(1)
	m.handoffCompleted(2.5)
	m.wedged(wedgeHandoffTimeout)
	m.restartTriggered("sighup")
	m.childExited(exitDrained)
	m.predecessorFound(true)
	m.drainCompleted(1.0)
	m.readyTransition(true)
}

func TestSupervisorMetrics_Recording(t *testing.T) {
	m, reader := newTestSupervisorMetrics(t)

	m.epochStarted(0)
	m.epochStarted(1)
	m.handoffCompleted(3.2)
	m.wedged(wedgeAdminUnresponsive)
	m.restartTriggered("config_change")
	m.restartTriggered("sighup")
	m.childExited(exitDrained)
	m.predecessorFound(true)
	m.drainCompleted(12.0)
	m.readyTransition(true)
	m.readyTransition(false)

	want := map[string]int64{
		"aether.supervisor.epoch":                1, // gauge: last value
		"aether.supervisor.handoff.duration":     1, // histogram: sample count
		"aether.supervisor.wedges":               1,
		"aether.supervisor.restart_triggers":     2,
		"aether.supervisor.child_exits":          1,
		"aether.supervisor.predecessor_detected": 1,
		"aether.supervisor.drain.duration":       1,
		"aether.supervisor.ready_transitions":    2,
	}
	for name, wantV := range want {
		got, found := metricValue(t, reader, name)
		if !found {
			t.Errorf("metric %s not recorded", name)
			continue
		}
		if got != wantV {
			t.Errorf("%s = %d, want %d", name, got, wantV)
		}
	}
}

// TestSupervisor_HotRestartRecordsEpoch verifies the epoch gauge tracks the
// newest launched epoch through the real hotRestart path.
func TestSupervisor_HotRestartRecordsEpoch(t *testing.T) {
	m, reader := newTestSupervisorMetrics(t)
	s := New(Config{
		EnvoyPath:  "/bin/true", // exits immediately; only the fork matters here
		ConfigPath: "/dev/null",
	}, logr.Discard(), m)

	if err := s.hotRestart(); err != nil {
		t.Fatalf("hotRestart() error = %v", err)
	}
	defer s.shutdown()

	if got, _ := metricValue(t, reader, "aether.supervisor.epoch"); got != 0 {
		t.Errorf("epoch after first start = %d, want 0", got)
	}

	if err := s.hotRestart(); err != nil {
		t.Fatalf("hotRestart() error = %v", err)
	}
	if got, _ := metricValue(t, reader, "aether.supervisor.epoch"); got != 1 {
		t.Errorf("epoch after hot restart = %d, want 1", got)
	}
}

func TestTelemetry_MeterAndShutdown(t *testing.T) {
	tel, err := NewTelemetry(context.Background(), TelemetryConfig{
		ServiceVersion: "test",
		// no BindAddress: Serve is a no-op; no OTLP: Prometheus reader only
	}, logr.Discard())
	if err != nil {
		t.Fatalf("NewTelemetry() error = %v", err)
	}

	if _, err := NewSupervisorMetrics(tel.Meter()); err != nil {
		t.Fatalf("NewSupervisorMetrics() on telemetry meter error = %v", err)
	}
	if err := tel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}
