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
