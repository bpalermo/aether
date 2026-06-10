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
	sweepErrors       metric.Int64Counter
	storagePods       metric.Int64Gauge
	healthTransitions metric.Int64Counter
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

	return m, nil
}

func (m *cniMetrics) sweepCompleted(ctx context.Context, ghostsRemoved, missingRegistered, storedPods int, err error) {
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
