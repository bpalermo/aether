package hotrestart

import (
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStartingSupervisor is a new proxy pod's supervisor on a node whose admin
// is f: production-shaped (state directory, admin address), nothing forked
// yet, with a base id of its own.
func newStartingSupervisor(t *testing.T, f *fakeAdminServer, recordPath string) *Supervisor {
	t.Helper()
	requireShell(t)
	return New(Config{
		EnvoyPath:                 stubEnvoy(t, recordPath),
		ConfigPath:                filepath.Join(t.TempDir(), "envoy.yaml"),
		BaseID:                    testBaseID(),
		DrainTime:                 1 * time.Second,
		ParentShutdownTime:        1 * time.Second,
		StateDir:                  t.TempDir(),
		ReadyMarkerPath:           filepath.Join(t.TempDir(), "ready"),
		AdminAddress:              f.addr(),
		AdminUnresponsiveDeadline: 2 * time.Second,
	}, slog.New(slog.DiscardHandler), nil)
}

// runUntilCleanup starts s.Run and returns its result channel; the cleanup
// cancels it, kills whatever it forked and waits for it.
func runUntilCleanup(t *testing.T, s *Supervisor) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		result <- s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		deadline := time.After(20 * time.Second)
		for {
			for e := 0; e < 6; e++ {
				s.signalEpoch(e, syscall.SIGKILL)
			}
			select {
			case <-returned:
				return
			case <-deadline:
				t.Error("Run did not return after the test")
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	return cancel, result
}

// TestStaleHeartbeatBesideALiveEnvoyStartsNothing is issue #1680, against the
// real Run loop.
//
// The node's Envoy runs at epoch 1 and its heartbeat is stale (its supervisor
// is stalled). A new pod's supervisor starts. It used to read "no live
// predecessor" and fork epoch 0, which beside a live epoch 1 does not collide
// on the hot-restart socket: on the pinned Envoy it replaces the shared memory
// and leaves it marked "initializing", and nothing can hot-restart from the
// live Envoy again (//agent/test/hotrestartrevert).
//
// The kernel says the Envoy is there: a process holds a hot-restart socket of
// the base id. While it does, nothing is forked. When the heartbeat is fresh
// again, the supervisor attaches at the next epoch like any new pod.
func TestStaleHeartbeatBesideALiveEnvoyStartsNothing(t *testing.T) {
	f := newFakeAdmin(t, adminLiveState, 1)
	f.identity.Store(predecessorIdentity)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newStartingSupervisor(t, f, recordPath)
	holdEnvoySocket(t, s.cfg.BaseID, 1)
	writeRawState(t, s, 1, 2*predecessorStale)

	_, done := runUntilCleanup(t, s)
	select {
	case err := <-done:
		t.Fatalf("Run returned %v", err)
	case <-time.After(3 * time.Second):
	}
	require.Empty(t, recordedEpochs(t, recordPath),
		"an envoy was forked beside the live one whose heartbeat is stale: at epoch 0 it replaces the live envoy's "+
			"hot-restart shared memory")

	// The stalled supervisor runs again and writes its heartbeat.
	writeRawState(t, s, 1, 0)
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 1 },
		10*time.Second, 20*time.Millisecond, "the supervisor never attached to the predecessor")
	assert.Equal(t, []string{"2"}, recordedEpochs(t, recordPath), "it must hot-restart from the live epoch 1")
}

// TestStaleHeartbeatWaitsForTheEnvoyToExit: the other way the wait ends. The
// Envoy that holds the base id is a terminating pod's, in its final drain (its
// supervisor stopped the heartbeat on purpose, so that nobody attaches to it).
// Epoch 0 is started when it has exited, and not before.
func TestStaleHeartbeatWaitsForTheEnvoyToExit(t *testing.T) {
	f := newFakeAdmin(t, adminLiveState, 4)
	f.identity.Store(predecessorIdentity)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newStartingSupervisor(t, f, recordPath)
	release := holdEnvoySocket(t, s.cfg.BaseID, 4)
	writeRawState(t, s, 4, 2*predecessorStale)

	runUntilCleanup(t, s)
	time.Sleep(2 * time.Second)
	require.Empty(t, recordedEpochs(t, recordPath), "an envoy was forked beside the draining one")

	release()
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 1 },
		10*time.Second, 20*time.Millisecond, "the supervisor never started once the envoy had exited")
	assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath))
}

// TestStaleHeartbeatWaitIsBounded: an Envoy that holds the base id and is
// never reported LIVE by a supervisor does not keep a new pod waiting for
// ever. Run fails, which restarts the container, and it has forked nothing.
func TestStaleHeartbeatWaitIsBounded(t *testing.T) {
	f := newFakeAdmin(t, adminLiveState, 1)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newStartingSupervisor(t, f, recordPath)
	s.unheardEnvoyWait = 1500 * time.Millisecond
	holdEnvoySocket(t, s.cfg.BaseID, 1)

	_, done := runUntilCleanup(t, s)
	select {
	case err := <-done:
		require.ErrorContains(t, err, "not starting an envoy beside it")
	case <-time.After(15 * time.Second):
		t.Fatal("Run never gave up on the envoy without a heartbeat")
	}
	assert.Empty(t, recordedEpochs(t, recordPath))
}

// TestNoHeartbeatAndNoEnvoyStartsAtOnce: the ordinary first start of a node is
// not delayed by any of this.
func TestNoHeartbeatAndNoEnvoyStartsAtOnce(t *testing.T) {
	f := newFakeAdmin(t, adminLiveState, 0)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newStartingSupervisor(t, f, recordPath)

	start := time.Now()
	require.NoError(t, s.initStartEpoch(context.Background()))
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, 0, s.nextEpoch)
}

// TestEpochZeroIsNeverForkedBesideAnEnvoy: the last line of defence. Whatever
// chose epoch 0, hotRestart refuses it while a process holds the base id.
func TestEpochZeroIsNeverForkedBesideAnEnvoy(t *testing.T) {
	f := newFakeAdmin(t, adminLiveState, 2)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := newStartingSupervisor(t, f, recordPath)
	release := holdEnvoySocket(t, s.cfg.BaseID, 2)

	require.ErrorIs(t, s.hotRestart(), errEpochZeroBesideEnvoy)
	assert.False(t, s.anyChildTracked())
	assert.Equal(t, -1, s.currentEpoch(), "a refused fork must not advance the epoch")

	release()
	require.NoError(t, s.hotRestart())
	t.Cleanup(func() { s.signalEpoch(0, syscall.SIGKILL) })
	assert.True(t, s.childTracked(0))
}

// TestDrainedPredecessorThatDidNotExitIsNotStartedBeside: the worker-count
// path (#1136) starts a fresh lineage at epoch 0 after it has drained and
// stopped the predecessor. When the predecessor does not exit (its stop
// failed), it used to start anyway, "a base-id collision is retried
// in-process": that is the same epoch 0 beside a live Envoy. It must choose
// the epoch again instead, and fork epoch 0 only once the Envoy is gone.
func TestDrainedPredecessorThatDidNotExitIsNotStartedBeside(t *testing.T) {
	f := newPredecessor(t, 4)
	s := newSuccessorSupervisor(t, f, 2)
	s.cfg.BaseID = testBaseID()
	release := holdEnvoySocket(t, s.cfg.BaseID, predecessorEpoch)

	done := make(chan error, 1)
	go func() { done <- s.initStartEpoch(context.Background()) }()
	require.Eventually(t, func() bool { return f.quitHits.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("epoch chosen (%v, next epoch %d) while the drained predecessor still holds the base id", err, s.nextEpoch)
	case <-time.After(shutdownGrace + 2*time.Second):
	}

	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("no epoch was chosen after the drained predecessor exited")
	}
	assert.Equal(t, 0, s.nextEpoch)
	assert.EqualValues(t, 1, f.drainHits.Load(), "the predecessor must not be drained twice")
}

// writeShm writes a hot-restart shared-memory segment as the pinned Envoy lays
// it out on amd64 (104 bytes: size, version, two mutexes, flags) into dir.
func writeShm(t *testing.T, s *Supervisor, size int, version, flags uint64) {
	t.Helper()
	b := make([]byte, size)
	binary.NativeEndian.PutUint64(b[0:], uint64(size))
	binary.NativeEndian.PutUint64(b[8:], version)
	// A mutex's bytes, which nothing may touch.
	for i := 16; i < size-8; i++ {
		b[i] = 0xAB
	}
	binary.NativeEndian.PutUint64(b[size-8:], flags)
	require.NoError(t, os.WriteFile(s.shmPath(), b, 0o600))
}

func readShm(t *testing.T, s *Supervisor) []byte {
	t.Helper()
	b, err := os.ReadFile(s.shmPath())
	require.NoError(t, err)
	return b
}

// newRepairSupervisor: a supervisor about to hot-restart at epoch 2 from the
// node's Envoy, LIVE at epoch 1, which holds its hot-restart socket; the
// shared memory lives in a directory of the test's.
func newRepairSupervisor(t *testing.T) (*Supervisor, *fakeAdminServer, func()) {
	t.Helper()
	f := newFakeAdmin(t, adminLiveState, 1)
	f.identity.Store(predecessorIdentity)
	s := newStartingSupervisor(t, f, filepath.Join(t.TempDir(), "epochs.txt"))
	s.shmDir = t.TempDir()
	release := holdEnvoySocket(t, s.cfg.BaseID, 1)
	return s, f, release
}

// TestStaleInitializingFlagIsCleared: the shared memory says an Envoy is
// initializing, and the only Envoy of the base id is the LIVE one the
// supervisor is about to hot-restart from. The flag is stale (the Envoy that
// set it died), and with it set the fork exits at once with "previous envoy
// process is still initializing", for ever. The supervisor clears it, and
// nothing else in the segment.
func TestStaleInitializingFlagIsCleared(t *testing.T) {
	s, _, _ := newRepairSupervisor(t)
	writeShm(t, s, 104, envoyHotRestartVersion, 0x5) // initializing, and a flag the supervisor does not know
	before := readShm(t, s)

	set, err := s.shmInitializing()
	require.NoError(t, err)
	require.True(t, set)

	s.clearStaleInitializing(context.Background(), 2)

	set, err = s.shmInitializing()
	require.NoError(t, err)
	assert.False(t, set, "the stale flag was left: every hot restart from the live envoy would be refused")
	after := readShm(t, s)
	assert.Equal(t, before[:96], after[:96], "only the flags may be written")
	assert.EqualValues(t, 0x4, binary.NativeEndian.Uint64(after[96:]), "only the initializing bit may be cleared")
	assert.Equal(t, filepath.Join(s.shmDir, "envoy_shared_memory_"+strconv.FormatUint(uint64(s.cfg.BaseID)*10, 10)), s.shmPath())
}

// TestInitializingFlagIsLeftWhenItMayBeTrue: every case in which another Envoy
// of the base id may be initializing, or the segment is not the pinned
// Envoy's, leaves the flag alone.
func TestInitializingFlagIsLeftWhenItMayBeTrue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, s *Supervisor, f *fakeAdminServer, release func())
		size    int
		version uint64
	}{
		{
			name: "another envoy holds a socket of the base id", size: 104, version: envoyHotRestartVersion,
			arrange: func(t *testing.T, s *Supervisor, _ *fakeAdminServer, _ func()) { holdEnvoySocket(t, s.cfg.BaseID, 2) },
		},
		{
			name: "the parent's own parent is still draining", size: 104, version: envoyHotRestartVersion,
			arrange: func(t *testing.T, s *Supervisor, _ *fakeAdminServer, _ func()) { holdEnvoySocket(t, s.cfg.BaseID, 0) },
		},
		{
			name: "the parent is not LIVE", size: 104, version: envoyHotRestartVersion,
			arrange: func(_ *testing.T, _ *Supervisor, f *fakeAdminServer, _ func()) { f.set("INITIALIZING", 1) },
		},
		{
			name: "the admin answers for another epoch", size: 104, version: envoyHotRestartVersion,
			arrange: func(_ *testing.T, _ *Supervisor, f *fakeAdminServer, _ func()) { f.set(adminLiveState, 4) },
		},
		{
			name: "nothing holds the parent's socket", size: 104, version: envoyHotRestartVersion,
			arrange: func(_ *testing.T, _ *Supervisor, _ *fakeAdminServer, release func()) { release() },
		},
		{
			name: "another hot-restart version", size: 104, version: envoyHotRestartVersion + 1,
			arrange: func(*testing.T, *Supervisor, *fakeAdminServer, func()) {},
		},
		{
			name: "a segment too large to be the pinned layout", size: 512, version: envoyHotRestartVersion,
			arrange: func(*testing.T, *Supervisor, *fakeAdminServer, func()) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, release := newRepairSupervisor(t)
			writeShm(t, s, tc.size, tc.version, envoyShmFlagInitializing)
			tc.arrange(t, s, f, release)
			before := readShm(t, s)

			s.clearStaleInitializing(context.Background(), 2)

			assert.Equal(t, before, readShm(t, s), "the segment was written")
		})
	}
}

// TestShmSizeFieldMustMatchTheFile: a segment whose size field disagrees with
// the file is not a layout the supervisor knows, whatever its version.
func TestShmSizeFieldMustMatchTheFile(t *testing.T) {
	s, _, _ := newRepairSupervisor(t)
	writeShm(t, s, 104, envoyHotRestartVersion, envoyShmFlagInitializing)
	b := readShm(t, s)
	binary.NativeEndian.PutUint64(b[0:], 96)
	require.NoError(t, os.WriteFile(s.shmPath(), b, 0o600))

	_, err := s.shmInitializing()
	require.ErrorIs(t, err, errUnknownShmLayout)
	s.clearStaleInitializing(context.Background(), 2)
	assert.Equal(t, b, readShm(t, s))
}

// TestNoSharedMemoryIsNotAnError: the first Envoy of a node has none yet, and
// an epoch-0 start never looks.
func TestNoSharedMemoryIsNotAnError(t *testing.T) {
	s, _, _ := newRepairSupervisor(t)
	set, err := s.shmInitializing()
	require.NoError(t, err)
	assert.False(t, set)

	writeShm(t, s, 104, envoyHotRestartVersion, envoyShmFlagInitializing)
	before := readShm(t, s)
	s.clearStaleInitializing(context.Background(), 0)
	assert.Equal(t, before, readShm(t, s), "an epoch-0 start replaces the segment itself; the supervisor must not touch it")
}
