package cniconflist

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-cni-conflist"

// reassertMetrics holds the re-assert loop's instruments. All methods are
// nil-receiver-safe so the loop runs unchanged when telemetry is disabled or
// instrument registration failed.
type reassertMetrics struct {
	reasserts metric.Int64Counter
	chained   metric.Int64Gauge
}

// newMetrics registers the re-assert instruments on the given meter.
func newMetrics(meter metric.Meter) (*reassertMetrics, error) {
	m := &reassertMetrics{}
	var err error

	if m.reasserts, err = meter.Int64Counter("aether.agent.cni_conflist.reasserts",
		metric.WithDescription("Times the agent re-appended aether's plugin entry to the node's active CNI conflist after a competing writer stripped it (kube-flannel's cp -f on pod recreation, #645); every increment is a window in which newly-started pods were created outside the mesh")); err != nil {
		return nil, fmt.Errorf("reasserts: %w", err)
	}
	if m.chained, err = meter.Int64Gauge("aether.agent.cni_conflist.chained",
		metric.WithDescription("1 when aether's plugin entry is present in the node's active CNI conflist, 0 when it is missing or no usable conflist exists; 0 means every pod created on this node from now on bypasses the mesh")); err != nil {
		return nil, fmt.Errorf("chained: %w", err)
	}

	return m, nil
}

// chainedState records the chain-present gauge. Called on every check, zero
// included, so the gauge clears the instant the entry is back — the same
// recording philosophy as the ghost sweep's unmeshed_pods gauge (#641).
func (m *reassertMetrics) chainedState(ctx context.Context, chained bool) {
	if m == nil {
		return
	}
	var v int64
	if chained {
		v = 1
	}
	m.chained.Record(ctx, v)
}

// reasserted records one successful re-assert.
func (m *reassertMetrics) reasserted(ctx context.Context) {
	if m == nil {
		return
	}
	m.reasserts.Add(ctx, 1)
}
