package spire

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-spire-bridge"

// attrStream labels which delegated-identity stream an event belongs to.
// Bounded cardinality: bundle or svid.
const attrStream = attribute.Key("aether.spire.stream")

const (
	streamBundle = "bundle"
	streamSVID   = "svid"
)

// bridgeMetrics holds the subscription-stream instruments. All methods are
// nil-receiver-safe so the bridge runs unchanged when telemetry is disabled.
//
// A flapping SPIRE agent used to surface only as an unexplained aether-agent
// restart (a bundle stream ending was fatal) or as a pod whose SVID silently
// stopped rotating; these counters make stream churn visible now that the
// bridge re-subscribes in place.
type bridgeMetrics struct {
	failures   metric.Int64Counter
	reconnects metric.Int64Counter

	// SDS publication-ordering instruments (issue #772). Both are zero in a
	// healthy agent, which is exactly why they are seeded: see newBridgeMetrics.
	stalePushes  metric.Int64Counter
	emptyBundles metric.Int64Counter
}

// newBridgeMetrics registers the subscription-stream instruments on the given meter.
func newBridgeMetrics(meter metric.Meter) (*bridgeMetrics, error) {
	m := &bridgeMetrics{}
	var err error

	if m.failures, err = meter.Int64Counter("aether.agent.spire.stream_failures",
		metric.WithDescription("SPIRE delegated-identity streams that ended unexpectedly or failed to re-subscribe, by stream kind (bundle/svid)")); err != nil {
		return nil, fmt.Errorf("stream failures: %w", err)
	}
	if m.reconnects, err = meter.Int64Counter("aether.agent.spire.stream_reconnects",
		metric.WithDescription("SPIRE delegated-identity streams re-established after a failure, by stream kind (bundle/svid)")); err != nil {
		return nil, fmt.Errorf("stream reconnects: %w", err)
	}
	if m.stalePushes, err = meter.Int64Counter("aether.agent.sds_push.stale_rejected",
		metric.WithDescription("SDS pushes dropped because the snapshot already holds a newer secret generation (an out-of-order publish that would have dropped a live secret)")); err != nil {
		return nil, fmt.Errorf("stale pushes: %w", err)
	}
	if m.emptyBundles, err = meter.Int64Counter("aether.agent.sds_push.empty_bundle_skipped",
		metric.WithDescription("Bundle updates that carried no trust bundles and were skipped to keep the previously served validation contexts")); err != nil {
		return nil, fmt.Errorf("empty bundles: %w", err)
	}

	// Seed the two publication-ordering counters at zero. The OTel SDK exports
	// a counter only after its first Add, so one that never increments — the
	// healthy case for both of these — never appears in Prometheus at all, and
	// "no series" is indistinguishable from "zero" to a grading query. Learned
	// the hard way on the #638 discriminator counters (issue #717).
	ctx := context.Background()
	m.stalePushes.Add(ctx, 0)
	m.emptyBundles.Add(ctx, 0)

	return m, nil
}

// streamFailed records one unexpected stream end or failed re-subscribe attempt.
func (m *bridgeMetrics) streamFailed(ctx context.Context, stream string) {
	if m == nil {
		return
	}
	m.failures.Add(ctx, 1, metric.WithAttributes(attrStream.String(stream)))
}

// streamReconnected records one stream re-established after a failure.
func (m *bridgeMetrics) streamReconnected(ctx context.Context, stream string) {
	if m == nil {
		return
	}
	m.reconnects.Add(ctx, 1, metric.WithAttributes(attrStream.String(stream)))
}

// stalePushRejected records one SDS push dropped for carrying an older secret
// generation than the snapshot already holds. Non-zero means publication
// ordering has been broken by a new caller and secrets could disappear from SDS.
func (m *bridgeMetrics) stalePushRejected(ctx context.Context) {
	if m == nil {
		return
	}
	m.stalePushes.Add(ctx, 1)
}

// emptyBundleSkipped records one bundle update that would have left the proxy
// with no validation contexts and was skipped in favour of the cached bundles.
func (m *bridgeMetrics) emptyBundleSkipped(ctx context.Context) {
	if m == nil {
		return
	}
	m.emptyBundles.Add(ctx, 1)
}
