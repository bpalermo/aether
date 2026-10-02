package hotrestart

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Wedge / trigger / exit attribute keys. All values are from small fixed sets.
const (
	attrWedgeReason   = attribute.Key("aether.supervisor.wedge.reason")
	attrTriggerReason = attribute.Key("aether.supervisor.trigger")
	attrExitKind      = attribute.Key("aether.supervisor.exit_kind")
	attrReady         = attribute.Key("aether.supervisor.ready")
)

// Admin-probe attribute keys. Both values are from small fixed sets.
const (
	attrProbeEndpoint = attribute.Key("aether.supervisor.probe.endpoint")
	attrProbeResult   = attribute.Key("aether.supervisor.probe.result")
)

// attrShutdownBranch labels which arm of handleShutdown's decision table a
// SIGTERM resolved on. Four values, fixed.
const attrShutdownBranch = attribute.Key("aether.supervisor.shutdown.branch")

// Shutdown branches. See handleShutdown's decision table (issue #795).
const (
	// shutdownBranchHandoff: the admin answered at a NEWER epoch — a successor
	// already holds our listen sockets — and its parent-shutdown protocol
	// terminated our Envoy inside the budget. The hitless mid-roll case.
	shutdownBranchHandoff = "handoff"
	// shutdownBranchSuccessorWait: the admin answered LIVE at OUR epoch (no
	// handoff had begun), we kept serving, and a surge successor arrived and
	// took over inside the budget. The hitless case for `kubectl delete pod`,
	// node drain, eviction and preemption.
	shutdownBranchSuccessorWait = "successor_wait"
	// shutdownBranchDrainFallback: no successor came (node shutdown, scale-down,
	// `kubectl delete daemonset`, a replacement stuck Pending) or
	// --shutdown-drain-immediately declared that none ever would, so the
	// supervisor drained Envoy's listeners itself before stopping it.
	shutdownBranchDrainFallback = "drain_fallback"
	// shutdownBranchChildDead: the admin did not answer at all — the child is
	// gone, or its main thread is wedged. Nothing to drain, nobody to hand to.
	shutdownBranchChildDead = "child_dead"
)

// shutdownBranchValues is the closed set, used to seed the counter at zero.
var shutdownBranchValues = []string{
	shutdownBranchHandoff,
	shutdownBranchSuccessorWait,
	shutdownBranchDrainFallback,
	shutdownBranchChildDead,
}

// Admin-mutation attribute keys (issue #1127): which state-changing admin
// request the supervisor meant to send, and whose Envoy the identity check found
// on the shared admin address. Both from small fixed sets.
const (
	attrAdminRequest = attribute.Key("aether.supervisor.admin.request")
	attrAdminOwner   = attribute.Key("aether.supervisor.admin.owner")
)

// adminRequestDrainListeners is the only state-changing admin request the
// supervisor makes.
const adminRequestDrainListeners = "drain_listeners"

// adminOwnerValues is the closed set, used to seed the counter at zero.
var adminOwnerValues = []adminOwnership{adminOwn, adminForeign, adminUnreachable, adminOwnGone}

// attrStallClass labels an Envoy thread stall (stallsampler.go, issue #1093).
// Three values, fixed: starved, blocked, busy.
const attrStallClass = attribute.Key("aether.supervisor.stall.class")

// attrHandoffMode labels how a supervisor took over from a live predecessor
// (issue #1136): hot or fresh_after_drain.
const attrHandoffMode = attribute.Key("mode")

// Admin endpoints the watchdog probes.
const (
	probeEndpointReady      = "ready"
	probeEndpointServerInfo = "server_info"
)

// Admin probe outcomes.
const (
	probeResultLive        = "live"
	probeResultNotLive     = "not_live"
	probeResultUnreachable = "unreachable"
)

// Wedge reasons (watchdog diagnoses).
const (
	wedgeHandoffTimeout    = "handoff_timeout"
	wedgeAdminUnresponsive = "admin_unresponsive"
)

// Child exit kinds.
const (
	exitDrained             = "drained"              // older epoch finished draining after a hot restart
	exitUnexpected          = "unexpected"           // newest epoch died — pod restart follows
	exitSuccessorTerminated = "successor_terminated" // cross-pod handoff: successor's parent-shutdown protocol
	exitBindCollision       = "bind_collision_retry" // fresh epoch lost the base-id socket bind race; retried in-process
)

// SupervisorMetrics holds the hot-restart lifecycle instruments. All methods
// are nil-receiver-safe so the supervisor runs unchanged without telemetry.
//
// These exist for post-mortems of wedged or crashed proxy pods: the epoch
// gauge and handoff histogram show how far a handoff got and how long it
// took, the wedge counter says why the watchdog killed the pod, and the
// child-exit counter distinguishes expected drains from crashes.
type SupervisorMetrics struct {
	epoch               metric.Int64Gauge
	handoffDuration     metric.Float64Histogram
	wedges              metric.Int64Counter
	restartTriggers     metric.Int64Counter
	childExits          metric.Int64Counter
	configRejections    metric.Int64Counter
	predecessorDetected metric.Int64Gauge
	drainDuration       metric.Float64Histogram
	readyTransitions    metric.Int64Counter
	adminProbes         metric.Int64Counter
	shutdownBranches    metric.Int64Counter
	childSilent         metric.Int64Counter
	envoyThreadStalls   metric.Int64Counter
	adminMutations      metric.Int64Counter
	handoffModes        metric.Int64Counter
}

// NewSupervisorMetrics registers the supervisor instruments on the given meter.
func NewSupervisorMetrics(meter metric.Meter) (*SupervisorMetrics, error) {
	m := &SupervisorMetrics{}
	var err error

	if m.epoch, err = meter.Int64Gauge("aether.supervisor.epoch",
		metric.WithDescription("Newest Envoy restart epoch launched by this supervisor")); err != nil {
		return nil, fmt.Errorf("epoch: %w", err)
	}
	if m.handoffDuration, err = meter.Float64Histogram("aether.supervisor.handoff.duration",
		metric.WithDescription("Time from forking an Envoy epoch to the admin confirming it LIVE"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120, 300)); err != nil {
		return nil, fmt.Errorf("handoff duration: %w", err)
	}
	if m.wedges, err = meter.Int64Counter("aether.supervisor.wedges",
		metric.WithDescription("Watchdog wedge diagnoses (each one terminates the pod), by reason")); err != nil {
		return nil, fmt.Errorf("wedges: %w", err)
	}
	if m.restartTriggers, err = meter.Int64Counter("aether.supervisor.restart_triggers",
		metric.WithDescription("Hot-restart trigger arms, by source (debounce may coalesce several into one restart)")); err != nil {
		return nil, fmt.Errorf("restart triggers: %w", err)
	}
	if m.childExits, err = meter.Int64Counter("aether.supervisor.child_exits",
		metric.WithDescription("Supervised Envoy process exits, by kind")); err != nil {
		return nil, fmt.Errorf("child exits: %w", err)
	}
	if m.configRejections, err = meter.Int64Counter("aether.supervisor.config_validation_failures",
		metric.WithDescription("Changed bootstrap configs rejected by node-local envoy --mode validate before hot restart (each one is a prevented outage; the current epoch keeps serving)")); err != nil {
		return nil, fmt.Errorf("config validation failures: %w", err)
	}
	if m.predecessorDetected, err = meter.Int64Gauge("aether.supervisor.predecessor_detected",
		metric.WithDescription("1 if a live predecessor was found at startup (cross-pod hot restart), 0 for a fresh epoch-0 start")); err != nil {
		return nil, fmt.Errorf("predecessor detected: %w", err)
	}
	if m.drainDuration, err = meter.Float64Histogram("aether.supervisor.drain.duration",
		metric.WithDescription("Time spent draining Envoy on a shutdown fallback, from the graceful /drain_listeners request (where one is made) to the last supervised Envoy exiting"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120)); err != nil {
		return nil, fmt.Errorf("drain duration: %w", err)
	}
	if m.readyTransitions, err = meter.Int64Counter("aether.supervisor.ready_transitions",
		metric.WithDescription("Readiness marker transitions, by new state")); err != nil {
		return nil, fmt.Errorf("ready transitions: %w", err)
	}
	if m.adminProbes, err = meter.Int64Counter("aether.supervisor.admin_probes",
		metric.WithDescription("Envoy admin watchdog probes, by endpoint and outcome. The server_info share is the epoch re-verification rate; a steady-state ratio other than ~1:14 against ready means the fast path is not engaging (#646)")); err != nil {
		return nil, fmt.Errorf("admin probes: %w", err)
	}
	if m.shutdownBranches, err = meter.Int64Counter("aether.supervisor.shutdown.branch",
		metric.WithDescription("SIGTERM outcomes, by which arm of the shutdown decision table was taken. handoff and successor_wait are the hitless ones; a drain_fallback during a rolling upgrade means the surge replacement never arrived (#795)")); err != nil {
		return nil, fmt.Errorf("shutdown branches: %w", err)
	}
	if m.childSilent, err = meter.Int64Counter("aether.supervisor.child_silent",
		metric.WithDescription("Hot-restart children whose admin stopped answering for 10s after they went LIVE, inside the parent-shutdown window: the #1050 wedge signature, counted once per epoch and well before the liveness watchdog fires (#1058). Expected 0 per roll")); err != nil {
		return nil, fmt.Errorf("child silent: %w", err)
	}
	if m.envoyThreadStalls, err = meter.Int64Counter("aether.supervisor.envoy_thread_stalls",
		metric.WithDescription("One-second windows in which an Envoy main or worker thread was starved of CPU (runnable but waiting), blocked outside its idle epoll wait, or busy (on a CPU for >=90% of the window) for at least the stall threshold, counted per thread and class (#1093). The matching 'envoy thread stall' log line names the thread, the wchan it blocked in and the node's CPU/softirq/PSI picture for that second")); err != nil {
		return nil, fmt.Errorf("envoy thread stalls: %w", err)
	}
	// Seeded per class: the soak reading is "stalls per roll, by class", and an
	// unseeded class reads the same as a sampler that never ran.
	for _, class := range stallClassValues {
		m.envoyThreadStalls.Add(context.Background(), 0, metric.WithAttributes(attrStallClass.String(class)))
	}
	// Seeded at zero for the same reason as the shutdown branches below: the
	// soak gate is "0 per roll", and an unseeded counter cannot tell 0 from
	// "never exported".
	m.childSilent.Add(context.Background(), 0)
	// Seed every branch at zero. A supervisor records exactly ONE shutdown
	// branch, once, at the very end of its life, and the OTel SDK exports a
	// counter only after its first Add — so without this, "the fleet never took
	// the drain_fallback branch" and "this counter was never registered" are the
	// same empty result in Prometheus. That is precisely how #638's identity
	// mismatch counters read as false zeros until #717 seeded them.
	for _, branch := range shutdownBranchValues {
		m.shutdownBranches.Add(context.Background(), 0, metric.WithAttributes(attrShutdownBranch.String(branch)))
	}

	// Registered in a helper so this constructor stays under the gocognit bar.
	return m.withAdminMutations(meter)
}

func (m *SupervisorMetrics) epochStarted(epoch int) {
	if m == nil {
		return
	}
	m.epoch.Record(context.Background(), int64(epoch))
}

func (m *SupervisorMetrics) handoffCompleted(seconds float64) {
	if m == nil {
		return
	}
	m.handoffDuration.Record(context.Background(), seconds)
}

func (m *SupervisorMetrics) wedged(reason string) {
	if m == nil {
		return
	}
	m.wedges.Add(context.Background(), 1, metric.WithAttributes(attrWedgeReason.String(reason)))
}

func (m *SupervisorMetrics) restartTriggered(trigger string) {
	if m == nil {
		return
	}
	m.restartTriggers.Add(context.Background(), 1, metric.WithAttributes(attrTriggerReason.String(trigger)))
}

func (m *SupervisorMetrics) childExited(kind string) {
	if m == nil {
		return
	}
	m.childExits.Add(context.Background(), 1, metric.WithAttributes(attrExitKind.String(kind)))
}

func (m *SupervisorMetrics) configValidationFailed() {
	if m == nil {
		return
	}
	m.configRejections.Add(context.Background(), 1)
}

func (m *SupervisorMetrics) predecessorFound(found bool) {
	if m == nil {
		return
	}
	v := int64(0)
	if found {
		v = 1
	}
	m.predecessorDetected.Record(context.Background(), v)
}

func (m *SupervisorMetrics) drainCompleted(seconds float64) {
	if m == nil {
		return
	}
	m.drainDuration.Record(context.Background(), seconds)
}

func (m *SupervisorMetrics) readyTransition(ready bool) {
	if m == nil {
		return
	}
	m.readyTransitions.Add(context.Background(), 1, metric.WithAttributes(attrReady.Bool(ready)))
}

// shutdownBranchTaken records the arm of handleShutdown's decision table this
// SIGTERM resolved on. Called exactly once per supervisor lifetime, just before
// Run returns; supervisorcmd's deferred telemetry flush is what gets it out of
// the dying process.
func (m *SupervisorMetrics) shutdownBranchTaken(branch string) {
	if m == nil {
		return
	}
	m.shutdownBranches.Add(context.Background(), 1, metric.WithAttributes(attrShutdownBranch.String(branch)))
}

// childSilentDetected counts one silent hot-restart child (see
// checkChildSilent).
func (m *SupervisorMetrics) childSilentDetected() {
	if m == nil {
		return
	}
	m.childSilent.Add(context.Background(), 1)
}

// envoyThreadStalled counts one flagged thread-window of the given class (see
// stallSampler.closeWindow).
func (m *SupervisorMetrics) envoyThreadStalled(class string) {
	if m == nil {
		return
	}
	m.envoyThreadStalls.Add(context.Background(), 1, metric.WithAttributes(attrStallClass.String(class)))
}

// withAdminMutations registers the admin-mutation counter, seeded at zero for
// every owner verdict: the gate is "foreign stays 0 per roll", and an unseeded
// counter cannot tell 0 from "never exported" (#717).
func (m *SupervisorMetrics) withAdminMutations(meter metric.Meter) (*SupervisorMetrics, error) {
	var err error
	if m.adminMutations, err = meter.Int64Counter("aether.supervisor.admin_mutations",
		metric.WithDescription("State-changing Envoy admin requests the supervisor decided on, by request and by whose Envoy the identity check found on the node-shared admin address. Only owner=own is sent; foreign means another pod's Envoy answered and was left alone (#1127)")); err != nil {
		return nil, fmt.Errorf("admin mutations: %w", err)
	}
	for _, owner := range adminOwnerValues {
		m.adminMutations.Add(context.Background(), 0, metric.WithAttributes(
			attrAdminRequest.String(adminRequestDrainListeners), attrAdminOwner.String(string(owner))))
	}
	return m.withHandoffModes(meter)
}

// withHandoffModes registers the handoff-mode counter (issue #1136), seeded at
// zero per mode for the same reason as admin_mutations: "no fresh start this
// roll" must read as 0, not as an absent series.
func (m *SupervisorMetrics) withHandoffModes(meter metric.Meter) (*SupervisorMetrics, error) {
	var err error
	if m.handoffModes, err = meter.Int64Counter("aether.supervisor.handoff_mode",
		metric.WithDescription("Takeovers from a live predecessor Envoy, by mode. hot is the hot restart; fresh_after_drain means the predecessor ran a different worker count (--concurrency), so it was drained and stopped and a fresh Envoy started instead, because a hot restart would reset about half of its live QUIC connections (#1136). Expected fresh_after_drain only on the rollout that changes proxy.concurrency")); err != nil {
		return nil, fmt.Errorf("handoff modes: %w", err)
	}
	for _, mode := range handoffModeValues {
		m.handoffModes.Add(context.Background(), 0, metric.WithAttributes(attrHandoffMode.String(mode)))
	}
	return m, nil
}

// handoffMode counts one takeover decision (see freshStartInsteadOfHotRestart).
func (m *SupervisorMetrics) handoffMode(mode string) {
	if m == nil {
		return
	}
	m.handoffModes.Add(context.Background(), 1, metric.WithAttributes(attrHandoffMode.String(mode)))
}

// adminMutation counts one state-changing admin request decision (see
// ownAdminRequest).
func (m *SupervisorMetrics) adminMutation(request string, owner adminOwnership) {
	if m == nil {
		return
	}
	m.adminMutations.Add(context.Background(), 1, metric.WithAttributes(
		attrAdminRequest.String(request), attrAdminOwner.String(string(owner))))
}

func (m *SupervisorMetrics) adminProbed(endpoint, result string) {
	if m == nil {
		return
	}
	m.adminProbes.Add(context.Background(), 1, metric.WithAttributes(
		attrProbeEndpoint.String(endpoint), attrProbeResult.String(result),
	))
}

// probeResult maps a probe's liveness answer onto its metric attribute value.
// Unreachable is reported by the caller, which alone can tell it apart.
func probeResult(live bool) string {
	if live {
		return probeResultLive
	}
	return probeResultNotLive
}
