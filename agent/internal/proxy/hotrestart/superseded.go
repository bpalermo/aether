package hotrestart

import (
	"context"
	"fmt"
	"net"
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
//   - successor_gone: nobody says anything and nothing is there. No process
//     holds a hot-restart socket of this base id (see envoyHoldsBaseID), the
//     heartbeat is stale and the admin is silent, all three for
//     supersededEmptyConfirm. Start a fresh Envoy at epoch 0. This is the
//     recovery for a successor that was killed outright, for one whose
//     supervisor predates the request file, and for one whose Envoy could not
//     be hot-restarted from.
//
// Neither may take a node from a successor that is healthy: that would leave
// the NEW pod superseded and NotReady, and the roll waiting on it for good.
//
// And a fork is never used to find out whether the node has an Envoy. An Envoy
// started at epoch 0 beside a live one is not harmless (envoyHoldsBaseID says
// why), so the fresh start requires that nothing holds the base id, and the
// hot restart is from the epoch that asked and no other. A relaunch that still
// dies at once is not retried through retryBindCollision, which re-runs epoch
// detection without those limits: the supervisor waits again, with a pause and
// a budget per recovery (supersededLosses). A spent budget never leaves an
// empty node without a proxy: it stops the answers to one asker, or hands the
// retries of a fresh start to the kubelet, and nothing else.

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

	// maxSupersededRelaunches bounds how many Envoys in a row a superseded
	// supervisor may start for one recovery and lose at once
	// (quickNonLiveExit); see supersededLosses for what happens then. Each
	// further attempt is preceded by bindCollisionRetryPause.
	maxSupersededRelaunches = 5

	// supersededEmptyConfirm is how long the node must have looked empty, on
	// every poll, before successor_gone starts a fresh Envoy. "Empty" already
	// includes a heartbeat nobody refreshed for predecessorStale, which is
	// what covers the moment between a supervisor's fork and its Envoy binding
	// the hot-restart sockets; this is on top of it. It is not the admin
	// watchdog's bound, which it was at first: once no process holds the base
	// id there is no Envoy whose silence could be waited out, only a node
	// without a proxy. Two fresh starts that still coincide are both epoch 0:
	// the second fails on the socket bind, before it touches anything.
	supersededEmptyConfirm = 3 * time.Second

	// Envoy's hot-restart domain sockets, at the pinned version
	// (source/server/hot_restarting_base.cc): the abstract address
	// "<socket path>_<role>_<base id * 10 + restart epoch % 3>", and every
	// Envoy binds both roles. The supervisor refuses --socket-path, so the
	// path is always Envoy's default.
	envoyHotRestartSocketPath = "@envoy_domain_socket"
	envoySunPathLen           = 108 // sizeof(sockaddr_un.sun_path) on Linux
	envoyMaxConcurrentEpochs  = 3
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

// envoyHoldsBaseID reports whether a process on the node holds one of the
// hot-restart domain sockets of this supervisor's base id, and which. That is
// an Envoy of this base id that has not exited: the kernel drops an abstract
// socket with its last descriptor, whatever state the process died in, and a
// stopped or wedged Envoy still holds its own. The pods share the host network
// namespace, so every proxy pod on the node sees the same sockets. Connecting
// a datagram socket sends nothing.
//
// It is what makes the fresh start of successor_gone safe. Envoy binds its two
// sockets BEFORE it touches the shared memory, and their names carry the
// restart epoch modulo 3 (at most three epochs coexist). So an Envoy started
// at epoch 0 fails on the bind, harmlessly, only against a live Envoy whose
// epoch is a multiple of 3. Against any other it binds, and because epoch 0
// means "first" it then unlinks the base id's shared memory and creates a new
// one (source/server/hot_restart_impl.cc, attachSharedMemory), sets the
// "initializing" flag in it, and dies on the admin address, which the live
// Envoy holds. The flag is never cleared: every later hot restart from the
// live Envoy maps the new segment and exits with "previous envoy process is
// still initializing", until that Envoy is gone. A bound socket of ANY of the
// three ids therefore forbids the fork.
func (s *Supervisor) envoyHoldsBaseID() (socket string, held bool) {
	for _, role := range []string{"parent", "child"} {
		for id := 0; id < envoyMaxConcurrentEpochs; id++ {
			name := fmt.Sprintf("%s_%s_%d", envoyHotRestartSocketPath, role, uint64(s.cfg.BaseID)*10+uint64(id))
			// Envoy binds with the length of the whole sockaddr_un, so the
			// abstract name is the text padded with NULs to all of sun_path.
			name += strings.Repeat("\x00", envoySunPathLen-len(name))
			conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
			if err == nil {
				_ = conn.Close()
				return name, true
			}
		}
	}
	return "", false
}

// advertiseWindow is how long a terminating supervisor that waits budget for a
// successor advertises for one (bounded false: for the whole wait, which then
// has no end and no drain after it).
//
// It stops predecessorStale + ParentShutdownTime before the budget ends. A pod
// that starts attaches to whatever the heartbeat names for predecessorStale
// after its last refresh, and a successor needs ParentShutdownTime from its
// workers' start to terminate its parent. Advertised up to the drain, the
// heartbeat sent a pod that started during the drain to hot-restart from an
// Envoy that was SIGTERMed under it mid-handoff.
func (s *Supervisor) advertiseWindow(budget time.Duration) (window time.Duration, bounded bool) {
	if budget <= 0 {
		return 0, false
	}
	return max(0, budget-(predecessorStale+s.cfg.ParentShutdownTime)), true
}

// advertiseForSuccessor runs while a terminating supervisor waits budget for a
// successor (awaitSuccessor). Every readyPollInterval of advertiseWindow in
// which the admin answers LIVE at our own epoch, it refreshes the node epoch
// heartbeat and the successor request; when the window ends it withdraws the
// request and writes nothing more. It returns the function that stops it.
//
// The heartbeat matters on its own: watchLiveness stops at the SIGTERM, so
// without this a replacement that starts more than predecessorStale later (a
// slow image pull) finds a stale heartbeat and starts at epoch 0 beside the
// Envoy it should have hot-restarted from.
//
// Nothing is written while the admin answers at another epoch: a successor is
// then already attached, and asking for a second one would start a race.
func (s *Supervisor) advertiseForSuccessor(ctx context.Context, budget time.Duration) (stop func()) {
	window, bounded := s.advertiseWindow(budget)
	if s.cfg.StateDir == "" || s.cfg.AdminAddress == "" || (bounded && window == 0) {
		return func() {}
	}
	epoch := s.currentEpoch()
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer s.clearSuccessorWanted(epoch)
		t := time.NewTicker(readyPollInterval)
		defer t.Stop()
		var end <-chan time.Time
		if bounded {
			timer := time.NewTimer(window)
			defer timer.Stop()
			end = timer.C
		}
		for {
			s.advertiseOnce(ctx, epoch)
			select {
			case <-quit:
				return
			case <-end:
				s.log.InfoContext(ctx, "no longer asking for a successor: one that attached now could not finish before the drain",
					"epoch", epoch, "advertisedFor", window.String(), "successorWaitBudget", budget.String())
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		close(quit)
		<-finished
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

// supersededLosses counts, for a superseded supervisor, the Envoys it started
// again and lost at once, in a row, per recovery. The two budgets end
// differently, and neither ends the watch for an empty node:
//
//   - answering one asker (successor_terminating): after
//     maxSupersededRelaunches the requests of THAT epoch are ignored. Its Envoy
//     cannot be hot-restarted from, and asking again will not change that.
//     Another epoch's request starts a new count, and successor_gone still
//     applies once the asker has drained and exited.
//   - a fresh start on an empty node (successor_gone): after
//     maxSupersededRelaunches the supervisor exits non-zero. An Envoy that
//     cannot start on an empty node is the pod crash it has always been, and
//     the kubelet's backoff is the pacing for it. The restarted container
//     starts as any does.
//
// Owned by the Run goroutine.
type supersededLosses struct {
	// pause: the last relaunch was lost; wait before the next.
	pause bool
	// gone counts fresh starts lost in a row.
	gone int
	// asked counts hot restarts from epoch askedBy lost in a row.
	asked   int
	askedBy int
}

func (l *supersededLosses) reset() { *l = supersededLosses{askedBy: -1} }

// note records a lost relaunch of the given recovery.
func (l *supersededLosses) note(trigger string, peer int) (attempt int) {
	l.pause = true
	if trigger == triggerSuccessorGone {
		l.gone++
		return l.gone
	}
	if peer != l.askedBy {
		l.asked, l.askedBy = 0, peer
	}
	l.asked++
	return l.asked
}

// mayAnswer reports whether a request from epoch may still be answered.
func (l *supersededLosses) mayAnswer(epoch int) bool {
	return epoch != l.askedBy || l.asked < maxSupersededRelaunches
}

// superseded is where a supervisor waits once a successor's hot-restart
// protocol has terminated its newest Envoy at lastEpoch, or once the Envoy it
// started again was lost at once (see supersededLosses). It returns done when
// the pod is deleted, which is how a completed roll ends, or after it has
// started an Envoy because the successor went away (see the top of this file).
// The values are handleChildExit's.
func (s *Supervisor) superseded(ctx context.Context, lastEpoch int) (retErr error, done bool, rearmReason string) {
	if s.supersededLost.gone >= maxSupersededRelaunches {
		return fmt.Errorf("%d fresh envoys in a row, started on a node with no envoy, exited at once: "+
			"exiting for a container restart", s.supersededLost.gone), true, ""
	}
	if s.supersededLost.pause {
		s.supersededLost.pause = false
		if !sleepCtx(ctx, bindCollisionRetryPause) {
			return nil, true, ""
		}
	}
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
	var emptySince time.Time
	for {
		select {
		case <-ctx.Done():
			return "", -1
		case <-t.C:
		}
		if epoch, ok := s.readSuccessorWanted(); ok && s.supersededLost.mayAnswer(epoch) && s.adminLiveAtEpoch(ctx, epoch) {
			return triggerSuccessorTerminating, epoch
		}
		if s.nodeHasAnEnvoy(ctx) {
			emptySince = time.Time{}
			continue
		}
		now := s.now()
		if emptySince.IsZero() {
			emptySince = now
		}
		if now.Sub(emptySince) >= supersededEmptyConfirm {
			return triggerSuccessorGone, -1
		}
	}
}

// nodeHasAnEnvoy reports whether anything suggests an Envoy is still running
// on the node: the shared admin answers (in any state, at any epoch), some
// supervisor confirmed its own LIVE within predecessorStale, or a process
// holds a hot-restart socket of the base id.
func (s *Supervisor) nodeHasAnEnvoy(ctx context.Context) bool {
	if _, reachable := s.adminServerInfo(ctx, s.currentEpoch()); reachable {
		return true
	}
	if _, heartbeat, ok := s.readState(); ok && time.Since(heartbeat) < predecessorStale {
		return true
	}
	_, held := s.envoyHoldsBaseID()
	return held
}

// relaunchSuperseded starts an Envoy again for a recovery awaitDeletion chose,
// or reports relaunched false when the node is no longer as that recovery
// needs it, and the wait resumes.
//
//   - successor_gone forks epoch 0, and only while nothing holds the base id.
//     It runs no epoch detection: there is no predecessor to detect, and
//     initStartEpoch would attach to (or, on a worker-count difference, drain
//     and stop) an Envoy that went LIVE in between.
//   - successor_terminating runs epoch detection restricted to the epoch that
//     asked (initStartEpochFrom), as a new pod would, and accepts two answers:
//     hot-restart from that epoch, or a fresh epoch 0 once nothing holds the
//     base id (the worker-count path drained and stopped the asker, or it
//     exited meanwhile).
func (s *Supervisor) relaunchSuperseded(ctx context.Context, lastEpoch int, trigger string, peer int) (relaunched bool, err error) {
	s.resetEpochForRetry()
	if trigger == triggerSuccessorTerminating {
		s.initStartEpochFrom(ctx, peer)
		if ctx.Err() != nil {
			return false, nil
		}
	} else {
		s.mu.Lock()
		s.gatedEpoch = -1
		s.handoffPeer = -1
		s.mu.Unlock()
		s.metrics.predecessorFound(false)
	}

	s.mu.Lock()
	next := s.nextEpoch
	s.mu.Unlock()
	// next is 0, or peer+1 from the restricted epoch detection: nothing else
	// can come out of the above.
	refused := ""
	if next == 0 {
		if socket, held := s.envoyHoldsBaseID(); held {
			refused = "an envoy holds " + strings.TrimRight(socket, "\x00") + ": a fresh epoch 0 beside it would replace its shared memory"
		}
	}
	if refused != "" {
		// Put the epoch back, so the liveness loop keeps asking about the
		// epoch this pod last ran and not about another pod's.
		s.mu.Lock()
		s.nextEpoch = lastEpoch + 1
		s.gatedEpoch = -1
		s.handoffPeer = -1
		s.mu.Unlock()
		s.log.InfoContext(ctx, "not starting envoy again; still awaiting pod deletion",
			"trigger", trigger, "reason", refused, "wouldStartAtEpoch", next)
		return false, nil
	}
	s.log.WarnContext(ctx, "the successor that terminated our envoy is going away and this pod was not deleted; starting envoy again",
		"trigger", trigger, "lastEpoch", lastEpoch, "startEpoch", next,
		"hotRestartFromEpoch", next-1)
	s.metrics.restartTriggered(trigger)
	if err := s.hotRestart(); err != nil {
		return false, fmt.Errorf("starting envoy again after its successor went away: %w", err)
	}
	s.mu.Lock()
	s.supersededRelaunchEpoch = s.nextEpoch - 1
	s.supersededFromEpoch = lastEpoch
	s.mu.Unlock()
	s.supersededRelaunchTrigger, s.supersededRelaunchPeer = trigger, peer
	return true, nil
}

// abandonSupersededRelaunch puts the epoch back to the one this supervisor ran
// before the relaunch it has just lost, drops that relaunch's readiness gate,
// and returns the epoch. Left at the lost epoch, the liveness loop would ask
// the node's admin about exactly the epoch the pod that won the race now runs,
// read LIVE, and mark this pod Ready with no Envoy in it.
func (s *Supervisor) abandonSupersededRelaunch() (lastEpoch int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextEpoch = s.supersededFromEpoch + 1
	s.supersededRelaunchEpoch = -1
	s.gatedEpoch = -1
	s.handoffPeer = -1
	s.epochLive = false
	return s.supersededFromEpoch
}

// lostSupersededRelaunch reports whether exit is the Envoy relaunchSuperseded
// started, dying before it was ever LIVE and within bindCollisionWindow,
// however it died. A fatal signal counts too: retryBindCollision would answer
// either with unrestricted epoch detection.
func (s *Supervisor) lostSupersededRelaunch(exit childExit) bool {
	s.mu.Lock()
	ours := exit.epoch == s.supersededRelaunchEpoch
	s.mu.Unlock()
	return ours && s.quickNonLiveExit()
}
