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
	predecessorDetected metric.Int64Gauge
	drainDuration       metric.Float64Histogram
	readyTransitions    metric.Int64Counter
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
	if m.predecessorDetected, err = meter.Int64Gauge("aether.supervisor.predecessor_detected",
		metric.WithDescription("1 if a live predecessor was found at startup (cross-pod hot restart), 0 for a fresh epoch-0 start")); err != nil {
		return nil, fmt.Errorf("predecessor detected: %w", err)
	}
	if m.drainDuration, err = meter.Float64Histogram("aether.supervisor.drain.duration",
		metric.WithDescription("Time from shutdown SIGTERM to the last supervised Envoy exiting"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120)); err != nil {
		return nil, fmt.Errorf("drain duration: %w", err)
	}
	if m.readyTransitions, err = meter.Int64Counter("aether.supervisor.ready_transitions",
		metric.WithDescription("Readiness marker transitions, by new state")); err != nil {
		return nil, fmt.Errorf("ready transitions: %w", err)
	}

	return m, nil
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
