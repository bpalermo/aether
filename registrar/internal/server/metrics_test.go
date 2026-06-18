package server

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectSum returns the summed int64 counter value for the named metric, or 0
// if the metric was never recorded.
func collectSum(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s is %T, want Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// collectGauge returns the last recorded int64 gauge value for the named
// metric. Fails the test if the metric was never recorded.
func collectGauge(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
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
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %s is %T, want Gauge[int64]", name, m.Data)
			}
			if len(gauge.DataPoints) == 0 {
				t.Fatalf("metric %s has no data points", name)
			}
			return gauge.DataPoints[len(gauge.DataPoints)-1].Value
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func newTestMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := NewMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}
	return m, reader
}

func TestMetrics_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	ctx := context.Background()
	m.watcherSubscribed(ctx)
	m.watcherUnsubscribed(ctx)
	m.eventBroadcast(ctx, "EVENT_TYPE_ENDPOINT_ADDED")
	m.eventDropped(ctx, "EVENT_TYPE_ENDPOINT_ADDED")
	m.syncCompleted(ctx, 0.1, 1, map[string]int{"EVENT_TYPE_ENDPOINT_ADDED": 1})
	m.syncFailed(ctx, 0.1)
	m.versionAdvanced(ctx, "7")
}

func TestBroadcaster_DropIncrementsCounter(t *testing.T) {
	m, reader := newTestMetrics(t)
	b := NewBroadcaster(slog.New(slog.DiscardHandler), m)

	ch := b.Subscribe("slow-watcher", nil)
	_ = ch // never drained: fills to capacity, then drops

	// Fill the buffer, then overflow by 3.
	overflow := 3
	events := make([]*registrarv1.WatchEndpointsResponse, 0, defaultChannelBuffer+overflow)
	for i := 0; i < defaultChannelBuffer+overflow; i++ {
		events = append(events, &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: fmt.Sprintf("svc-%d", i),
		})
	}
	b.Broadcast(events)

	if got := collectSum(t, reader, "aether.registrar.broadcast.dropped_events"); got != int64(overflow) {
		t.Errorf("dropped_events = %d, want %d", got, overflow)
	}
	if got := collectSum(t, reader, "aether.registrar.broadcast.events"); got != int64(defaultChannelBuffer) {
		t.Errorf("broadcast.events = %d, want %d", got, defaultChannelBuffer)
	}
}

func TestBroadcaster_WatcherCountMetric(t *testing.T) {
	m, reader := newTestMetrics(t)
	b := NewBroadcaster(slog.New(slog.DiscardHandler), m)

	ch1 := b.Subscribe("a", nil)
	b.Subscribe("b", nil)
	if got := collectSum(t, reader, "aether.registrar.watchers"); got != 2 {
		t.Errorf("watchers after subscribes = %d, want 2", got)
	}

	// Reconnect with the same ID: count must not grow.
	b.Subscribe("a", nil)
	if got := collectSum(t, reader, "aether.registrar.watchers"); got != 2 {
		t.Errorf("watchers after reconnect = %d, want 2", got)
	}

	// A stale unsubscribe (old channel) must not decrement.
	b.Unsubscribe("a", ch1)
	if got := collectSum(t, reader, "aether.registrar.watchers"); got != 2 {
		t.Errorf("watchers after stale unsubscribe = %d, want 2", got)
	}
}

func TestMetrics_SyncCompletedRecordsVersionAndEvents(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.syncCompleted(ctx, 0.05, 42, map[string]int{
		"EVENT_TYPE_ENDPOINT_ADDED":   2,
		"EVENT_TYPE_ENDPOINT_REMOVED": 1,
	})

	if got := collectGauge(t, reader, "aether.registrar.snapshot.version"); got != 42 {
		t.Errorf("snapshot.version = %d, want 42", got)
	}
	if got := collectSum(t, reader, "aether.registrar.sync.events"); got != 3 {
		t.Errorf("sync.events = %d, want 3", got)
	}
}

func TestMetrics_VersionAdvanced(t *testing.T) {
	m, reader := newTestMetrics(t)
	m.versionAdvanced(context.Background(), "17")
	if got := collectGauge(t, reader, "aether.registrar.snapshot.version"); got != 17 {
		t.Errorf("snapshot.version = %d, want 17", got)
	}
	// Unparseable versions are ignored, not recorded as garbage.
	m.versionAdvanced(context.Background(), "not-a-number")
	if got := collectGauge(t, reader, "aether.registrar.snapshot.version"); got != 17 {
		t.Errorf("snapshot.version after bad input = %d, want 17", got)
	}
}
