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
	// upstreamsTTLRefreshed counts observed dependencies that crossed the idle
	// TTL but were REFRESHED instead of expired because the node's proxy still
	// holds a live on-demand subscription for them (issue #682). It is the only
	// external evidence that the in-use exemption is doing anything: the
	// exemption's whole effect is that nothing happens — no expiry, no cluster
	// drop, no ODCDS re-warm — so without this counter a validator cannot tell
	// a working exemption from a demand set that simply never aged.
	upstreamsTTLRefreshed metric.Int64Counter
	// upstreamsRestored counts observed dependencies re-admitted from the
	// agent's local storage at start (issue #701): the demand a previous agent
	// on this node had already granted, carried across a full agent+proxy
	// replacement so the first snapshot serves it. Incremented once per
	// restart; NOT misses — nothing asked for anything new.
	upstreamsRestored metric.Int64Counter
	// bindingMismatch counts local source pods whose outbound clusters are
	// bound to an SDS client-certificate secret that is NOT that pod's own
	// SPIFFE ID (issue #638). Non-zero means the node's egress would present a
	// co-located workload's SVID on that pod's behalf.
	bindingMismatch metric.Int64Counter
	// inboundBindingMismatch counts local pods whose INBOUND filter chains are
	// bound to an SDS server-certificate secret that is NOT that pod's own
	// SPIFFE ID (issue #638). Non-zero means the node would TERMINATE mesh mTLS
	// for that pod while presenting a co-located workload's SVID — which is
	// exactly what a caller's ssl_fail_verify_san rejects.
	inboundBindingMismatch metric.Int64Counter
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
	if m.upstreamsTTLRefreshed, err = meter.Int64Counter("aether.agent.upstreams.ttl_refreshed",
		metric.WithDescription("Observed dependencies past the idle TTL kept in the node dependency set because the proxy still holds a live on-demand subscription")); err != nil {
		return nil, fmt.Errorf("upstreams ttl refreshed: %w", err)
	}
	if m.upstreamsRestored, err = meter.Int64Counter("aether.agent.upstreams.restored",
		metric.WithDescription("Observed dependencies restored from the agent's local storage at start (a replaced agent starting warm)")); err != nil {
		return nil, fmt.Errorf("upstreams restored: %w", err)
	}
	if m.bindingMismatch, err = meter.Int64Counter("aether.agent.identity.outbound_binding_mismatch",
		metric.WithDescription("Local source pods whose outbound clusters are bound to another workload's SDS client-certificate secret")); err != nil {
		return nil, fmt.Errorf("outbound binding mismatch: %w", err)
	}
	if m.inboundBindingMismatch, err = meter.Int64Counter("aether.agent.identity.inbound_binding_mismatch",
		metric.WithDescription("Inbound filter chains bound to another workload's SDS server-certificate secret")); err != nil {
		return nil, fmt.Errorf("inbound binding mismatch: %w", err)
	}

	// Seed the two #638 discriminator counters at zero. The OTel SDK exports a
	// counter only after its first Add, so a counter that is never incremented
	// (the healthy case for both of these) never appears in Prometheus at all —
	// and "no series" is indistinguishable from "zero" to a grading query.
	// Seeding makes a live zero visible and lets increase()/rate() work from
	// process start. Observed on talos-main rev200: neither series existed.
	ctx := context.Background()
	m.bindingMismatch.Add(ctx, 0)
	m.inboundBindingMismatch.Add(ctx, 0)

	return m, nil
}

// OutboundBindingMismatch counts n source pods found bound to a foreign
// identity in one snapshot generation (issue #638). The pod and the two SPIFFE
// IDs are deliberately NOT attributes (unbounded cardinality); they are logged
// at WARN instead. A no-op for n <= 0 so the steady state records nothing.
func (m *Metrics) OutboundBindingMismatch(ctx context.Context, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.bindingMismatch.Add(ctx, n)
}

// InboundBindingMismatch counts n inbound filter chains found bound to a
// foreign server certificate in one snapshot generation (issue #638). The pod
// and the two SPIFFE IDs are deliberately NOT attributes (unbounded
// cardinality); they are logged at WARN instead. A no-op for n <= 0 so the
// steady state records nothing.
func (m *Metrics) InboundBindingMismatch(ctx context.Context, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.inboundBindingMismatch.Add(ctx, n)
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

// UpstreamTTLRefreshed counts n observed dependencies exempted from idle expiry
// in one prune pass by a live on-demand subscription (issue #682). Service names
// are deliberately NOT attributes (unbounded cardinality). A no-op for n <= 0 so
// the steady state — nothing near its TTL — records nothing.
func (m *Metrics) UpstreamTTLRefreshed(ctx context.Context, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.upstreamsTTLRefreshed.Add(ctx, n)
}

// UpstreamsRestored counts n observed dependencies re-admitted from local
// storage at start (issue #701). Service names are deliberately NOT attributes
// (unbounded cardinality); the restore log line names them. A no-op for
// n <= 0 so a cold start records nothing.
func (m *Metrics) UpstreamsRestored(ctx context.Context, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.upstreamsRestored.Add(ctx, n)
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
