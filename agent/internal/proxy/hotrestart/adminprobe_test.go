package hotrestart

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// newProbeSupervisor builds a supervisor wired to the given fake admin, with no
// children and no telemetry — enough for the probe helpers.
func newProbeSupervisor(t *testing.T, f *fakeAdminServer) *Supervisor {
	t.Helper()
	s := New(Config{AdminAddress: f.addr(), StateDir: t.TempDir()}, slog.New(slog.DiscardHandler), nil)
	// Release the pinned connection before httptest.Server.Close asserts that
	// no connection is outstanding.
	t.Cleanup(s.adminFast.CloseIdleConnections)
	return s
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
