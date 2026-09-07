package spire

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// waitMeterName identifies this instrumentation scope in metric backends.
const waitMeterName = "aether/spire-identity"

// waitSecondsBuckets are the explicit boundaries, in SECONDS, for
// aether.<component>.spire.wait_seconds. The OTel defaults are millisecond-oriented
// (0, 5, 10, … 10000), which would put every wait short of 5s — the healthy
// case — into one bucket and every outage into the next (#732). The interesting
// range is one retry (1s) to well past the warn threshold (2m).
var waitSecondsBuckets = []float64{0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300, 600}

// metricName builds this component's name for one of the wait instruments.
func metricName(component, instrument string) string {
	return fmt.Sprintf("aether.%s.spire.%s", component, instrument)
}

// waitMetrics instruments the wait for the first SVID. All methods are
// nil-receiver-safe so the wait runs unchanged when instrument registration
// failed or telemetry is disabled.
type waitMetrics struct {
	wait     metric.Float64Histogram
	restarts metric.Int64Counter
	ready    metric.Int64ObservableGauge
	meter    metric.Meter
}

// newWaitMetrics registers the identity-wait instruments on the given meter,
// namespaced by the component that is waiting (aether.agent.spire.*,
// aether.controller.spire.*, aether.registrar.spire.*, aether.edge.spire.*).
//
// The instruments are defined ONCE, here, and only their namespace varies: four
// binaries wait for the same thing for the same reason, and an operator reading
// one dashboard panel per component should be reading the same three series. The
// namespace is per-component rather than a component ATTRIBUTE because these are
// separate workloads with separate resource attributes, and #210's lesson is that
// a fleet-collapsed counter makes rate() lie.
func newWaitMetrics(meter metric.Meter, component string) (*waitMetrics, error) {
	m := &waitMetrics{meter: meter}
	var err error

	if m.wait, err = meter.Float64Histogram(metricName(component, "wait_seconds"),
		metric.WithDescription("Seconds from process start to the SPIRE Workload API issuing this workload's first SVID (recorded once, when it arrives)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(waitSecondsBuckets...)); err != nil {
		return nil, fmt.Errorf("spire wait seconds: %w", err)
	}
	if m.restarts, err = meter.Int64Counter(metricName(component, "source_restarts"),
		metric.WithDescription("Workload API sources that connected but could not serve an SVID and were re-created; expected to stay 0")); err != nil {
		return nil, fmt.Errorf("spire source restarts: %w", err)
	}
	if m.ready, err = meter.Int64ObservableGauge(metricName(component, "svid_ready"),
		metric.WithDescription("1 when this workload holds a SPIRE SVID, 0 while it is still waiting for its first one")); err != nil {
		return nil, fmt.Errorf("spire svid ready: %w", err)
	}

	return m, nil
}

// observeReadiness wires the svid_ready gauge to ready, which is polled on every
// collection. A registration failure only drops the gauge.
func (m *waitMetrics) observeReadiness(ready func() bool) {
	if m == nil {
		return
	}
	_, _ = m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		var v int64
		if ready() {
			v = 1
		}
		o.ObserveInt64(m.ready, v)
		return nil
	}, m.ready)
}

// waited records the one-shot wait duration once the first SVID has arrived.
func (m *waitMetrics) waited(ctx context.Context, d time.Duration) {
	if m == nil {
		return
	}
	m.wait.Record(ctx, d.Seconds())
}

// sourceRestarted records one Workload API source discarded and re-created.
func (m *waitMetrics) sourceRestarted(ctx context.Context) {
	if m == nil {
		return
	}
	m.restarts.Add(ctx, 1)
}
