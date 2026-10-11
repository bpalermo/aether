package hotrestart

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aethermesh.dev/common/file"
)

// A superseded supervisor, and how it gets the node back.
//
// A cross-pod handoff ends with the successor pod's Envoy terminating ours
// over the hot-restart protocol. From then on this pod serves nothing: it is
// NotReady and waits for the DaemonSet to delete it, which happens seconds
// later on a roll that completes.
//
// A roll does not always complete. When the DaemonSet's template goes back to
// this pod's revision while the surge pod is starting (a `helm rollback`, or
// an upgrade followed at once by its revert), the DaemonSet deletes the SURGE
// pod and keeps this one. The surge pod's Envoy has usually taken the node
// over by then: it is a fraction of a second from its fork to its workers. So
// the pod the DaemonSet deleted holds the node's only Envoy, and the pod it
// kept has none. The deleted pod waits successorWaitBudget for a replacement
// the DaemonSet will never create (this pod IS the current revision), drains,
// and exits. The node then had no proxy at all, and this pod stayed NotReady
// until something replaced it (nightly e2e, 2026-10-11).
//
// Two things end that state, both decided here by the superseded supervisor
// and never by the one that is terminating:
//
//   - successor_terminating: the terminating supervisor says so. While it
//     waits for a successor it keeps the node epoch heartbeat fresh and writes
//     successorWantedFileName beside it (advertiseForSuccessor). A superseded
//     supervisor that reads a fresh one, and finds the admin LIVE at the epoch
//     it names, hot-restarts from that Envoy exactly as a new pod would: the
//     node never goes without a proxy.
//   - successor_gone: nobody says anything and nothing answers. The admin has
//     been unreachable for AdminUnresponsiveDeadline and the heartbeat is
//     stale, so no supervisor on the node has an Envoy it would still be
//     keeping (a wedged one is killed by its own supervisor within that same
//     bound). Start a fresh Envoy at epoch 0. This is the recovery for a
//     successor that was killed outright, and for one whose supervisor
//     predates the advertisement.
//
// Neither may take a node from a successor that is healthy: that would leave
// the NEW pod superseded and NotReady, and the roll waiting on it for good. So
// a fork that this file started and that dies at once (another Envoy holds the
// base-id socket after all) is not retried through retryBindCollision, which
// would attach to whoever is live by then; the supervisor goes back to waiting.

const (
	// successorWantedFileName is the file, beside the epoch heartbeat in
	// StateDir, in which a terminating supervisor whose Envoy is still LIVE
	// asks for a successor: "<epoch> <unixMillis>", rewritten every
	// readyPollInterval and read as stale after predecessorStale, like the
	// heartbeat. A supervisor that does not know the file ignores it, and one
	// that does not write it is recovered from by triggerSuccessorGone.
	successorWantedFileName = "successor-wanted"

	// The restart_triggers values of the two recoveries.
	triggerSuccessorTerminating = "successor_terminating"
	triggerSuccessorGone        = "successor_gone"
)

func (s *Supervisor) successorWantedPath() string {
	return filepath.Join(s.cfg.StateDir, successorWantedFileName)
}

// readSuccessorWanted returns the epoch a terminating supervisor asks a
// successor for, when the request is fresh.
func (s *Supervisor) readSuccessorWanted() (epoch int, ok bool) {
	b, err := os.ReadFile(s.successorWantedPath())
	if err != nil {
		return 0, false
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, false
	}
	e, err1 := strconv.Atoi(parts[0])
	ms, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || time.Since(time.UnixMilli(ms)) >= predecessorStale {
		return 0, false
	}
	return e, true
}

func (s *Supervisor) writeSuccessorWanted(epoch int) {
	data := fmt.Sprintf("%d %d\n", epoch, time.Now().UnixMilli())
	if err := file.AtomicWrite(s.successorWantedPath(), []byte(data), 0o644); err != nil {
		s.log.Error("writing the successor request", "error", err)
	}
}

// clearSuccessorWanted removes the request if it is still this epoch's.
func (s *Supervisor) clearSuccessorWanted(epoch int) {
	if cur, ok := s.readSuccessorWanted(); ok && cur == epoch {
		_ = os.Remove(s.successorWantedPath())
	}
}

// advertiseForSuccessor runs while a terminating supervisor waits for a
// successor (awaitSuccessor). Every readyPollInterval in which the admin
// answers LIVE at our own epoch it refreshes the node epoch heartbeat and the
// successor request. It returns the function that stops it and withdraws the
// request; the caller must call it before draining.
//
// The heartbeat matters on its own: watchLiveness stops at the SIGTERM, so
// without this a replacement that starts more than predecessorStale later (a
// slow image pull) finds a stale heartbeat, starts at epoch 0 and collides
// with the Envoy it should have hot-restarted from.
//
// Nothing is written while the admin answers at another epoch: a successor is
// then already attached, and asking for a second one would start a race.
func (s *Supervisor) advertiseForSuccessor(ctx context.Context) (stop func()) {
	if s.cfg.StateDir == "" || s.cfg.AdminAddress == "" {
		return func() {}
	}
	epoch := s.currentEpoch()
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(readyPollInterval)
		defer t.Stop()
		for {
			s.advertiseOnce(ctx, epoch)
			select {
			case <-quit:
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		close(quit)
		<-finished
		s.clearSuccessorWanted(epoch)
	}
}

// advertiseOnce is one tick of advertiseForSuccessor. ctx is the supervisor's
// already cancelled context, so the probe runs detached from it (see
// handleShutdown).
func (s *Supervisor) advertiseOnce(ctx context.Context, epoch int) {
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownProbeTimeout)
	defer cancel()
	if !s.adminLiveAtEpoch(probeCtx, epoch) {
		return
	}
	s.writeState(epoch)
	s.writeSuccessorWanted(epoch)
}

// superseded is where a supervisor waits once a successor's hot-restart
// protocol has terminated its newest Envoy at lastEpoch. It returns done when
// the pod is deleted, which is how a completed roll ends, or after it has
// started an Envoy again because the successor went away (see the top of this
// file). The values are handleChildExit's.
func (s *Supervisor) superseded(ctx context.Context, lastEpoch int) (retErr error, done bool, rearmReason string) {
	for {
		trigger, peer := s.awaitDeletion(ctx)
		if trigger == "" {
			return nil, true, ""
		}
		relaunched, err := s.relaunchSuperseded(ctx, lastEpoch, trigger, peer)
		if err != nil {
			return err, true, ""
		}
		if relaunched {
			return nil, false, ""
		}
		if ctx.Err() != nil {
			return nil, true, ""
		}
	}
}

// awaitDeletion blocks until the pod is deleted (trigger "") or one of the two
// recoveries applies. peer is the epoch to hot-restart from for
// triggerSuccessorTerminating.
//
// Without a state directory or an admin address there is no node to observe
// (coordination-less unit-test supervisors only): it waits for the deletion
// alone, as the supervisor always did.
func (s *Supervisor) awaitDeletion(ctx context.Context) (trigger string, peer int) {
	if s.cfg.StateDir == "" || s.cfg.AdminAddress == "" {
		<-ctx.Done()
		return "", -1
	}
	t := time.NewTicker(readyPollInterval)
	defer t.Stop()
	var silentSince time.Time
	for {
		select {
		case <-ctx.Done():
			return "", -1
		case <-t.C:
		}
		if epoch, ok := s.readSuccessorWanted(); ok && s.adminLiveAtEpoch(ctx, epoch) {
			return triggerSuccessorTerminating, epoch
		}
		if s.nodeHasAnEnvoy(ctx) {
			silentSince = time.Time{}
			continue
		}
		now := s.now()
		if silentSince.IsZero() {
			silentSince = now
		}
		if now.Sub(silentSince) >= s.adminUnresponsiveDeadline() {
			return triggerSuccessorGone, -1
		}
	}
}

// nodeHasAnEnvoy reports whether anything suggests an Envoy is still running
// on the node: the shared admin answers (in any state, at any epoch), or some
// supervisor confirmed its own LIVE within predecessorStale.
func (s *Supervisor) nodeHasAnEnvoy(ctx context.Context) bool {
	if _, reachable := s.adminServerInfo(ctx, s.currentEpoch()); reachable {
		return true
	}
	_, heartbeat, ok := s.readState()
	return ok && time.Since(heartbeat) < predecessorStale
}

// relaunchSuperseded starts an Envoy again for a recovery awaitDeletion chose.
// Epoch selection is initStartEpoch's, as for any new supervisor, and its
// answer is then checked against what the recovery is allowed to do: start
// fresh at epoch 0, or (successor_terminating only) hot-restart from the epoch
// that asked. Anything else means another supervisor's Envoy went LIVE in
// between; attaching to it would take the node from a healthy pod, so the
// epoch is put back and the wait resumes (relaunched false).
func (s *Supervisor) relaunchSuperseded(ctx context.Context, lastEpoch int, trigger string, peer int) (relaunched bool, err error) {
	s.resetEpochForRetry()
	s.initStartEpoch(ctx)
	if ctx.Err() != nil {
		return false, nil
	}
	s.mu.Lock()
	next := s.nextEpoch
	allowed := next == 0 || (trigger == triggerSuccessorTerminating && next == peer+1)
	if !allowed {
		s.nextEpoch = lastEpoch + 1
		s.gatedEpoch = -1
		s.handoffPeer = -1
	}
	s.mu.Unlock()
	if !allowed {
		s.log.InfoContext(ctx, "another envoy went live on the node while this one was about to start again; still awaiting pod deletion",
			"trigger", trigger, "liveEpoch", next-1)
		return false, nil
	}

	s.log.WarnContext(ctx, "the successor that terminated our envoy is going away and this pod was not deleted; starting envoy again",
		"trigger", trigger, "lastEpoch", lastEpoch, "startEpoch", next,
		"hotRestartFromEpoch", next-1, "adminUnresponsiveDeadline", s.adminUnresponsiveDeadline().String())
	s.metrics.restartTriggered(trigger)
	if err := s.hotRestart(); err != nil {
		return false, fmt.Errorf("starting envoy again after its successor went away: %w", err)
	}
	s.mu.Lock()
	s.supersededRelaunchEpoch = s.nextEpoch - 1
	s.mu.Unlock()
	return true, nil
}

// lostSupersededRelaunch reports whether exit is the Envoy relaunchSuperseded
// started, dying at once without a crash signal: the base-id socket is held,
// so the node has an Envoy after all.
func (s *Supervisor) lostSupersededRelaunch(exit childExit) bool {
	s.mu.Lock()
	ours := exit.epoch == s.supersededRelaunchEpoch
	s.mu.Unlock()
	return ours && s.quickNonLiveExit() && !isCrashSignal(exit.err)
}
