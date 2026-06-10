package cache

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-xds-cache"

// cacheMetrics holds the snapshot-generation instruments. All methods are
// nil-receiver-safe so the cache runs unchanged when telemetry is disabled.
//
// A snapshot build failure leaves Envoy on the previous version — stale
// config — so the errors counter is a direct staleness signal, and the
// version gauge shows whether snapshots keep advancing at all.
type cacheMetrics struct {
	builds   metric.Int64Counter
	errors   metric.Int64Counter
	duration metric.Float64Histogram
	version  metric.Int64Gauge
}

// newCacheMetrics registers the snapshot instruments on the given meter.
func newCacheMetrics(meter metric.Meter) (*cacheMetrics, error) {
	m := &cacheMetrics{}
	var err error

	if m.builds, err = meter.Int64Counter("aether.agent.snapshot.builds",
		metric.WithDescription("xDS snapshot generations set on the cache")); err != nil {
		return nil, fmt.Errorf("builds: %w", err)
	}
	if m.errors, err = meter.Int64Counter("aether.agent.snapshot.errors",
		metric.WithDescription("Failed xDS snapshot generations (Envoy left on the previous version)")); err != nil {
		return nil, fmt.Errorf("errors: %w", err)
	}
	if m.duration, err = meter.Float64Histogram("aether.agent.snapshot.duration",
		metric.WithDescription("Duration of an xDS snapshot generation"),
		metric.WithUnit("s")); err != nil {
		return nil, fmt.Errorf("duration: %w", err)
	}
	if m.version, err = meter.Int64Gauge("aether.agent.snapshot.version",
		metric.WithDescription("Counter component of the current xDS snapshot version")); err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}

	return m, nil
}

// generated records the outcome of one snapshot generation.
func (m *cacheMetrics) generated(ctx context.Context, seconds float64, version int64, err error) {
	if m == nil {
		return
	}
	m.duration.Record(ctx, seconds)
	if err != nil {
		m.errors.Add(ctx, 1)
		return
	}
	m.builds.Add(ctx, 1)
	m.version.Record(ctx, version)
}
