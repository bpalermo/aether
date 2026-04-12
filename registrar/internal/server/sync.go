package server

import (
	"context"
	"time"

	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// Syncer periodically polls an external registry, computes a diff against the
// local snapshot, and broadcasts changes to all watching agents. It implements
// the controller-runtime Runnable interface.
type Syncer struct {
	registry     registry.Registry
	snapshot     *Snapshot
	broadcaster  *Broadcaster
	syncInterval time.Duration
	log          logr.Logger
	firstSync    bool
}

// NewSyncer creates a Syncer that polls the external registry at the given interval.
func NewSyncer(reg registry.Registry, snapshot *Snapshot, broadcaster *Broadcaster, syncInterval time.Duration, log logr.Logger) *Syncer {
	return &Syncer{
		registry:     reg,
		snapshot:     snapshot,
		broadcaster:  broadcaster,
		syncInterval: syncInterval,
		log:          log.WithName("syncer"),
		firstSync:    true,
	}
}

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

	endpoints, err := s.registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	if err != nil {
		s.log.Error(err, "failed to list endpoints from registry")
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

	if s.firstSync {
		s.log.Info("initial sync complete", "version", version, "endpoints", countEndpoints(newState))
		s.firstSync = false
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
