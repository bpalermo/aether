package hotrestart

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foreignIdentity is what another proxy pod's supervisor hands ITS Envoy: the
// same shape as ours, a different pod and nonce.
const foreignIdentity = "/var/run/aether-proxy/envoy-admin-address.aether-proxy-new.0123456789abcdef01234567"

// runShutdown drives handleShutdown to completion on the cancelled context its
// only call site hands it.
func runShutdown(t *testing.T, s *Supervisor, within time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(within):
		t.Fatal("handleShutdown did not return")
	}
}

// TestShutdownFallbackDoesNotDrainAForeignEnvoyAtTheSameEpoch is the issue
// #1127 regression, in the incident's exact shape.
//
// The new pod's successor crashed and its supervisor started a FRESH Envoy at
// epoch 0; that Envoy bound the node-shared admin address after ours had
// closed its admin. Our Envoy is also epoch 0, so the restart epoch cannot tell
// them apart: on main the old supervisor read "LIVE at our epoch", waited out
// its successor budget, and POSTed /drain_listeners?graceful to the NEW pod's
// Envoy — which then added no listeners for pods created on the node until the
// next handoff (2026-10-01, w01/w03, ~12 minutes).
func TestShutdownFallbackDoesNotDrainAForeignEnvoyAtTheSameEpoch(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	f.identity.Store(foreignIdentity) // another pod's fresh epoch-0 Envoy
	// 1s drain + 5s grace + 10s margin + 2s of successor wait.
	s.cfg.TerminationGrace = 18 * time.Second

	runShutdown(t, s, 20*time.Second)

	assert.Zero(t, f.drainHits.Load(),
		"the old pod's fallback drained ANOTHER pod's envoy on the shared admin address")
	assert.False(t, s.childTracked(0), "our own envoy must still be stopped and reaped")
}

// TestShutdownFallbackDoesNotDrainTheSuccessor: the mid-handoff arm (admin
// answers at a NEWER epoch, i.e. the successor's Envoy) whose wait expires.
// Whatever answers is the successor, never us; draining it is the same bug.
func TestShutdownFallbackDoesNotDrainTheSuccessor(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 1)
	s := newShutdownSupervisor(t, f)
	f.identity.Store(foreignIdentity)
	s.cfg.TerminationGrace = 18 * time.Second

	runShutdown(t, s, 20*time.Second)

	assert.Zero(t, f.drainHits.Load(), "the successor's envoy must not be drained by the old pod")
	assert.False(t, s.childTracked(0))
}

// TestDrainImmediatelyDoesNotDrainAForeignEnvoy: --shutdown-drain-immediately
// skips the wait, not the identity check.
func TestDrainImmediatelyDoesNotDrainAForeignEnvoy(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	f.identity.Store(foreignIdentity)
	s.cfg.ShutdownDrainImmediately = true

	runShutdown(t, s, 15*time.Second)

	assert.Zero(t, f.drainHits.Load())
	assert.False(t, s.childTracked(0))
}

// TestDrainSkippedOnceOurEnvoyIsGone: our Envoy has exited and been reaped, so
// whatever answers the admin address cannot be ours — even an answer that
// carries our identity (a stale echo) is not trusted without a tracked child.
func TestDrainSkippedOnceOurEnvoyIsGone(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	// Our own nonce, at our own epoch: still not trusted once the child is gone.
	// newShutdownSupervisor already answers as us.

	s.signalEpoch(0, syscall.SIGKILL)
	select {
	case exit := <-s.childExited:
		s.reap(exit.epoch)
	case <-time.After(5 * time.Second):
		t.Fatal("stub envoy did not exit")
	}
	require.False(t, s.anyChildTracked())

	s.drainThenShutdown(context.Background())
	assert.Zero(t, f.drainHits.Load(), "nothing of ours is left to drain")
}

// TestDrainListenersRidesTheVerifiedConnection is the positive control: our own
// Envoy answers, so the drain IS sent — and on the very connection whose
// /server_info named us, never on a fresh one that could reach another process.
func TestDrainListenersRidesTheVerifiedConnection(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f) // answers with s.adminIdentity

	require.True(t, s.drainListeners(context.Background()))
	assert.Equal(t, int64(1), f.drainHits.Load())
	assert.Equal(t, "graceful", f.drainQuery.Load())
	assert.Equal(t, [][]string{{"/server_info", "/drain_listeners"}}, f.connectionsServing("/drain_listeners"),
		"the drain must ride the identity-checked connection, immediately after the check")
}

// TestDrainSkippedForOurNonceAtAnUntrackedEpoch: the nonce matches but the
// epoch is not one of our tracked children — e.g. an in-pod epoch we already
// reaped. Both halves of the identity must hold.
func TestDrainSkippedForOurNonceAtAnUntrackedEpoch(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 5)
	s := newShutdownSupervisor(t, f)

	assert.False(t, s.drainListeners(context.Background()))
	assert.Zero(t, f.drainHits.Load())
}

// TestDrainNotRetriedOnAFreshConnection: our Envoy passes the identity check
// and then closes the connection. Sending the drain on a new connection could
// reach whichever Envoy holds the port by then, so it is not sent at all.
func TestDrainNotRetriedOnAFreshConnection(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	f.closeAfterServerInfo.Store(true)
	before := f.conns.Load()

	assert.False(t, s.drainListeners(context.Background()))
	assert.Zero(t, f.drainHits.Load())
	assert.Equal(t, int64(1), f.conns.Load()-before, "no second connection may be opened for the drain")
}

// TestDrainSkippedWhenAdminUnreachable: nothing answers the identity check.
func TestDrainSkippedWhenAdminUnreachable(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	f.srv.Close()

	assert.False(t, s.drainListeners(context.Background()))
	assert.Zero(t, f.drainHits.Load())
}

// TestEnvoyCarriesThisSupervisorsAdminIdentity pins the carrier: every fork
// passes --admin-address-path set to a per-supervisor nonce, in the pod-local
// ready-marker directory (the proxy's root filesystem is read-only), naming
// the pod for logs — and two supervisors, even of the same pod name, differ.
func TestEnvoyCarriesThisSupervisorsAdminIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{EnvoyPath: "/bin/true", ReadyMarkerPath: filepath.Join(dir, "ready"), PodName: "aether-proxy-abcde"}
	a := New(cfg, slog.New(slog.DiscardHandler), nil)
	b := New(cfg, slog.New(slog.DiscardHandler), nil)

	assert.NotEqual(t, a.adminIdentity, b.adminIdentity, "the identity must be unique per supervisor")
	assert.Equal(t, dir, filepath.Dir(a.adminIdentity))
	assert.True(t, strings.HasPrefix(filepath.Base(a.adminIdentity), adminIdentityPrefix+"aether-proxy-abcde."))

	args := a.buildEnvoyCmd(3).Args
	i := slices.Index(args, "--admin-address-path")
	require.GreaterOrEqual(t, i, 0, "envoy must be started with the identity carrier")
	assert.Equal(t, a.adminIdentity, args[i+1])
}

// TestRemoveStaleAdminIdentities: address files of earlier supervisor processes
// of this pod (container restarts keep the emptyDir) are cleaned up; ours stays.
func TestRemoveStaleAdminIdentities(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{ReadyMarkerPath: filepath.Join(dir, "ready")}, slog.New(slog.DiscardHandler), nil)
	stale := filepath.Join(dir, adminIdentityPrefix+"old")
	other := filepath.Join(dir, "ready")
	for _, p := range []string{stale, other, s.adminIdentity} {
		require.NoError(t, os.WriteFile(p, []byte("127.0.0.1:9901"), 0o600))
	}

	s.removeStaleAdminIdentities()

	assert.NoFileExists(t, stale)
	assert.FileExists(t, other)
	assert.FileExists(t, s.adminIdentity)
}
