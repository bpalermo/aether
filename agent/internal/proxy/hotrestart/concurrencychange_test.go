package hotrestart

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// predecessorIdentity is the --admin-address-path another proxy pod's
// supervisor gave its Envoy (issue #1127): what a cross-pod predecessor
// reports, and never this supervisor's own.
const predecessorIdentity = "/var/run/aether-proxy/envoy-admin-address.aether-proxy-old.00112233445566778899aabb"

const predecessorEpoch = 3

// newPredecessor is a fake node admin answered by a LIVE cross-pod predecessor
// at predecessorEpoch running the given worker count.
func newPredecessor(t *testing.T, concurrency int) *fakeAdminServer {
	t.Helper()
	f := newFakeAdmin(t, adminLiveState, predecessorEpoch)
	f.identity.Store(predecessorIdentity)
	f.concurrency.Store(int64(concurrency))
	return f
}

// newSuccessorSupervisor is the surge successor pod's supervisor: a fresh
// heartbeat names the predecessor's epoch, and its own Envoy would run with
// successorConcurrency workers (0 = no --concurrency flag).
func newSuccessorSupervisor(t *testing.T, f *fakeAdminServer, successorConcurrency int) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		StateDir:        dir,
		ReadyMarkerPath: filepath.Join(dir, "ready"),
		AdminAddress:    f.addr(),
		DrainTime:       50 * time.Millisecond,
		ExtraArgs:       []string{"-l", "info", "--service-node", "n1"},
	}
	if successorConcurrency > 0 {
		cfg.ExtraArgs = append(cfg.ExtraArgs, "--concurrency", strconv.Itoa(successorConcurrency))
	}
	s := New(cfg, slog.New(slog.DiscardHandler), nil)
	s.cpus = &fakeCPUs{onlineErr: errors.New("the CPU sources must not be consulted with an explicit --concurrency")}
	writeRawState(t, s, predecessorEpoch, 0)
	return s
}

func initStartEpochWithin(t *testing.T, s *Supervisor, within time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	s.initStartEpoch(ctx)
	require.NoError(t, ctx.Err(), "initStartEpoch did not return within %s", within)
}

// assertHotRestart: the supervisor attaches at predecessor+1, gated as before,
// and never touched the predecessor.
func assertHotRestart(t *testing.T, s *Supervisor, f *fakeAdminServer) {
	t.Helper()
	assert.Equal(t, predecessorEpoch+1, s.nextEpoch, "must hot-restart at the predecessor's epoch + 1")
	assert.Equal(t, predecessorEpoch+1, s.gatedEpoch, "the cross-pod readiness gate must be armed as before")
	assert.Zero(t, f.drainHits.Load(), "a hot restart must not drain the predecessor")
	assert.Zero(t, f.quitHits.Load(), "a hot restart must not stop the predecessor")
	assert.False(t, s.readyRequiresOwnIdentity)
}

// assertFreshAfterDrain: the predecessor was drained gracefully on the
// connection that was identity-checked, then stopped, and the supervisor
// starts a new, ungated lineage at epoch 0.
func assertFreshAfterDrain(t *testing.T, s *Supervisor, f *fakeAdminServer) {
	t.Helper()
	assert.Equal(t, 0, s.nextEpoch, "a worker-count change must start a fresh lineage at epoch 0")
	assert.Equal(t, -1, s.gatedEpoch, "a fresh start has no predecessor to gate on")
	assert.Equal(t, -1, s.handoffPeer)
	assert.EqualValues(t, 1, f.drainHits.Load(), "the predecessor must be drained exactly once")
	assert.Equal(t, "graceful", f.drainQuery.Load(), "the drain must be the graceful one")
	assert.EqualValues(t, 1, f.quitHits.Load(), "the drained predecessor must be stopped")
	assert.Contains(t, f.connectionsServing("/drain_listeners"), []string{"/server_info", "/drain_listeners"},
		"the drain must ride the connection whose /server_info was checked")
	assert.Contains(t, f.connectionsServing("/quitquitquit"), []string{"/server_info", "/quitquitquit"},
		"the stop must ride a connection that re-checked the predecessor's identity")
	assert.True(t, s.readyRequiresOwnIdentity, "readiness must require our own identity in the new lineage")
}

// (a) Counts equal: the hot restart is unchanged.
func TestConcurrencyEqualHotRestarts(t *testing.T) {
	f := newPredecessor(t, 2)
	s := newSuccessorSupervisor(t, f, 2)
	initStartEpochWithin(t, s, 10*time.Second)
	assertHotRestart(t, s, f)
}

// (b) Parent 4 workers, successor 2: drain, stop, fresh start.
func TestConcurrencyDecreaseDrainsThenStartsFresh(t *testing.T) {
	f := newPredecessor(t, 4)
	s := newSuccessorSupervisor(t, f, 2)
	initStartEpochWithin(t, s, 10*time.Second)
	assertFreshAfterDrain(t, s, f)
}

// (c) Parent 2 workers, successor 4: the mirror case, same outcome.
func TestConcurrencyIncreaseDrainsThenStartsFresh(t *testing.T) {
	f := newPredecessor(t, 2)
	s := newSuccessorSupervisor(t, f, 4)
	initStartEpochWithin(t, s, 10*time.Second)
	assertFreshAfterDrain(t, s, f)
}

// Without --concurrency the successor runs Envoy's default, the node's online
// CPU count, which is what the predecessor reports when it had no flag either.
func TestConcurrencyDefaultComparesOnlineCPUs(t *testing.T) {
	t.Run("default equals the predecessor's count", func(t *testing.T) {
		f := newPredecessor(t, 4)
		s := newSuccessorSupervisor(t, f, 0)
		s.cpus = &fakeCPUs{online: 4}
		initStartEpochWithin(t, s, 10*time.Second)
		assertHotRestart(t, s, f)
	})
	t.Run("default differs from the predecessor's explicit count", func(t *testing.T) {
		f := newPredecessor(t, 2)
		s := newSuccessorSupervisor(t, f, 0)
		s.cpus = &fakeCPUs{online: 4}
		initStartEpochWithin(t, s, 10*time.Second)
		assertFreshAfterDrain(t, s, f)
	})
	t.Run("default unknown", func(t *testing.T) {
		f := newPredecessor(t, 2)
		s := newSuccessorSupervisor(t, f, 0)
		s.cpus = &fakeCPUs{onlineErr: errors.New("no sysfs")}
		initStartEpochWithin(t, s, 10*time.Second)
		assertHotRestart(t, s, f)
	})
}

// (d) An answer that cannot be trusted or read never blocks a normal roll: the
// supervisor hot-restarts and leaves the predecessor alone.
func TestConcurrencyCheckInconclusiveHotRestarts(t *testing.T) {
	cases := map[string]func(f *fakeAdminServer){
		"no admin identity (non-aether or pre-#1127 envoy)": func(f *fakeAdminServer) { f.identity.Store("") },
		"identity that is not an aether supervisor's":       func(f *fakeAdminServer) { f.identity.Store("/tmp/admin-address") },
		"no concurrency reported":                           func(f *fakeAdminServer) { f.concurrency.Store(0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPredecessor(t, 4)
			mutate(f)
			s := newSuccessorSupervisor(t, f, 2)
			initStartEpochWithin(t, s, 10*time.Second)
			assertHotRestart(t, s, f)
		})
	}
}

// The answer is read only from the predecessor at the heartbeat's epoch, LIVE.
func TestReadPredecessorRejectsAnotherEpochOrState(t *testing.T) {
	f := newPredecessor(t, 4)
	ctx := context.Background()

	conn, err := dialAdmin(ctx, f.addr())
	require.NoError(t, err)
	_, why := readPredecessor(ctx, conn, predecessorEpoch+1)
	assert.Contains(t, why, "epoch")
	_ = conn.Close()

	f.set("DRAINING", predecessorEpoch)
	conn, err = dialAdmin(ctx, f.addr())
	require.NoError(t, err)
	_, why = readPredecessor(ctx, conn, predecessorEpoch)
	assert.Contains(t, why, "not LIVE")
	_ = conn.Close()

	f.set(adminLiveState, predecessorEpoch)
	conn, err = dialAdmin(ctx, f.addr())
	require.NoError(t, err)
	info, why := readPredecessor(ctx, conn, predecessorEpoch)
	assert.Empty(t, why)
	assert.Equal(t, predecessorInfo{identity: predecessorIdentity, epoch: predecessorEpoch, concurrency: 4}, info)
	_ = conn.Close()
}

// The operator's opt-out forces the pre-#1136 hot restart across a change.
func TestConcurrencyChangeOptOutHotRestarts(t *testing.T) {
	f := newPredecessor(t, 4)
	s := newSuccessorSupervisor(t, f, 2)
	s.cfg.HotRestartOnConcurrencyChange = true
	initStartEpochWithin(t, s, 10*time.Second)
	assertHotRestart(t, s, f)
}

// A predecessor this supervisor already drained (its stop failed and it still
// answers LIVE, e.g. on a bind-collision retry) is never drained twice.
func TestAlreadyDrainedPredecessorIsNotDrainedAgain(t *testing.T) {
	f := newPredecessor(t, 4)
	s := newSuccessorSupervisor(t, f, 2)
	s.drainedPredecessor = predecessorIdentity
	initStartEpochWithin(t, s, 10*time.Second)
	assert.Equal(t, predecessorEpoch+1, s.nextEpoch)
	assert.Zero(t, f.drainHits.Load())
}

// Readiness in the new lineage: a predecessor still answering LIVE at the
// epoch the fresh lineage reuses must not make this pod Ready; only our own
// Envoy's answer may.
func TestFreshLineageReadinessRequiresOwnIdentity(t *testing.T) {
	f := newFakeAdmin(t, adminLiveState, 0)
	f.identity.Store(predecessorIdentity)
	s := New(Config{AdminAddress: f.addr()}, slog.New(slog.DiscardHandler), nil)
	ctx := context.Background()

	live, _ := s.adminServerInfo(ctx, 0)
	assert.True(t, live, "outside a fresh lineage the epoch alone decides, as before")

	s.readyRequiresOwnIdentity = true
	live, reachable := s.adminServerInfo(ctx, 0)
	assert.False(t, live, "a foreign envoy at our epoch is not our fresh envoy")
	assert.True(t, reachable)

	f.identity.Store(s.adminIdentity)
	live, _ = s.adminServerInfo(ctx, 0)
	assert.True(t, live, "our own fresh envoy LIVE at epoch 0 makes the pod Ready")
}

func TestConcurrencyArg(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		n        int
		explicit bool
		wantErr  bool
		repeated bool
	}{
		{args: nil},
		{args: []string{"-l", "info"}},
		{args: []string{"--concurrency", "2"}, n: 2, explicit: true},
		// The pinned Envoy does not parse --concurrency=N (#1407), so no Envoy
		// runs with that count. Until #1407 this case expected 4.
		{args: []string{"--concurrency=4"}, wantErr: true},
		{args: []string{"-l", "info", "--concurrency", "2", "--service-node", "n1"}, n: 2, explicit: true},
		// Envoy refuses a repeated --concurrency; it does not keep the last
		// (#1375). So there is no worker count to report.
		{args: []string{"--concurrency", "2", "--concurrency=3"}, wantErr: true, repeated: true},
		{args: []string{"--concurrency", "2", "--concurrency", "2"}, wantErr: true, repeated: true},
		{args: []string{"--concurrency=x", "--concurrency", "2"}, wantErr: true, repeated: true},
		{args: []string{"--concurrency", "2", "--concurrency"}, wantErr: true, repeated: true},
		// A second --concurrency where the first one's value should be is a
		// repeat too, not a bad value.
		{args: []string{"--concurrency", "--concurrency", "2"}, wantErr: true, repeated: true},
		{args: []string{"--concurrency", "--concurrency=4"}, wantErr: true, repeated: true},
		{args: []string{"--concurrency", "--concurrency"}, wantErr: true, repeated: true},
		{args: []string{"--concurrency"}, wantErr: true},
		{args: []string{"-l", "info", "--concurrency"}, wantErr: true},
		// Envoy runs ONE worker for 0 (max(1, value); measured, #1408). Until
		// #1408 this case expected an error.
		{args: []string{"--concurrency", "0"}, n: 1, explicit: true},
		{args: []string{"--concurrency=x"}, wantErr: true},
		{args: []string{"--concurrency", "02"}, n: 2, explicit: true},
		// What Envoy refuses, or reads as a count the supervisor cannot know
		// (parseConcurrencyValue has the measurements).
		{args: []string{"--concurrency", "x"}, wantErr: true},
		{args: []string{"--concurrency", ""}, wantErr: true},
		{args: []string{"--concurrency", "-1"}, wantErr: true},
		{args: []string{"--concurrency", "+2"}, wantErr: true},
		{args: []string{"--concurrency", " 2"}, wantErr: true},
		{args: []string{"--concurrency", "2 "}, wantErr: true},
		{args: []string{"--concurrency", "1.5"}, wantErr: true},
		{args: []string{"--concurrency", "4294967296"}, wantErr: true},
		{args: []string{"--concurrency", "2147483648"}, wantErr: true},
		{args: []string{"--concurrency", "2147483647"}, n: 2147483647, explicit: true},
		// The next argument is the value even when it is a flag: Envoy reads
		// "-l" as the count and refuses it.
		{args: []string{"--concurrency", "-l", "info"}, wantErr: true},
		{args: []string{"--concurrency", "--skip-hot-restart-parent-stats"}, wantErr: true},
	} {
		n, explicit, err := concurrencyArg(tc.args)
		if tc.wantErr {
			require.Error(t, err, "%v", tc.args)
			assert.Equal(t, tc.repeated, errors.Is(err, errRepeatedConcurrency), "%v: %v", tc.args, err)
			assert.False(t, explicit, "%v", tc.args)
			continue
		}
		require.NoError(t, err, "%v", tc.args)
		assert.Equal(t, tc.n, n, "%v", tc.args)
		assert.Equal(t, tc.explicit, explicit, "%v", tc.args)
	}
}

func TestParseCPUList(t *testing.T) {
	for list, want := range map[string]int{"0": 1, "0-3": 4, "0,2-5": 5, "0-1,4-5,7": 5} {
		got, err := parseCPUList(list)
		require.NoError(t, err, list)
		assert.Equal(t, want, got, list)
	}
	for _, bad := range []string{"", "a", "3-1", "0-"} {
		_, err := parseCPUList(bad)
		assert.Error(t, err, bad)
	}
}

func TestHandoffModesSeededAndRecorded(t *testing.T) {
	m, reader := newTestSupervisorMetrics(t)
	byMode := metricSumByAttr(t, reader, "aether.supervisor.handoff_mode", attrHandoffMode)
	for _, mode := range handoffModeValues {
		if got, ok := byMode[mode]; !ok || got != 0 {
			t.Errorf("mode %q seeded = %d (exported %v), want 0", mode, got, ok)
		}
	}
	m.handoffMode(handoffModeFreshAfterDrain)
	byMode = metricSumByAttr(t, reader, "aether.supervisor.handoff_mode", attrHandoffMode)
	assert.EqualValues(t, 1, byMode[handoffModeFreshAfterDrain])
	assert.EqualValues(t, 0, byMode[handoffModeHot])
}
