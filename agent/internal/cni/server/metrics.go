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
	ghostsRemoved      metric.Int64Counter
	missingRegistered  metric.Int64Counter
	stalePruned        metric.Int64Counter
	orphansPruned      metric.Int64Counter
	missingStorage     metric.Int64Counter
	sweepErrors        metric.Int64Counter
	pruneBreaker       metric.Int64Counter
	pruneBreakerOpen   metric.Int64Gauge
	lostAddEvictions   metric.Int64Counter
	staleRunningEvicts metric.Int64Counter
	unmeshedPods       metric.Int64Gauge
	storagePods        metric.Int64Gauge
	staleStoragePods   metric.Int64Gauge
	healthTransitions  metric.Int64Counter
	promotionDelay     metric.Float64Histogram
	spiffeIDOverrides  metric.Int64Counter
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
	if m.orphansPruned, err = meter.Int64Counter("aether.agent.ghost_sweep.orphans_pruned",
		metric.WithDescription("Local storage pod entries pruned because the Kubernetes pod no longer exists (missed CNI DEL whose netns pin lingered, so the netns check could not catch it)")); err != nil {
		return nil, fmt.Errorf("orphans pruned: %w", err)
	}
	if m.missingStorage, err = meter.Int64Counter("aether.agent.ghost_sweep.missing_storage",
		metric.WithDescription("Live mesh-managed pods on this node with no local storage entry (lost CNI ADD); self-healed by eviction after the detection threshold (#567)")); err != nil {
		return nil, fmt.Errorf("missing storage: %w", err)
	}
	if m.sweepErrors, err = meter.Int64Counter("aether.agent.ghost_sweep.errors",
		metric.WithDescription("Ghost sweep cycles that failed before reconciling")); err != nil {
		return nil, fmt.Errorf("sweep errors: %w", err)
	}
	if m.pruneBreaker, err = meter.Int64Counter("aether.agent.ghost_sweep.prune_breaker_tripped",
		metric.WithDescription("Ghost sweep passes whose mass-delete circuit breaker refused to prune (correlated netns-check failure suspected)")); err != nil {
		return nil, fmt.Errorf("prune breaker: %w", err)
	}
	if m.pruneBreakerOpen, err = meter.Int64Gauge("aether.agent.ghost_sweep.prune_breaker_engaged",
		metric.WithDescription("1 while the ghost sweep's mass-delete circuit breaker is withholding netns-derived pruning on this node, 0 when clear; a standing 1 means stale storage cannot be cleaned and will grow (#670)")); err != nil {
		return nil, fmt.Errorf("prune breaker engaged: %w", err)
	}
	if m.lostAddEvictions, err = meter.Int64Counter("aether.agent.ghost_sweep.lost_add_evictions",
		metric.WithDescription("Pods evicted by the ghost sweep to force a fresh CNI ADD after a lost one left them with no mesh listener")); err != nil {
		return nil, fmt.Errorf("lost add evictions: %w", err)
	}
	if m.staleRunningEvicts, err = meter.Int64Counter("aether.agent.ghost_sweep.stale_running_evictions",
		metric.WithDescription("Pods evicted by the ghost sweep because their stored registration referenced a defunct network namespace while the pod was Running — the node-reboot CNI boot race (#640)")); err != nil {
		return nil, fmt.Errorf("stale running evictions: %w", err)
	}
	if m.unmeshedPods, err = meter.Int64Gauge("aether.agent.ghost_sweep.unmeshed_pods",
		metric.WithDescription("Running mesh-managed pods on this node currently outside the mesh: missing from local storage (lost CNI ADD, #567) or covered only by stale dead-netns registrations (reboot boot race, #640); nonzero means traffic to/from these pods bypasses the mesh")); err != nil {
		return nil, fmt.Errorf("unmeshed pods: %w", err)
	}
	if m.storagePods, err = meter.Int64Gauge("aether.agent.storage.pods",
		metric.WithDescription("Pods currently tracked in the agent's local file storage")); err != nil {
		return nil, fmt.Errorf("storage pods: %w", err)
	}
	if m.staleStoragePods, err = meter.Int64Gauge("aether.agent.storage.stale_pods",
		metric.WithDescription("Local storage pod entries this node's Kubernetes pod list does not account for (missed CNI DEL), whether or not they were prunable this pass; against aether.agent.storage.pods this is the direct stale-vs-live ratio (#670)")); err != nil {
		return nil, fmt.Errorf("stale storage pods: %w", err)
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
	if m.spiffeIDOverrides, err = meter.Int64Counter("aether.agent.identity.spiffe_id_override_rejected",
		metric.WithDescription("Pods carrying the rejected aether.io/spiffe-id annotation, whose mesh identity was derived from the pod's own namespace/ServiceAccount instead (#669); nonzero means someone is trying to choose a workload identity by annotation")); err != nil {
		return nil, fmt.Errorf("spiffe id overrides: %w", err)
	}

	return m, nil
}

// spiffeIDOverrideRejected records one pod whose aether.io/spiffe-id annotation
// was ignored. Counted per pod-lifecycle event that reads the pod's identity (a
// CNI ADD, or an agent-restart resubscribe), not per config push.
func (m *cniMetrics) spiffeIDOverrideRejected(ctx context.Context) {
	if m == nil {
		return
	}
	m.spiffeIDOverrides.Add(ctx, 1)
}

func (m *cniMetrics) sweepCompleted(ctx context.Context, ghostsRemoved, missingRegistered, stalePruned, orphansPruned, missingStorage, staleRunning, storedPods int, err error) {
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
	if orphansPruned > 0 {
		m.orphansPruned.Add(ctx, int64(orphansPruned))
	}
	if missingStorage > 0 {
		m.missingStorage.Add(ctx, int64(missingStorage))
	}
	// Recorded every pass, zero included, so the gauge clears the moment the last
	// unmeshed pod re-registers — it is the alerting signal for "a Running pod is
	// outside the mesh" (#640).
	m.unmeshedPods.Record(ctx, int64(missingStorage+staleRunning))
	m.storagePods.Record(ctx, int64(storedPods))
}

// pruneBreakerTripped records a sweep pass whose mass-delete circuit breaker
// refused to prune (#566).
func (m *cniMetrics) pruneBreakerTripped(ctx context.Context) {
	if m == nil {
		return
	}
	m.pruneBreaker.Add(ctx, 1)
}

// pruneBreakerState records whether the mass-delete circuit breaker is engaged
// (1) or clear (0). Recorded every sweep pass, zero included, so "the breaker is
// standing open" is a state an alert can match rather than a counter slope
// somebody has to notice — the counter climbed once a minute on every node of
// talos-main for weeks and went unremarked (#670).
func (m *cniMetrics) pruneBreakerState(ctx context.Context, engaged bool) {
	if m == nil {
		return
	}
	var v int64
	if engaged {
		v = 1
	}
	m.pruneBreakerOpen.Record(ctx, v)
}

// staleStorageEntriesObserved records how many storage entries this pass had no
// Kubernetes pod on this node — the stale backlog as DETECTED, independent of
// whether it could be pruned. orphans_pruned reads zero both when there is no
// staleness and when cleanup is blocked; this gauge tells those apart (#670).
func (m *cniMetrics) staleStorageEntriesObserved(ctx context.Context, stale int) {
	if m == nil {
		return
	}
	m.staleStoragePods.Record(ctx, int64(stale))
}

// lostAddEvicted records a pod evicted to force a fresh CNI ADD (#567).
func (m *cniMetrics) lostAddEvicted(ctx context.Context) {
	if m == nil {
		return
	}
	m.lostAddEvictions.Add(ctx, 1)
}

// staleRunningEvicted records a pod evicted because its registration referenced
// a defunct netns while the pod was Running (#640).
func (m *cniMetrics) staleRunningEvicted(ctx context.Context) {
	if m == nil {
		return
	}
	m.staleRunningEvicts.Add(ctx, 1)
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
