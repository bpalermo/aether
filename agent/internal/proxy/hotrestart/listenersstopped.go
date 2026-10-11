package hotrestart

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// "LIVE on the admin" is not "serving" (issue #1679).
//
// An Envoy asked to drain its listeners (POST /drain_listeners) closes them
// and stays as it was in every other respect: /server_info and /ready go on
// answering LIVE. Measured on the pinned Envoy with a graceful drain and
// --drain-time-s 3: the listener accepted for 3 s, then refused every
// connection, and the one thing that changed on the admin was the counter
// listener_manager.listener_stopped, 0 before and 1 after.
//
// The only thing that drains another pod's Envoy is a supervisor replacing it
// for a worker-count change (#1136), which then stops it. When that supervisor
// dies in between (killed outright, or an older one terminated mid-way, which
// did not finish what it started), the drained Envoy stays: LIVE, serving
// nothing, its pod Ready, the node without a data plane until the pod was
// deleted by hand.
//
// So the liveness loop also asks its own Envoy whether it has stopped its
// listeners, and when it has:
//
//   - the pod is not Ready, at once: it does not serve;
//   - if the Envoy is still there, still this supervisor's and still the one
//     that answers the admin listenersStoppedGrace later, nobody is replacing
//     it, and the supervisor hot-restarts it in place (restart_triggers
//     "listeners_stopped"). Measured: an Envoy forked at the next epoch from a
//     drained parent binds the listeners again and serves within half a
//     second.
//
// What is compared is an INCREASE of the counter in one Envoy process. Its
// absolute value says nothing: a hot-restart child inherits its parent's
// counters, and every parent stops its listeners when its child's workers
// start, so every Envoy above epoch 0 has a non-zero one. The baseline is
// therefore taken only once the Envoy is this supervisor's only one (no parent
// of its own left to merge counters from) and its pod has been Ready (a
// cross-pod parent is gone by then, see liveAnchoredReadyGate).
//
// The counter is read on a fresh connection, after /server_info on that same
// connection has shown the answering Envoy to be this supervisor's at its
// newest epoch. A pooled connection would not do: a hot-restart parent keeps
// answering on connections it has already accepted, and such a parent has
// stopped its listeners because its child serves.

const (
	// adminListenersStoppedPath selects the one counter. The filter is a
	// regular expression; the name is matched exactly when the answer is read.
	adminListenersStoppedPath = "/stats?filter=listener_manager.listener_stopped"
	adminListenersStoppedStat = "listener_manager.listener_stopped"
	adminStatsBodyLimit       = 4096

	// listenersStoppedGrace is how long an Envoy that stopped its listeners is
	// left to whoever drained it. A supervisor replacing it for a worker-count
	// change stops it within shutdownProbeTimeout of the listeners closing
	// (both happen one drain time after the request).
	listenersStoppedGrace = shutdownGrace
	// listenersStoppedRetrigger re-arms the hot restart while the Envoy still
	// serves nothing: a restart that was skipped (a bootstrap that fails
	// validation) is asked for again.
	listenersStoppedRetrigger = 30 * time.Second

	triggerListenersStopped = "listeners_stopped"
)

// listenerWatch is the liveness loop's view of whether this supervisor's
// newest Envoy has stopped its listeners. Owned by the watchLiveness goroutine.
type listenerWatch struct {
	s        *Supervisor
	interval time.Duration

	epoch        int // the epoch baseline was read from; -1: none
	baseline     int64
	lastCheck    time.Time
	stoppedSince time.Time // zero: not stopped
	triggeredAt  time.Time // zero: no restart asked for yet
}

func newListenerWatch(s *Supervisor) *listenerWatch {
	return &listenerWatch{s: s, interval: s.adminReverifyInterval(), epoch: -1}
}

func (w *listenerWatch) reset() {
	w.epoch, w.baseline = -1, 0
	w.stoppedSince, w.triggeredAt = time.Time{}, time.Time{}
}

// stopped is called on every tick on which the admin answered LIVE for epoch,
// and reports whether that Envoy has stopped its listeners. ready is whether
// the pod is Ready now.
func (w *listenerWatch) stopped(ctx context.Context, epoch int, ready bool) bool {
	s := w.s
	if !s.soleChild(epoch) {
		w.reset()
		return false
	}
	if w.epoch != epoch {
		w.reset()
	}
	now := s.now()
	suspect := !w.stoppedSince.IsZero()
	// One request per re-verify interval while all is well, one per tick once
	// the listeners are known to be stopped.
	if !suspect && now.Sub(w.lastCheck) < w.interval {
		return false
	}
	w.lastCheck = now
	count, ok := s.ownListenersStopped(ctx, epoch)
	switch {
	case !ok:
		// Not ours on the admin just now, or no answer. The liveness probe
		// decides what that means; what was known stays known.
		return suspect
	case w.epoch != epoch:
		if ready {
			w.epoch, w.baseline = epoch, count
		}
		return false
	case count <= w.baseline:
		return false
	}
	if !suspect {
		w.stoppedSince = now
		s.log.WarnContext(ctx, "this pod's envoy is LIVE on its admin but has stopped its listeners "+
			"(another supervisor asked it to drain, to replace it for a worker-count change); it serves nothing",
			"epoch", epoch, "listenerStopped", count, "baseline", w.baseline,
			"hotRestartIn", listenersStoppedGrace.String())
		return true
	}
	w.restartIfLeft(ctx, epoch, now)
	return true
}

// restartIfLeft asks for the in-place hot restart once the Envoy has been left
// with its listeners stopped for listenersStoppedGrace, and again every
// listenersStoppedRetrigger for as long as it is.
func (w *listenerWatch) restartIfLeft(ctx context.Context, epoch int, now time.Time) {
	if now.Sub(w.stoppedSince) < listenersStoppedGrace {
		return
	}
	if !w.triggeredAt.IsZero() && now.Sub(w.triggeredAt) < listenersStoppedRetrigger {
		return
	}
	w.triggeredAt = now
	w.s.log.WarnContext(ctx, "nobody replaced the envoy whose listeners were stopped; hot-restarting it in place "+
		"(the next epoch binds the listeners again)",
		"epoch", epoch, "trigger", triggerListenersStopped,
		"stoppedFor", now.Sub(w.stoppedSince).Round(time.Millisecond).String())
	select {
	case w.s.selfTrigger <- triggerListenersStopped:
	default: // one is already pending.
	}
}

// soleChild reports whether the Envoy at epoch is the only one this supervisor
// tracks.
func (s *Supervisor) soleChild(epoch int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.children[epoch]
	return ok && len(s.children) == 1
}

// ownListenersStopped reads listener_manager.listener_stopped from this
// supervisor's Envoy at epoch. ok is false when the Envoy answering the admin
// is not that one (or not LIVE), or the counter could not be read.
func (s *Supervisor) ownListenersStopped(ctx context.Context, epoch int) (count int64, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, readyPollInterval)
	defer cancel()
	conn, err := dialAdmin(ctx, s.cfg.AdminAddress)
	if err != nil {
		return 0, false
	}
	defer func() { _ = conn.Close() }()

	code, raw, err := conn.do(ctx, http.MethodGet, "/server_info", adminServerInfoBodyLimit)
	var info adminServerInfoDoc
	if err != nil || code != http.StatusOK || json.Unmarshal(raw, &info) != nil {
		return 0, false
	}
	if info.State != adminLiveState || info.CommandLineOptions.RestartEpoch != epoch || !s.ownsAdminAnswer(info) {
		return 0, false
	}
	code, raw, err = conn.do(ctx, http.MethodGet, adminListenersStoppedPath, adminStatsBodyLimit)
	if err != nil || code != http.StatusOK {
		return 0, false
	}
	return parseStat(raw, adminListenersStoppedStat)
}

// parseStat finds "<name>: <value>" in Envoy's plain-text /stats output.
func parseStat(body []byte, name string) (value int64, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		key, val, found := strings.Cut(sc.Text(), ":")
		if !found || strings.TrimSpace(key) != name {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}
