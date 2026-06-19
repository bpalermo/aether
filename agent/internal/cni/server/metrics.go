package server

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-cni-server"

// Health-transition attribute keys. Bounded cardinality: health states only.
const (
	attrHealthFrom = attribute.Key("aether.health.from")
	attrHealthTo   = attribute.Key("aether.health.to")
)

// cniMetrics holds the reconciliation-loop instruments. All methods are
// nil-receiver-safe so the server runs unchanged when telemetry is disabled.
//
// Every ghost-sweep correction is direct evidence of a missed update somewhere
// in the pipeline (a lost CNI DEL, a registration dropped during a registry
// outage) — these counters turn silently-self-healing bugs into visible ones.
type cniMetrics struct {
	ghostsRemoved     metric.Int64Counter
	missingRegistered metric.Int64Counter
	stalePruned       metric.Int64Counter
	sweepErrors       metric.Int64Counter
	storagePods       metric.Int64Gauge
	healthTransitions metric.Int64Counter
	promotionDelay    metric.Float64Histogram
}

// newCNIMetrics registers the reconciliation instruments on the given meter.
func newCNIMetrics(meter metric.Meter) (*cniMetrics, error) {
	m := &cniMetrics{}
	var err error

	if m.ghostsRemoved, err = meter.Int64Counter("aether.agent.ghost_sweep.ghosts_removed",
		metric.WithDescription("Registry endpoints deregistered because no live local pod accounts for them (missed CNI DEL)")); err != nil {
		return nil, fmt.Errorf("ghosts removed: %w", err)
	}
	if m.missingRegistered, err = meter.Int64Counter("aether.agent.ghost_sweep.missing_registered",
		metric.WithDescription("Live local pods re-registered because the registry was missing them (missed CNI ADD registration)")); err != nil {
		return nil, fmt.Errorf("missing registered: %w", err)
	}
	if m.stalePruned, err = meter.Int64Counter("aether.agent.ghost_sweep.stale_pruned",
		metric.WithDescription("Local storage pod entries pruned because their network namespace no longer exists (missed CNI DEL); keeping them faults Envoy on the dead netns")); err != nil {
		return nil, fmt.Errorf("stale pruned: %w", err)
	}
	if m.sweepErrors, err = meter.Int64Counter("aether.agent.ghost_sweep.errors",
		metric.WithDescription("Ghost sweep cycles that failed before reconciling")); err != nil {
		return nil, fmt.Errorf("sweep errors: %w", err)
	}
	if m.storagePods, err = meter.Int64Gauge("aether.agent.storage.pods",
		metric.WithDescription("Pods currently tracked in the agent's local file storage")); err != nil {
		return nil, fmt.Errorf("storage pods: %w", err)
	}
	if m.healthTransitions, err = meter.Int64Counter("aether.agent.liveness.health_transitions",
		metric.WithDescription("Endpoint health transitions reflected into the registry by the liveness loop")); err != nil {
		return nil, fmt.Errorf("health transitions: %w", err)
	}
	if m.promotionDelay, err = meter.Float64Histogram("aether.agent.liveness.promotion_delay_seconds",
		metric.WithDescription("Seconds from the liveness loop first observing a pod's programmed health gateway to promoting it HEALTHY in the registry"),
		metric.WithUnit("s")); err != nil {
		return nil, fmt.Errorf("promotion delay: %w", err)
	}

	return m, nil
}

func (m *cniMetrics) sweepCompleted(ctx context.Context, ghostsRemoved, missingRegistered, stalePruned, storedPods int, err error) {
	if m == nil {
		return
	}
	if err != nil {
		m.sweepErrors.Add(ctx, 1)
		return
	}
	if ghostsRemoved > 0 {
		m.ghostsRemoved.Add(ctx, int64(ghostsRemoved))
	}
	if missingRegistered > 0 {
		m.missingRegistered.Add(ctx, int64(missingRegistered))
	}
	if stalePruned > 0 {
		m.stalePruned.Add(ctx, int64(stalePruned))
	}
	m.storagePods.Record(ctx, int64(storedPods))
}

func (m *cniMetrics) healthTransition(ctx context.Context, from, to string) {
	if m == nil {
		return
	}
	m.healthTransitions.Add(ctx, 1, metric.WithAttributes(
		attrHealthFrom.String(from),
		attrHealthTo.String(to),
	))
}

// promotionDelayObserved records how long a new pod sat between its health
// gateway becoming observable and its first HEALTHY promotion — the mesh's
// endpoint-promotion latency (the e2e-measured gap that lets k8s rolls outpace
// mesh routability).
func (m *cniMetrics) promotionDelayObserved(ctx context.Context, seconds float64) {
	if m == nil {
		return
	}
	m.promotionDelay.Record(ctx, seconds)
}
