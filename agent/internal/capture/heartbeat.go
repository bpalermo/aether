package capture

import (
	"context"
	"log/slog"
	"time"

	commonlog "github.com/bpalermo/aether/common/log"
)

// MeshDNSHeartbeatInterval is how often the last-known mesh-DNS record table is
// re-persisted with a fresh writtenAt stamp (issue #586).
//
// The writer is the capture reconciler, which is purely event-driven (the manager has
// no short SyncPeriod, so the default resync is 10h). A healthy but quiet cluster
// would therefore leave the snapshot untouched for hours and look indistinguishable —
// to the resolver daemon's snapshot-age gauge — from an agent that crashed, lost RBAC,
// or whose reconciler wedged. The heartbeat makes "fresh" mean "the agent is alive and
// projecting", so the daemon's age gauge is an actual liveness signal for the writer.
// It must stay well under any alert threshold on that gauge.
const MeshDNSHeartbeatInterval = 60 * time.Second

// MeshDNSSnapshotRewriter re-persists the last projected mesh-DNS record table. It is
// implemented by the xDS snapshot cache (the capture reconciler's sink).
type MeshDNSSnapshotRewriter interface {
	// RewriteMeshDNSSnapshot re-stamps the snapshot's freshness without changing its
	// records or generation. It is a no-op before the first projection and when mesh
	// DNS is disabled.
	RewriteMeshDNSSnapshot()
}

// MeshDNSHeartbeat is the controller-runtime Runnable that drives the mesh-DNS
// snapshot freshness heartbeat. The agent only registers it when --mesh-dns is on, and
// the rewrite is itself a no-op when the snapshot path is empty, so it is safe either
// way.
type MeshDNSHeartbeat struct {
	// Rewriter is the snapshot cache to re-stamp. Required.
	Rewriter MeshDNSSnapshotRewriter
	// Interval overrides MeshDNSHeartbeatInterval when positive (tests).
	Interval time.Duration
	// Log is the agent logger; a "mesh-dns-heartbeat" child is derived from it.
	Log *slog.Logger
}

// Start ticks until the manager's context is cancelled. It never returns an error: a
// missed heartbeat degrades a freshness signal, it never breaks resolution.
func (h *MeshDNSHeartbeat) Start(ctx context.Context) error {
	log := commonlog.Named(h.Log, "mesh-dns-heartbeat")
	interval := h.Interval
	if interval <= 0 {
		interval = MeshDNSHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.InfoContext(ctx, "mesh-DNS snapshot freshness heartbeat started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			h.Rewriter.RewriteMeshDNSSnapshot()
		}
	}
}

// NeedLeaderElection reports false: every node agent must re-stamp its OWN node's
// snapshot, so this runs on all agents, not just a leader.
func (h *MeshDNSHeartbeat) NeedLeaderElection() bool { return false }
