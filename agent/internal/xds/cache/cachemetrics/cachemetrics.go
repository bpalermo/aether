// Package cachemetrics holds the agent xDS snapshot-generation instruments.
//
// The Metrics type is a thin wrapper over OpenTelemetry instruments recording
// snapshot build outcomes, durations, versions, and demand-scoping shape
// (cluster/upstream counts). All methods are nil-receiver-safe so the cache
// runs unchanged when telemetry is disabled.
package cachemetrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// MeterName identifies this instrumentation scope in metric backends.
const MeterName = "aether/agent-xds-cache"

// Metrics holds the snapshot-generation instruments. All methods are
// nil-receiver-safe so the cache runs unchanged when telemetry is disabled.
//
// A snapshot build failure leaves Envoy on the previous version — stale
// config — so the errors counter is a direct staleness signal, and the
// version gauge shows whether snapshots keep advancing at all.
type Metrics struct {
	builds   metric.Int64Counter
	errors   metric.Int64Counter
	duration metric.Float64Histogram
	version  metric.Int64Gauge
	// clusters is the headline demand-scoping number: how many clusters this
	// node's snapshot carries (scoped set + per-pod clusters), vs. the full
	// mesh service count.
	clusters metric.Int64Gauge
	// upstreamsDeclared is the size of the node's declared dependency union
	// (config.aether.io/upstreams across local pods).
	upstreamsDeclared metric.Int64Gauge
	// upstreamsObserved is the number of live ODCDS-observed dependencies.
	upstreamsObserved metric.Int64Gauge
	// upstreamsMiss counts ODCDS requests for services outside the node
	// dependency set — each is an undeclared upstream that should be
	// promoted to a config.aether.io/upstreams annotation.
	upstreamsMiss metric.Int64Counter
}

// New registers the snapshot instruments on the given meter.
func New(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
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
	if m.clusters, err = meter.Int64Gauge("aether.agent.snapshot.clusters",
		metric.WithDescription("Clusters in the node's current xDS snapshot (demand-scoped set + per-pod clusters)")); err != nil {
		return nil, fmt.Errorf("clusters: %w", err)
	}
	if m.upstreamsDeclared, err = meter.Int64Gauge("aether.agent.upstreams.declared",
		metric.WithDescription("Distinct upstream services declared by local pods (config.aether.io/upstreams union)")); err != nil {
		return nil, fmt.Errorf("upstreams declared: %w", err)
	}
	if m.upstreamsObserved, err = meter.Int64Gauge("aether.agent.upstreams.observed",
		metric.WithDescription("Live ODCDS-observed dependencies in the node dependency set")); err != nil {
		return nil, fmt.Errorf("upstreams observed: %w", err)
	}
	if m.upstreamsMiss, err = meter.Int64Counter("aether.agent.upstreams.miss",
		metric.WithDescription("ODCDS requests for services outside the node dependency set (undeclared upstreams; promote to annotations)")); err != nil {
		return nil, fmt.Errorf("upstreams miss: %w", err)
	}

	return m, nil
}

// SnapshotShape records per-snapshot size gauges.
func (m *Metrics) SnapshotShape(ctx context.Context, clusters, declared, observed int) {
	if m == nil {
		return
	}
	m.clusters.Record(ctx, int64(clusters))
	m.upstreamsDeclared.Record(ctx, int64(declared))
	m.upstreamsObserved.Record(ctx, int64(observed))
}

// UpstreamMiss counts one ODCDS miss. The service name is deliberately NOT a
// metric attribute (unbounded cardinality); it is logged instead.
func (m *Metrics) UpstreamMiss(ctx context.Context, _ string) {
	if m == nil {
		return
	}
	m.upstreamsMiss.Add(ctx, 1)
}

// Generated records the outcome of one snapshot generation.
func (m *Metrics) Generated(ctx context.Context, seconds float64, version int64, err error) {
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
