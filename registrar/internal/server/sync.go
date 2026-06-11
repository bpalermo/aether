package server

import (
	"context"
	"strconv"
	"time"

	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// tracerName identifies this instrumentation scope in trace backends.
const tracerName = "aether/registrar"

// Syncer periodically polls an external registry, computes a diff against the
// local snapshot, and broadcasts changes to all watching agents. It implements
// the controller-runtime Runnable interface.
type Syncer struct {
	registry     registry.Registry
	snapshot     *Snapshot
	broadcaster  *Broadcaster
	syncInterval time.Duration
	log          logr.Logger
	metrics      *Metrics
	firstSync    bool
	// synced is closed after the first successful sync cycle; RPCs that serve
	// snapshot state gate on it so a freshly restarted registrar never serves
	// an empty/partial view of the world (agents deriving route configs from
	// such a view 404 live traffic — observed on the rev-66 co-roll).
	synced chan struct{}
}

// NewSyncer creates a Syncer that polls the external registry at the given
// interval. metrics may be nil to disable instrumentation.
func NewSyncer(reg registry.Registry, snapshot *Snapshot, broadcaster *Broadcaster, syncInterval time.Duration, log logr.Logger, metrics *Metrics) *Syncer {
	return &Syncer{
		registry:     reg,
		snapshot:     snapshot,
		broadcaster:  broadcaster,
		syncInterval: syncInterval,
		log:          log.WithName("syncer"),
		metrics:      metrics,
		firstSync:    true,
		synced:       make(chan struct{}),
	}
}

// Synced returns a channel that is closed once the first sync cycle has
// completed and the snapshot reflects the external registry.
func (s *Syncer) Synced() <-chan struct{} { return s.synced }

// Start runs the sync loop until the context is cancelled.
func (s *Syncer) Start(ctx context.Context) error {
	s.log.Info("starting sync loop", "interval", s.syncInterval)

	// Perform an initial sync immediately.
	s.sync(ctx)

	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("sync loop stopped")
			return nil
		case <-ticker.C:
			s.sync(ctx)
		}
	}
}

// sync performs a single sync cycle: fetches all endpoints from the external
// registry, diffs against the snapshot, applies changes, and broadcasts deltas.
func (s *Syncer) sync(ctx context.Context) {
	s.log.V(1).Info("starting sync cycle")

	ctx, span := otel.Tracer(tracerName).Start(ctx, "registrar.sync_loop")
	var retErr error
	defer func() { telemetry.EndSpan(span, retErr) }()
	start := time.Now()

	endpoints, err := s.registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	if err != nil {
		s.log.Error(err, "failed to list endpoints from registry")
		s.metrics.syncFailed(ctx, time.Since(start).Seconds())
		retErr = err
		return
	}

	// Build the new state in the format the snapshot expects.
	newState := make(map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint)
	for svcName, eps := range endpoints {
		if newState[svcName] == nil {
			newState[svcName] = make(map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint)
		}
		newState[svcName][registryv1.Service_PROTOCOL_HTTP] = eps
	}

	// Compute diff and apply.
	events := s.snapshot.Diff(newState)
	version := s.snapshot.Replace(newState)
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
		s.log.Info("initial sync complete", "version", version, "endpoints", countEndpoints(newState))
		s.firstSync = false
		close(s.synced)
	} else if len(events) > 0 {
		s.log.V(1).Info("sync detected changes", "version", version, "events", len(events))
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
