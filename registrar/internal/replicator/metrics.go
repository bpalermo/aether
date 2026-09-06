package replicator

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// metrics holds the replicator OTel instruments (proposal 006 P2c). All
// methods are nil-receiver-safe so the replicator runs unchanged when
// telemetry is disabled. Every measurement carries a peer.region attribute
// (bounded: one value per configured peer).
type metrics struct {
	connected metric.Int64Gauge
	events    metric.Int64Counter
	errors    metric.Int64Counter
	resyncs   metric.Int64Counter
	syncSecs  metric.Float64Histogram
}

// syncDurationBuckets are the explicit boundaries, in SECONDS, for
// aether.replicator.sync.duration. The OTel defaults are millisecond-oriented
// (0, 5, 10, ... 10000): against a seconds-valued duration every range-sync would land
// in the "<= 5 s" first bucket and the quantiles would read flat (#732).
var syncDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// newMetrics builds the instruments on the global MeterProvider (a no-op
// meter until a provider is installed). Returns nil on registration error
// (methods are nil-safe).
func newMetrics() *metrics {
	meter := otel.Meter("aether/replicator")
	m := &metrics{}
	var err error
	if m.connected, err = meter.Int64Gauge("aether.replicator.peer.connected",
		metric.WithDescription("1 while the peer mirror stream is live (lease held + synced + watching), 0 while disconnected/redialing (proposal 006)")); err != nil {
		return nil
	}
	if m.events, err = meter.Int64Counter("aether.replicator.events",
		metric.WithDescription("Watch events mirrored into the peer (op = put|delete)")); err != nil {
		return nil
	}
	if m.errors, err = meter.Int64Counter("aether.replicator.errors",
		metric.WithDescription("Mirror stream failures (dial, lease grant/keepalive loss, sync, watch, or peer write) that force a redial + resync")); err != nil {
		return nil
	}
	if m.resyncs, err = meter.Int64Counter("aether.replicator.resyncs",
		metric.WithDescription("Full range-syncs against the peer (initial, post-redial, and compaction-forced)")); err != nil {
		return nil
	}
	if m.syncSecs, err = meter.Float64Histogram("aether.replicator.sync.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Full range-sync duration"),
		metric.WithExplicitBucketBoundaries(syncDurationBuckets...)); err != nil {
		return nil
	}
	return m
}

func peerAttrs(region string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("peer.region", region))
}

func (m *metrics) setConnected(ctx context.Context, region string, up bool) {
	if m == nil {
		return
	}
	v := int64(0)
	if up {
		v = 1
	}
	m.connected.Record(ctx, v, peerAttrs(region))
}

func (m *metrics) event(ctx context.Context, region, op string) {
	if m == nil {
		return
	}
	m.events.Add(ctx, 1, peerAttrs(region), metric.WithAttributes(attribute.String("op", op)))
}

func (m *metrics) mirrorError(ctx context.Context, region string) {
	if m == nil {
		return
	}
	m.errors.Add(ctx, 1, peerAttrs(region))
}

func (m *metrics) resync(ctx context.Context, region string, took time.Duration) {
	if m == nil {
		return
	}
	m.resyncs.Add(ctx, 1, peerAttrs(region))
	m.syncSecs.Record(ctx, took.Seconds(), peerAttrs(region))
}
