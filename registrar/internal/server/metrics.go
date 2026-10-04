package server

import (
	"context"
	"fmt"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"aethermesh.dev/registry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// attrEventType labels endpoint-change events by their type (ADDED, UPDATED,
// REMOVED, FULL_SNAPSHOT). Bounded cardinality: one series per event type.
const attrEventType = attribute.Key("aether.event.type")

// eventTypeOptions holds the pre-built attribute set for every declared event
// type, indexed by the enum value. The broadcast fan-out is O(events ×
// watchers), so building metric.WithAttributes there would allocate on the
// hottest path in the registrar. Derived from the generated enum table so a new
// proto value is covered without touching this file.
var eventTypeOptions = buildEventTypeOptions()

func buildEventTypeOptions() []metric.MeasurementOption {
	maxValue := int32(0)
	for v := range registrarv1.WatchEndpointsResponse_EventType_name {
		if v > maxValue {
			maxValue = v
		}
	}
	opts := make([]metric.MeasurementOption, maxValue+1)
	for v, name := range registrarv1.WatchEndpointsResponse_EventType_name {
		opts[v] = metric.WithAttributes(attrEventType.String(name))
	}
	return opts
}

// eventTypeOption returns the cached attribute set for an event type, falling
// back to building one for a value outside the generated table (a proto enum
// carries unknown values through unchanged).
func eventTypeOption(t registrarv1.WatchEndpointsResponse_EventType) metric.MeasurementOption {
	if t >= 0 && int(t) < len(eventTypeOptions) && eventTypeOptions[t] != nil {
		return eventTypeOptions[t]
	}
	return metric.WithAttributes(attrEventType.String(t.String()))
}

// Metrics holds the registrar server's OTel instruments. All methods are
// nil-receiver-safe so the server runs unchanged when telemetry is disabled
// (pass a nil *Metrics).
//
// These instruments exist to make missed updates and stale state observable:
// a nonzero dropped-events counter means at least one agent was force-resynced
// (or, before PR4, silently diverged), and the store/snapshot revision gauges
// (ObserveSnapshot) against the agents' aether.agent.registry.last_version are
// the stored -> in-place -> applied lag queries (docs/runbook.md, #1193).
type Metrics struct {
	meter           metric.Meter
	watchers        metric.Int64UpDownCounter
	broadcastEvents metric.Int64Counter
	droppedEvents   metric.Int64Counter
	syncDuration    metric.Float64Histogram
	syncErrors      metric.Int64Counter
	syncEvents      metric.Int64Counter
	filteredSubs    metric.Int64Gauge
	snapshotVersion metric.Int64Gauge
	wbQueueDepth    metric.Int64Gauge
	wbFlushFailures metric.Int64Counter
	wbDrops         metric.Int64Counter
	wbShields       metric.Int64Counter
	watchStarts     metric.Int64Counter
}

// syncDurationBuckets are the explicit boundaries, in SECONDS, for
// aether.registrar.sync.duration. The OTel defaults are millisecond-oriented
// (0, 5, 10, ... 10000), so a seconds-valued sync duration would collapse into the
// "<= 5 s" first bucket and the quantiles would read flat (#732). A sync cycle runs
// every 5 s and is normally a few milliseconds of registry list plus diff.
var syncDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// NewMetrics registers the registrar server instruments on the given meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{meter: meter}
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
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(syncDurationBuckets...)); err != nil {
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
	if m.filteredSubs, err = meter.Int64Gauge("aether.registrar.watch.filtered_subscribers",
		metric.WithDescription("Watch streams carrying a service filter (demand-scoped agents)")); err != nil {
		return nil, fmt.Errorf("filtered subscribers: %w", err)
	}
	// The series name predates #1193, when it was a per-process counter bumped
	// on every sync; it is kept so existing queries keep resolving. It is now
	// the snapshot GENERATION: per process, and it moves only when the
	// snapshot's contents change. It is not comparable across replicas or with
	// agents -- aether.registrar.snapshot_revision is.
	if m.snapshotVersion, err = meter.Int64Gauge("aether.registrar.snapshot.version",
		metric.WithDescription("Snapshot generation: per-process count of content changes (changes only when the served endpoint set changes; not comparable across replicas -- use aether.registrar.snapshot_revision)")); err != nil {
		return nil, fmt.Errorf("snapshot version: %w", err)
	}
	if m.wbQueueDepth, err = meter.Int64Gauge("aether.registrar.writebehind.queue_depth",
		metric.WithDescription("Pending external-registry writes (snapshot-first ops not yet flushed and observed)")); err != nil {
		return nil, fmt.Errorf("writebehind queue depth: %w", err)
	}
	if m.wbFlushFailures, err = meter.Int64Counter("aether.registrar.writebehind.flush_failures",
		metric.WithDescription("External-registry write attempts that failed and were rescheduled")); err != nil {
		return nil, fmt.Errorf("writebehind flush failures: %w", err)
	}
	if m.wbDrops, err = meter.Int64Counter("aether.registrar.writebehind.drops",
		metric.WithDescription("Write-behind ops dropped after exceeding max age (agents' re-assertion/sweep repair these)")); err != nil {
		return nil, fmt.Errorf("writebehind drops: %w", err)
	}
	if m.wbShields, err = meter.Int64Counter("aether.registrar.writebehind.shielded_intents",
		metric.WithDescription("Pending intents overlaid onto a sync cycle's fetched state (each prevented a snapshot regression)")); err != nil {
		return nil, fmt.Errorf("writebehind shields: %w", err)
	}

	if m.watchStarts, err = meter.Int64Counter("aether.registrar.watch.starts",
		metric.WithDescription("Agent watch streams opened, by whether the full snapshot was sent (resume=resent) or the agent's last_version named the current contents (resume=current)")); err != nil {
		return nil, fmt.Errorf("watch starts: %w", err)
	}

	return m, nil
}

// attrResume labels a watch start by its resume outcome. Two values.
const attrResume = attribute.Key("resume")

var (
	resumeResent  = metric.WithAttributes(attrResume.String("resent"))
	resumeCurrent = metric.WithAttributes(attrResume.String("current"))
)

// watchStarted counts a watch start by whether it resent the snapshot.
func (m *Metrics) watchStarted(ctx context.Context, resent bool) {
	if m == nil {
		return
	}
	if resent {
		m.watchStarts.Add(ctx, 1, resumeResent)
		return
	}
	m.watchStarts.Add(ctx, 1, resumeCurrent)
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

func (m *Metrics) filteredWatchers(ctx context.Context, n int) {
	if m == nil {
		return
	}
	m.filteredSubs.Record(ctx, int64(n))
}

// eventsBroadcast records a whole fan-out batch: one Add per event type rather
// than one per enqueued event, so the totals are unchanged while the counter
// work leaves the per-watcher loop.
func (m *Metrics) eventsBroadcast(ctx context.Context, counts map[registrarv1.WatchEndpointsResponse_EventType]int64) {
	if m == nil {
		return
	}
	for eventType, n := range counts {
		m.broadcastEvents.Add(ctx, n, eventTypeOption(eventType))
	}
}

func (m *Metrics) eventDropped(ctx context.Context, eventType string) {
	if m == nil {
		return
	}
	m.droppedEvents.Add(ctx, 1, metric.WithAttributes(attrEventType.String(eventType)))
}

func (m *Metrics) syncCompleted(ctx context.Context, seconds float64, state State, eventsByType map[string]int) {
	if m == nil {
		return
	}
	m.syncDuration.Record(ctx, seconds)
	m.snapshotVersion.Record(ctx, int64(state.Generation))
	for eventType, n := range eventsByType {
		m.syncEvents.Add(ctx, int64(n), metric.WithAttributes(attrEventType.String(eventType)))
	}
}

// snapshotState records the snapshot generation after an RPC write path
// (RegisterEndpoint/UnregisterEndpoint) changed it outside the sync loop.
func (m *Metrics) snapshotState(ctx context.Context, state State) {
	if m == nil {
		return
	}
	m.snapshotVersion.Record(ctx, int64(state.Generation))
}

// attrContentHash labels the snapshot-content info gauge with the hash of the
// endpoint set this replica serves.
const attrContentHashLabel = attribute.Key("content_hash")

// ObserveSnapshot registers the asynchronous snapshot-identity instruments
// (#1193), read from snap (and store, when the backend has revisions) at each
// collection:
//
//   - aether.registrar.store_revision: the newest store revision this replica
//     has seen (its etcd watch or listing headers). etcd only.
//   - aether.registrar.snapshot_revision: the store revision of the listing
//     this replica's snapshot was last installed from. etcd only.
//   - aether.registrar.snapshot.content: an info gauge, always 1, labelled
//     content_hash. It is observed (not recorded) so exactly ONE series per
//     replica is exported at a time: an asynchronous instrument reports only
//     the attribute sets of its latest callback, so a superseded hash stops
//     being exported instead of accumulating. Two replicas reporting the same
//     snapshot_revision with different content_hash (and no write-behind
//     intent pending) diverged: that is a bug.
//
// A nil receiver or nil snap registers nothing; store may be nil (backend
// without revisions), in which case the revision gauges are not reported.
func (m *Metrics) ObserveSnapshot(snap *Snapshot, store registry.RevisionedLister) error {
	if m == nil || snap == nil {
		return nil
	}
	storeRev, err := m.meter.Int64ObservableGauge("aether.registrar.store_revision",
		metric.WithDescription("Newest store revision this registrar replica has seen (etcd header revision; etcd backend only). store_revision - snapshot_revision is this replica's lag"))
	if err != nil {
		return fmt.Errorf("store revision: %w", err)
	}
	snapRev, err := m.meter.Int64ObservableGauge("aether.registrar.snapshot_revision",
		metric.WithDescription("Store revision of the listing this replica serves (etcd backend only); compare with store_revision (replica lag) and aether.agent.registry.last_version (fleet propagation lag)"))
	if err != nil {
		return fmt.Errorf("snapshot revision: %w", err)
	}
	content, err := m.meter.Int64ObservableGauge("aether.registrar.snapshot.content",
		metric.WithDescription("Always 1; the content_hash label names the endpoint set this replica serves (one series per replica). Same snapshot_revision with different content_hash across replicas means divergence"))
	if err != nil {
		return fmt.Errorf("snapshot content: %w", err)
	}
	_, err = m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		state := snap.State()
		o.ObserveInt64(content, 1, metric.WithAttributes(attrContentHashLabel.String(state.ContentHash)))
		if state.Revision > 0 {
			o.ObserveInt64(snapRev, state.Revision)
		}
		if store != nil {
			if rev := store.StoreRevision(); rev > 0 {
				o.ObserveInt64(storeRev, rev)
			}
		}
		return nil
	}, storeRev, snapRev, content)
	if err != nil {
		return fmt.Errorf("register snapshot callback: %w", err)
	}
	return nil
}

func (m *Metrics) syncFailed(ctx context.Context, seconds float64) {
	if m == nil {
		return
	}
	m.syncDuration.Record(ctx, seconds)
	m.syncErrors.Add(ctx, 1)
}

func (m *Metrics) wbDepth(ctx context.Context, depth int) {
	if m == nil {
		return
	}
	m.wbQueueDepth.Record(ctx, int64(depth))
}

func (m *Metrics) wbFlushFailed(ctx context.Context) {
	if m == nil {
		return
	}
	m.wbFlushFailures.Add(ctx, 1)
}

func (m *Metrics) wbDropped(ctx context.Context) {
	if m == nil {
		return
	}
	m.wbDrops.Add(ctx, 1)
}

func (m *Metrics) wbShielded(ctx context.Context, n int) {
	if m == nil {
		return
	}
	m.wbShields.Add(ctx, int64(n))
}
