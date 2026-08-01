package server

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
)

// tracerName identifies this instrumentation scope in trace backends.
const tracerName = "aether/registrar"

// syncedProtocols are the registry protocols the syncer reflects into the
// snapshot each cycle. HTTP services ride the HCM path; TCP services ride the
// transparent-capture TCP floor. Each cycle lists every protocol so both flow
// through the snapshot, the agent watch stream, and name resolution.
var syncedProtocols = []registryv1.Service_Protocol{
	registryv1.Service_PROTOCOL_HTTP,
	registryv1.Service_PROTOCOL_TCP,
}

// Syncer periodically polls an external registry, computes a diff against the
// local snapshot, and broadcasts changes to all watching agents. It implements
// the controller-runtime Runnable interface.
type Syncer struct {
	registry     registry.Registry
	snapshot     *Snapshot
	broadcaster  *Broadcaster
	syncInterval time.Duration
	log          *slog.Logger
	metrics      *Metrics
	firstSync    bool
	// writeBehind, when set, overlays still-pending snapshot-first intents
	// onto the fetched external state before Diff/Replace, so the sync never
	// regresses an intent the external registry has not materialized yet.
	writeBehind *WriteBehindQueue
	// synced is closed after the first successful sync cycle; RPCs that serve
	// snapshot state gate on it so a freshly restarted registrar never serves
	// an empty/partial view of the world (agents deriving route configs from
	// such a view 404 live traffic — observed on the rev-66 co-roll).
	synced chan struct{}
}

// NewSyncer creates a Syncer that polls the external registry at the given
// interval. metrics may be nil to disable instrumentation.
func NewSyncer(reg registry.Registry, snapshot *Snapshot, broadcaster *Broadcaster, syncInterval time.Duration, log *slog.Logger, metrics *Metrics) *Syncer {
	return &Syncer{
		registry:     reg,
		snapshot:     snapshot,
		broadcaster:  broadcaster,
		syncInterval: syncInterval,
		log:          commonlog.Named(log, "syncer"),
		metrics:      metrics,
		firstSync:    true,
		synced:       make(chan struct{}),
	}
}

// UseWriteBehind wires the write-behind queue's pending-intent overlay into
// the sync cycle.
func (s *Syncer) UseWriteBehind(q *WriteBehindQueue) { s.writeBehind = q }

// Synced returns a channel that is closed once the first sync cycle has
// completed and the snapshot reflects the external registry.
func (s *Syncer) Synced() <-chan struct{} { return s.synced }

// changeDebounce coalesces a burst of registry change signals (e.g. an etcd
// watch firing per key during a roll) into a single sync, while still
// reacting within a fraction of the poll interval.
const changeDebounce = 200 * time.Millisecond

// Start runs the sync loop until the context is cancelled. When the registry
// supports change notifications (registry.ChangeNotifier — the etcd backend's
// clientv3 watch), the loop syncs at watch speed (debounced) instead of only
// at the poll interval; the periodic poll remains a backstop for any event
// missed during a watch re-establish. Backends without notifications (e.g.
// DynamoDB) fall back to poll-only, unchanged.
func (s *Syncer) Start(ctx context.Context) error {
	s.log.InfoContext(ctx, "starting sync loop", "interval", s.syncInterval)

	// Perform an initial sync immediately.
	s.sync(ctx)

	var changes <-chan struct{}
	if n, ok := s.registry.(registry.ChangeNotifier); ok {
		changes = n.Changes()
		s.log.InfoContext(ctx, "registry supports change notifications; syncing at watch speed (poll is backstop)")
	}

	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()

	// Debounce timer for change-driven syncs, created stopped.
	debounce := time.NewTimer(changeDebounce)
	if !debounce.Stop() {
		<-debounce.C
	}

	for {
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "sync loop stopped")
			return nil
		case <-ticker.C:
			s.sync(ctx)
		case <-changes:
			// (Re)arm the debounce window so a burst of watch events triggers
			// a single sync once they settle.
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(changeDebounce)
		case <-debounce.C:
			s.sync(ctx)
		}
	}
}

// sync performs a single sync cycle: fetches all endpoints from the external
// registry, diffs against the snapshot, applies changes, and broadcasts deltas.
func (s *Syncer) sync(ctx context.Context) {
	s.log.DebugContext(ctx, "starting sync cycle")

	ctx, span := otel.Tracer(tracerName).Start(ctx, "registrar.sync_loop")
	var retErr error
	defer func() { telemetry.EndSpan(span, retErr) }()
	start := time.Now()

	// Build the new state in the format the snapshot expects, listing every
	// protocol so non-HTTP (TCP) services flow through the snapshot, watch
	// stream, and name resolution alongside HTTP.
	newState := make(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint)
	for _, protocol := range syncedProtocols {
		endpoints, err := s.registry.ListAllEndpoints(ctx, protocol)
		if err != nil {
			s.log.ErrorContext(ctx, "failed to list endpoints from registry", "protocol", protocol.String(), "error", err)
			s.metrics.syncFailed(ctx, time.Since(start).Seconds())
			retErr = err
			return
		}
		for svcName, eps := range endpoints {
			if newState[svcName] == nil {
				newState[svcName] = make(map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint)
			}
			newState[svcName][protocol] = eps
		}
	}

	// Reconcile pending write-behind intents: release the observed ones,
	// overlay the rest so neither the diff events nor the replacement
	// snapshot regress them toward the external registry's stale view.
	if s.writeBehind != nil {
		s.writeBehind.Overlay(newState)
	}

	// Compute diff and apply.
	events := s.snapshot.Diff(newState)
	version, transitions := s.snapshot.Replace(newState)
	events = append(events, transitions...)
	span.SetAttributes(
		attribute.Int("aether.sync.events", len(events)),
		telemetry.AttrSnapshotVersion.String(version),
	)

	eventsByType := make(map[string]int)
	for _, event := range events {
		eventsByType[event.GetType().String()]++
	}
	versionNum, _ := strconv.ParseInt(version, 10, 64)
	s.metrics.syncCompleted(ctx, time.Since(start).Seconds(), versionNum, eventsByType)

	if s.firstSync {
		s.log.InfoContext(ctx, "initial sync complete", "version", version, "endpoints", countEndpoints(newState))
		s.firstSync = false
		close(s.synced)
	} else if len(events) > 0 {
		s.log.DebugContext(ctx, "sync detected changes", "version", version, "events", len(events))
	}

	// Stamp version on events and broadcast.
	if len(events) > 0 {
		for _, event := range events {
			event.Version = version
		}
		s.broadcaster.Broadcast(events)
	}
}

// NeedLeaderElection returns false so the syncer runs on all replicas,
// not just the leader. Each replica independently polls the external
// registry and broadcasts changes to its connected agents.
func (s *Syncer) NeedLeaderElection() bool { return false }

// countEndpoints counts total endpoints across all services and protocols.
func countEndpoints(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) int {
	count := 0
	for _, protocols := range state {
		for _, eps := range protocols {
			count += len(eps)
		}
	}
	return count
}
