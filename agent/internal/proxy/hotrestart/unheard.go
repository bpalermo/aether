package hotrestart

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// An Envoy without a heartbeat (issue #1680).
//
// The node epoch heartbeat says "a supervisor saw its Envoy LIVE within
// predecessorStale". Its absence does not say the node has no Envoy: the
// heartbeat is written by the supervisor, and a supervisor that is stalled (a
// starved node), or whose admin probes time out, stops writing it while its
// Envoy goes on serving. A new pod that read the stale heartbeat as "no
// predecessor" started its Envoy at restart epoch 0 beside the live one.
//
// That start is not the harmless collision it was taken for. Envoy binds its
// hot-restart domain sockets, named by restart epoch MODULO 3, before it
// attaches the shared memory (envoyHoldsBaseID). So an epoch 0 collides on the
// bind only with a live Envoy whose epoch is a multiple of 3. Beside any other
// it binds, replaces the base id's shared memory, sets the "initializing" flag
// in the new one, and dies on the admin address, flag still set. Every later
// hot restart from the healthy Envoy then exits with "previous envoy process is
// still initializing": the next roll's pod could never attach until the old pod
// was deleted by hand.
//
// So epoch 0 needs more than a stale heartbeat: no process may hold a
// hot-restart socket of the base id. While one does, the supervisor forks
// nothing and waits, for defaultUnheardEnvoyWait at most, for one of two
// things:
//
//   - the heartbeat is fresh again (the stalled supervisor runs again): the
//     ordinary path confirms the predecessor on the admin and hot-restarts
//     from it;
//   - the sockets are released (the Envoy exited): epoch 0 is safe.
//
// It does NOT attach to the Envoy that holds the base id on the admin's word
// alone, although the admin gives its epoch. A stale heartbeat is also how a
// terminating supervisor says "do not attach to my Envoy": it stops the
// heartbeat predecessorStale before it drains (advertiseWindow), and its Envoy
// answers LIVE until the SIGTERM that follows the drain. A pod that attached
// then had its hot-restart parent killed under it (measured in
// //agent/test/hotrestartrevert: the child looped on "hot restart sendmsg()
// connection refused" until the admin watchdog fired). The supervisor cannot
// tell that Envoy from a healthy one with a stalled supervisor, and waiting is
// right for both.

// defaultUnheardEnvoyWait bounds the wait for an Envoy that holds the base id
// and has no fresh heartbeat. It is the time the in-process bind-collision
// retries used to take before giving up (maxBindCollisionRetries attempts,
// bindCollisionRetryPause apart), which comfortably exceeds a predecessor
// pod's termination grace.
const defaultUnheardEnvoyWait = maxBindCollisionRetries * bindCollisionRetryPause

// unheardEnvoy is the state of one initStartEpoch call's wait on an Envoy
// without a heartbeat.
type unheardEnvoy struct {
	since time.Time // first seen holding the base id; zero: not seen
}

// heldHotRestartIDs returns which of the base id's hot-restart socket ids
// (restart epoch modulo envoyMaxConcurrentEpochs) are bound by a process on the
// node, in ascending order. See envoyHoldsBaseID for what a bound socket means.
func (s *Supervisor) heldHotRestartIDs() []int {
	var held []int
	for id := 0; id < envoyMaxConcurrentEpochs; id++ {
		if _, ok := s.hotRestartSocketHeld(id); ok {
			held = append(held, id)
		}
	}
	return held
}

// hotRestartSocketHeld reports whether a process holds a hot-restart domain
// socket of this base id with the given id, and its name. Connecting a datagram
// socket sends nothing.
func (s *Supervisor) hotRestartSocketHeld(id int) (socket string, held bool) {
	for _, role := range []string{"parent", "child"} {
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
	return "", false
}

// adminLiveEpoch returns the restart epoch of the Envoy that answers the
// node's admin, when it answers LIVE. Authoritative and unpooled, like every
// epoch-identity probe (newAdminClients).
func (s *Supervisor) adminLiveEpoch(ctx context.Context) (epoch int, live bool) {
	info, reachable, parsed := s.adminInfo(ctx)
	if !reachable {
		s.metrics.adminProbed(probeEndpointServerInfo, probeResultUnreachable)
		return 0, false
	}
	live = parsed && info.State == adminLiveState
	s.metrics.adminProbed(probeEndpointServerInfo, probeResult(live))
	return info.CommandLineOptions.RestartEpoch, live
}

// envoyWithoutHeartbeat is one step of initStartEpoch's decision when the
// heartbeat is stale or absent: it reports whether a process holds the base
// id, in which case nothing may be forked and the caller asks again. It
// returns an error once that has lasted s.unheardEnvoyWait.
func (s *Supervisor) envoyWithoutHeartbeat(ctx context.Context, st *unheardEnvoy) (held bool, err error) {
	if s.cfg.AdminAddress == "" {
		// Coordination-less unit-test supervisors only.
		return false, nil
	}
	ids := s.heldHotRestartIDs()
	if len(ids) == 0 {
		if !st.since.IsZero() {
			s.log.InfoContext(ctx, "the envoy that held this base id without a heartbeat has exited",
				"waited", time.Since(st.since).Round(time.Millisecond).String())
		}
		return false, nil
	}
	if st.since.IsZero() {
		st.since = time.Now()
		epoch, live := s.adminLiveEpoch(ctx)
		s.log.WarnContext(ctx, "the node epoch heartbeat is stale or absent, but an envoy holds this base id: "+
			"not starting at epoch 0 beside it (that would replace its hot-restart shared memory); "+
			"waiting for its supervisor's heartbeat, to hot-restart from it, or for it to exit",
			"baseID", s.cfg.BaseID, "heldSocketIDs", ids, "adminLive", live, "adminLiveEpoch", epoch,
			"waitBound", s.unheardEnvoyWait.String())
	}
	if waited := time.Since(st.since); waited > s.unheardEnvoyWait {
		return true, fmt.Errorf(
			"an envoy has held hot-restart socket id(s) %v of base id %d for %s and no supervisor reports it LIVE "+
				"(no fresh heartbeat): not starting an envoy beside it; if it is another proxy pod's, delete that pod",
			ids, s.cfg.BaseID, waited.Round(time.Second))
	}
	return true, nil
}
