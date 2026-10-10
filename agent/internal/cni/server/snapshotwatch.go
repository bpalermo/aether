package server

import (
	"context"
	"errors"
	"log/slog"

	"aethermesh.dev/agent/internal/xds/cache"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
)

// podSnapshots is what this server asks of the agent's xDS snapshot cache
// (*cache.SnapshotCache). An interface so a test can make a build return an
// error the real cache returns only after waiting out its bound.
type podSnapshots interface {
	AddPod(ctx context.Context, pod *cniv1.CNIPod, trustDomain string) error
	RemovePod(ctx context.Context, netns string) error
	SetNodeLocality(region, zone string)
}

// The callers of the snapshot cache in this package: the values of
// aether.agent.cni.snapshot_watch_unanswered's caller attribute.
const (
	snapshotCallerCNIAdd     = "cni_add"
	snapshotCallerCNIDel     = "cni_del"
	snapshotCallerTakeover   = "takeover"
	snapshotCallerGhostSweep = "ghost_sweep"
)

// snapshotWatchUnansweredMsg is the WARN a caller logs for a snapshot change
// that was installed while an open watch was not answered from it.
const snapshotWatchUnansweredMsg = "pod's snapshot change is installed, but an open watch was not answered from it; not a failure, a later snapshot build sends the proxy the change"

// podLog is log with the pod's name and namespace, as the RPC handlers carry.
func podLog(log *slog.Logger, pod *cniv1.CNIPod) *slog.Logger {
	return log.With("pod", pod.GetName(), "namespace", pod.GetNamespace())
}

// snapshotInstalled is what every caller of the snapshot cache in this package
// passes a mutator's error through (#1620). cache.ErrWatchNotAnswered means the
// snapshot holding the change IS installed and is the one every later request
// is answered from; only handing it to a watch that was open did not finish
// within the cache's bound, and a later build sends the proxy the change. That
// is not a failed change: it is logged at WARN, counted by caller, and nil is
// returned. Every other error is returned as it is: the snapshot was not built.
//
// Per caller, what a returned error does, and so what this spares:
//   - CNI ADD: fails the RPC, and the runtime tears down and retries a sandbox
//     whose listeners are published.
//   - CNI DEL and the ghost sweep: nothing but an ERROR line saying the
//     listener was not removed, which is untrue.
//   - takeover: fails the takeover step in the log, for listeners that are
//     installed.
func (s *CNIServer) snapshotInstalled(ctx context.Context, log *slog.Logger, caller string, err error) error {
	if !errors.Is(err, cache.ErrWatchNotAnswered) {
		return err
	}
	log.WarnContext(ctx, snapshotWatchUnansweredMsg, "caller", caller, "error", err)
	s.metrics.snapshotWatchUnanswered(ctx, caller)
	return nil
}
