package hotrestart

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProbeSupervisor builds a supervisor wired to the given fake admin, with no
// children and no telemetry — enough for the probe helpers.
func newProbeSupervisor(t *testing.T, f *fakeAdminServer) *Supervisor {
	t.Helper()
	s := New(Config{
		AdminAddress:       f.addr(),
		StateDir:           t.TempDir(),
		ParentShutdownTime: 15 * time.Second, // the deployed value; derives a 5s re-verify interval
	}, slog.New(slog.DiscardHandler), nil)
	// Release the pinned connection before httptest.Server.Close asserts that
	// no connection is outstanding.
	t.Cleanup(s.adminFast.CloseIdleConnections)
	return s
}

// verifiedProber returns a prober that has just confirmed epoch 0 LIVE through
// /server_info, with a tracked child for it — i.e. sitting exactly on the fast
// path. The supervisor's newest epoch is 0.
func verifiedProber(t *testing.T, f *fakeAdminServer) (*Supervisor, *adminProber) {
	t.Helper()
	s := newProbeSupervisor(t, f)
	s.children[0] = &exec.Cmd{}
	s.nextEpoch = 1

	p := newAdminProber(s)
	live, reachable := p.probe(context.Background(), 0)
	require.True(t, live, "first probe must confirm LIVE at epoch 0")
	require.True(t, reachable)
	require.Equal(t, int64(1), f.serverInfoHits.Load(), "verification must go through /server_info")
	require.Zero(t, f.readyHits.Load(), "an unverified epoch must never be probed via /ready")
	return s, p
}

// TestAdminProbeUsesReadyOnceEpochVerified is the point of #646: after one
// authoritative confirmation, the per-second watchdog costs a plain-text /ready.
func TestAdminProbeUsesReadyOnceEpochVerified(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	_, p := verifiedProber(t, f)

	for range 5 {
		live, reachable := p.probe(context.Background(), 0)
		assert.True(t, live)
		assert.True(t, reachable)
	}
	assert.Equal(t, int64(1), f.serverInfoHits.Load(), "the verified epoch must not re-probe /server_info")
	assert.Equal(t, int64(5), f.readyHits.Load())
}

// TestAdminProbeReverifiesOnInterval checks the fast path expires: identity is
// re-confirmed at least every derived re-verify interval (see
// adminReverifyIntervalFor).
func TestAdminProbeReverifiesOnInterval(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	_, p := verifiedProber(t, f)

	p.verifiedAt = time.Now().Add(-p.reverify - time.Second)
	live, reachable := p.probe(context.Background(), 0)

	assert.True(t, live)
	assert.True(t, reachable)
	assert.Equal(t, int64(2), f.serverInfoHits.Load(), "an expired verification must go authoritative")
	assert.Zero(t, f.readyHits.Load())
}

// TestAdminProbeReverificationCatchesSuccessorTakeover is why the fast path
// expires at all: a cross-pod successor answering LIVE at ITS epoch is
// indistinguishable from us on /ready, and only /server_info sees the epoch.
func TestAdminProbeReverificationCatchesSuccessorTakeover(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	_, p := verifiedProber(t, f)

	p.verifiedAt = time.Now().Add(-p.reverify - time.Second)
	f.set("LIVE", 1) // a successor took over the shared admin port

	live, reachable := p.probe(context.Background(), 0)
	assert.False(t, live, "LIVE at another epoch is not LIVE at ours")
	assert.True(t, reachable)
}

// TestAdminProbeFallsBackWhenChildGone covers the child exiting under a still
// valid verification: with nothing of ours left to be LIVE, /ready's answer is
// meaningless and the probe must go authoritative.
func TestAdminProbeFallsBackWhenChildGone(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	s, p := verifiedProber(t, f)

	s.reap(0)
	_, _ = p.probe(context.Background(), 0)

	assert.Equal(t, int64(2), f.serverInfoHits.Load())
	assert.Zero(t, f.readyHits.Load())
}

// TestAdminProbeFallsBackOnNewEpoch covers a hot restart: the new epoch is
// unverified, so the watchdog is back on /server_info until it goes LIVE.
func TestAdminProbeFallsBackOnNewEpoch(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	s, p := verifiedProber(t, f)

	s.children[1] = &exec.Cmd{}
	s.nextEpoch = 2
	live, reachable := p.probe(context.Background(), 1)

	assert.False(t, live, "the admin still answers at epoch 0")
	assert.True(t, reachable)
	assert.Equal(t, int64(2), f.serverInfoHits.Load())
	assert.Zero(t, f.readyHits.Load())
}

// TestAdminProbeNotLiveInvalidatesFastPath: a non-LIVE /ready is ambiguous (is
// it ours draining, or a successor initializing?), so it drops the fast path
// and the next tick resolves it authoritatively.
func TestAdminProbeNotLiveInvalidatesFastPath(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	_, p := verifiedProber(t, f)

	f.set("DRAINING", 0)
	live, reachable := p.probe(context.Background(), 0)
	assert.False(t, live)
	assert.True(t, reachable, "a 503 from /ready is still an answer")
	assert.Equal(t, int64(1), f.readyHits.Load())

	_, _ = p.probe(context.Background(), 0)
	assert.Equal(t, int64(2), f.serverInfoHits.Load(), "the next tick must re-verify")
	assert.Equal(t, int64(1), f.readyHits.Load())
}

// TestAdminProbeUnreachableInvalidates: an admin that stops answering must
// surface as not-reachable (the admin watchdog keys off it) and leave the
// prober unverified.
func TestAdminProbeUnreachableInvalidates(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	_, p := verifiedProber(t, f)

	f.srv.Close()
	live, reachable := p.probe(context.Background(), 0)

	assert.False(t, live)
	assert.False(t, reachable)
	assert.Equal(t, epochUnverified, p.verifiedEpoch)
}

// TestAdminProbeReusesConnection is the saving, measured: the verified steady
// state pins ONE connection however many ticks pass, while the authoritative
// probe keeps dialing fresh (the invariant of newAdminClients).
func TestAdminProbeReusesConnection(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	s, p := verifiedProber(t, f)

	before := f.conns.Load()
	for range 10 {
		live, _ := p.probe(context.Background(), 0)
		require.True(t, live)
	}
	assert.Equal(t, int64(1), f.conns.Load()-before, "the fast path must pin a single connection")

	before = f.conns.Load()
	for range 5 {
		_, _ = s.adminServerInfo(context.Background(), 0)
	}
	assert.Equal(t, int64(5), f.conns.Load()-before, "the authoritative probe must never pool")
}

// TestAdminProbeInvalidateDropsPinnedConnection: invalidation is not just a
// flag — the pinned connection goes with it, so no later answer can come off a
// connection accepted by a since-superseded process.
func TestAdminProbeInvalidateDropsPinnedConnection(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	s, p := verifiedProber(t, f)

	require.True(t, mustProbeLive(t, p))
	pinned := f.conns.Load()
	require.True(t, mustProbeLive(t, p))
	require.Equal(t, pinned, f.conns.Load(), "the second fast probe must reuse the pinned connection")

	p.invalidate()
	live, _ := s.adminReady(context.Background())

	assert.True(t, live)
	assert.Equal(t, pinned+1, f.conns.Load(), "invalidate must drop the pinned connection")
}

func mustProbeLive(t *testing.T, p *adminProber) bool {
	t.Helper()
	live, _ := p.probe(context.Background(), 0)
	return live
}

// TestAdminWatchdogFiresWhenAdminHangsAfterLive runs the real supervisor
// through the fast path and then wedges the admin (accepts, never answers) —
// the post-LIVE failure mode of proposal 001. It guards against a /ready probe
// that succeeds too easily: if the cheap path could not observe the wedge, the
// watchdog would never fire and the node's data plane would stay silently dead.
func TestAdminWatchdogFiresWhenAdminHangsAfterLive(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available in this sandbox")
	}

	configPath := filepath.Join(t.TempDir(), "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	marker := filepath.Join(t.TempDir(), "ready")
	f := newFakeAdmin(t, "LIVE", 0)

	s := New(Config{
		EnvoyPath:                 stubEnvoy(t, filepath.Join(t.TempDir(), "epochs.txt")),
		ConfigPath:                configPath,
		DrainTime:                 time.Second,
		ParentShutdownTime:        time.Second,
		StateDir:                  t.TempDir(),
		ReadyMarkerPath:           marker,
		AdminAddress:              f.addr(),
		AdminUnresponsiveDeadline: 1 * time.Second,
	}, slog.New(slog.DiscardHandler), nil)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.Background()) }()

	// Ready means epoch 0 was confirmed LIVE, so the watchdog is on /ready.
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "pod never became ready")
	require.Eventually(t, func() bool {
		return f.readyHits.Load() > 0
	}, 5*time.Second, 50*time.Millisecond, "fast path never engaged")

	f.hang.Store(true)

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "admin watchdog")
	case <-time.After(15 * time.Second):
		t.Fatal("admin watchdog did not fire")
	}
}

// TestAdminReadyMatchesServerInfoState pins the equivalence the fast path rests
// on: for every Envoy server state, /ready's liveness answer is the same as
// /server_info's at the matching epoch. Envoy computes both from the same
// main-thread Utility::serverState(...) call; if a future Envoy diverges (e.g.
// /ready answering 200 while not LIVE), this test is the tripwire.
func TestAdminReadyMatchesServerInfoState(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"LIVE", "DRAINING", "PRE_INITIALIZING", "INITIALIZING"} {
		t.Run(state, func(t *testing.T) {
			f := newFakeAdmin(t, state, 0)
			s := newProbeSupervisor(t, f)

			readyLive, readyReachable := s.adminReady(ctx)
			infoLive, infoReachable := s.adminServerInfo(ctx, 0)

			assert.Equal(t, infoLive, readyLive, "/ready liveness must match /server_info at the same epoch")
			assert.Equal(t, infoReachable, readyReachable, "both probes must agree the admin answered")
			assert.Equal(t, state == adminLiveState, readyLive)
		})
	}
}

// TestAdminServerInfoNeverPoolsConnection is the structural half of the
// correctness invariant: the epoch-identity probe must dial fresh every time,
// so a connection pinned to a superseded (draining, still answering LIVE at its
// old epoch) process can never answer it. See newAdminClients.
func TestAdminServerInfoNeverPoolsConnection(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	s := newProbeSupervisor(t, f)

	before := f.conns.Load()
	for range 5 {
		live, reachable := s.adminServerInfo(context.Background(), 0)
		assert.True(t, live)
		assert.True(t, reachable)
	}
	assert.Equal(t, int64(5), f.conns.Load()-before, "each /server_info probe must dial a fresh connection")
}

// TestAdminServerInfoUnreachable covers the wedged-admin shape the watchdog
// keys off: the admin accepts but never answers, so the probe times out and
// reports not-reachable rather than merely not-live.
func TestAdminServerInfoUnreachable(t *testing.T) {
	f := newFakeAdmin(t, "LIVE", 0)
	f.hang.Store(true)
	s := newProbeSupervisor(t, f)

	live, reachable := s.adminServerInfo(context.Background(), 0)
	assert.False(t, live)
	assert.False(t, reachable)
}
