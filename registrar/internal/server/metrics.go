package server

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// attrEventType labels endpoint-change events by their type (ADDED, UPDATED,
// REMOVED, FULL_SNAPSHOT). Bounded cardinality: one series per event type.
const attrEventType = attribute.Key("aether.event.type")

// Metrics holds the registrar server's OTel instruments. All methods are
// nil-receiver-safe so the server runs unchanged when telemetry is disabled
// (pass a nil *Metrics).
//
// These instruments exist to make missed updates and stale state observable:
// a nonzero dropped-events counter means at least one agent was force-resynced
// (or, before PR4, silently diverged), and the snapshot-version gauge is the
// registrar half of the registrar-vs-agent version-skew query.
type Metrics struct {
	watchers        metric.Int64UpDownCounter
	broadcastEvents metric.Int64Counter
	droppedEvents   metric.Int64Counter
	syncDuration    metric.Float64Histogram
	syncErrors      metric.Int64Counter
	syncEvents      metric.Int64Counter
	snapshotVersion metric.Int64Gauge
}

// NewMetrics registers the registrar server instruments on the given meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
	var err error

	if m.watchers, err = meter.Int64UpDownCounter("aether.registrar.watchers",
		metric.WithDescription("Agent watch streams currently subscribed to the broadcaster")); err != nil {
		return nil, fmt.Errorf("watchers: %w", err)
	}
	if m.broadcastEvents, err = meter.Int64Counter("aether.registrar.broadcast.events",
		metric.WithDescription("Endpoint events enqueued to agent watch streams")); err != nil {
		return nil, fmt.Errorf("broadcast events: %w", err)
	}
	if m.droppedEvents, err = meter.Int64Counter("aether.registrar.broadcast.dropped_events",
		metric.WithDescription("Endpoint events dropped because an agent watch stream was too slow")); err != nil {
		return nil, fmt.Errorf("dropped events: %w", err)
	}
	if m.syncDuration, err = meter.Float64Histogram("aether.registrar.sync.duration",
		metric.WithDescription("Duration of a registry sync cycle"),
		metric.WithUnit("s")); err != nil {
		return nil, fmt.Errorf("sync duration: %w", err)
	}
	if m.syncErrors, err = meter.Int64Counter("aether.registrar.sync.errors",
		metric.WithDescription("Registry sync cycles that failed (snapshot left stale until the next tick)")); err != nil {
		return nil, fmt.Errorf("sync errors: %w", err)
	}
	if m.syncEvents, err = meter.Int64Counter("aether.registrar.sync.events",
		metric.WithDescription("Endpoint change events detected by the sync loop, by event type")); err != nil {
		return nil, fmt.Errorf("sync events: %w", err)
	}
	if m.snapshotVersion, err = meter.Int64Gauge("aether.registrar.snapshot.version",
		metric.WithDescription("Current endpoint snapshot version (compare with aether.agent.registry.last_version for skew)")); err != nil {
		return nil, fmt.Errorf("snapshot version: %w", err)
	}

	return m, nil
}

func (m *Metrics) watcherSubscribed(ctx context.Context) {
	if m == nil {
		return
	}
	m.watchers.Add(ctx, 1)
}

func (m *Metrics) watcherUnsubscribed(ctx context.Context) {
	if m == nil {
		return
	}
	m.watchers.Add(ctx, -1)
}

func (m *Metrics) eventBroadcast(ctx context.Context, eventType string) {
	if m == nil {
		return
	}
	m.broadcastEvents.Add(ctx, 1, metric.WithAttributes(attrEventType.String(eventType)))
}

func (m *Metrics) eventDropped(ctx context.Context, eventType string) {
	if m == nil {
		return
	}
	m.droppedEvents.Add(ctx, 1, metric.WithAttributes(attrEventType.String(eventType)))
}

func (m *Metrics) syncCompleted(ctx context.Context, seconds float64, version int64, eventsByType map[string]int) {
	if m == nil {
		return
	}
	m.syncDuration.Record(ctx, seconds)
	m.snapshotVersion.Record(ctx, version)
	for eventType, n := range eventsByType {
		m.syncEvents.Add(ctx, int64(n), metric.WithAttributes(attrEventType.String(eventType)))
	}
}

// versionAdvanced records the snapshot version after an RPC write path
// (RegisterEndpoint/UnregisterEndpoint) bumped it outside the sync loop.
func (m *Metrics) versionAdvanced(ctx context.Context, version string) {
	if m == nil {
		return
	}
	if v, err := strconv.ParseInt(version, 10, 64); err == nil {
		m.snapshotVersion.Record(ctx, v)
	}
}

func (m *Metrics) syncFailed(ctx context.Context, seconds float64) {
	if m == nil {
		return
	}
	m.syncDuration.Record(ctx, seconds)
	m.syncErrors.Add(ctx, 1)
}
