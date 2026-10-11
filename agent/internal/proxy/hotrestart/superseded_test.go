package hotrestart

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The file a terminating supervisor asks for a successor in. Spelled out, not
// taken from the constant: two supervisor versions on one node agree on it.
const successorWantedFile = "successor-wanted"

// handledExit is what handleChildExit returned.
type handledExit struct {
	err  error
	done bool
}

// testBaseID returns a base id no other test (and no Envoy on the machine) is
// likely to use: the successor_gone recovery asks the kernel whether any
// process holds a hot-restart socket of the supervisor's base id.
func testBaseID() uint32 { return 1_000_000 + rand.Uint32N(400_000_000) }

// holdEnvoySocket binds the hot-restart domain socket a live Envoy of baseID
// at the given restart epoch holds (role "parent"), until the returned
// function is called. It is the kernel-side trace of an Envoy process, which
// the stub Envoy does not leave.
func holdEnvoySocket(t *testing.T, baseID uint32, epoch int) (release func()) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("abstract unix sockets are Linux only")
	}
	// As Envoy binds it: sizeof(sockaddr_un) long, so NUL-padded to 108 bytes
	// of sun_path (//agent/test/hotrestartrevert holds this to the real one).
	name := fmt.Sprintf("@envoy_domain_socket_parent_%d", uint64(baseID)*10+uint64(epoch%3))
	name += strings.Repeat("\x00", 108-len(name))
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	require.NoError(t, err)
	var once sync.Once
	release = func() { once.Do(func() { _ = conn.Close() }) }
	t.Cleanup(release)
	return release
}

// newSupersededSupervisor returns a production-shaped supervisor (state
// directory, admin address) in the state a completed cross-pod handoff leaves
// the OLD pod in: its epoch-0 Envoy was started, and then exited 0, which is
// how the successor's parent-shutdown protocol ends it. The exit is returned
// unhandled, for the test to hand to handleChildExit.
func newSupersededSupervisor(t *testing.T, f *fakeAdminServer, envoyPath, stateDir string) (*Supervisor, childExit) {
	t.Helper()
	s := New(Config{
		EnvoyPath:                 envoyPath,
		ConfigPath:                filepath.Join(t.TempDir(), "envoy.yaml"),
		BaseID:                    testBaseID(),
		DrainTime:                 1 * time.Second,
		ParentShutdownTime:        1 * time.Second,
		StateDir:                  stateDir,
		ReadyMarkerPath:           filepath.Join(t.TempDir(), "ready"),
		AdminAddress:              f.addr(),
		AdminUnresponsiveDeadline: 2 * time.Second,
	}, slog.New(slog.DiscardHandler), nil)
	require.NoError(t, s.hotRestart())
	require.Eventually(t, func() bool { return s.childTracked(0) }, 5*time.Second, 20*time.Millisecond)
	// Let the stub install its TERM trap, so the signal is an exit 0.
	time.Sleep(300 * time.Millisecond)
	s.signalEpoch(0, syscall.SIGTERM)
	select {
	case exit := <-s.childExited:
		require.NoError(t, exit.err, "the stub must exit 0, as an envoy terminated by its successor does")
		require.Equal(t, 0, exit.epoch)
		t.Cleanup(func() {
			for e := 0; e < 8; e++ {
				s.signalEpoch(e, syscall.SIGKILL)
			}
		})
		return s, exit
	case <-time.After(10 * time.Second):
		t.Fatal("the stub envoy did not exit on SIGTERM")
		return nil, childExit{}
	}
}

// handleExit runs handleChildExit as Run's loop does and delivers its result.
func handleExit(ctx context.Context, s *Supervisor, exit childExit) <-chan handledExit {
	out := make(chan handledExit, 1)
	go func() {
		var bind, crash int
		err, done, _ := s.handleChildExit(ctx, exit, &bind, &crash)
		out <- handledExit{err: err, done: done}
	}()
	return out
}

func writeRawSuccessorWanted(t *testing.T, stateDir string, epoch int, age time.Duration) {
	t.Helper()
	data := fmt.Sprintf("%d %d\n", epoch, time.Now().Add(-age).UnixMilli())
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, successorWantedFile), []byte(data), 0o644))
}

// newTerminatingSuccessor returns the supervisor of the surge pod of that
// handoff: its Envoy runs at epoch 1 and holds the node. The fake admin must
// answer for it.
func newTerminatingSuccessor(t *testing.T, f *fakeAdminServer, stateDir string) (*Supervisor, string) {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), "successor-epochs.txt")
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          1 * time.Second,
		ParentShutdownTime: 1 * time.Second,
		StateDir:           stateDir,
		ReadyMarkerPath:    filepath.Join(t.TempDir(), "ready"),
		AdminAddress:       f.addr(),
	}, slog.New(slog.DiscardHandler), nil)
	s.mu.Lock()
	s.nextEpoch = 1
	s.mu.Unlock()
	require.NoError(t, s.hotRestart())
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 1 },
		5*time.Second, 20*time.Millisecond, "the successor's stub envoy did not start")
	require.Equal(t, 1, s.currentEpoch())
	t.Cleanup(func() { s.signalEpoch(1, syscall.SIGKILL) })
	return s, recordPath
}

// TestSupersededSupervisorTakesTheNodeBackFromATerminatingSuccessor is the
// nightly e2e failure of 2026-10-11 (first-install, both Helm majors).
//
// A roll is reverted while the surge pod is starting: the DaemonSet deletes
// the surge pod and keeps the old one. The surge pod's Envoy has already taken
// the node over, and it terminates the old pod's Envoy when its
// parent-shutdown time is up. The node's only Envoy is now in a pod that is
// terminating, and the pod that stays has none.
//
// The terminating supervisor asks for a successor while it waits for one, and
// the superseded supervisor answers: it hot-restarts from the epoch that
// asked, as a new pod would. Before the fix it waited for a deletion that
// never came: this test fails on its first timeout, and on the node the
// terminating pod drained 155 s later and nothing served any more.
func TestSupersededSupervisorTakesTheNodeBackFromATerminatingSuccessor(t *testing.T) {
	requireShell(t)

	stateDir := t.TempDir()
	// The node's admin: the successor's Envoy, LIVE at epoch 1.
	f := newFakeAdmin(t, adminLiveState, 1)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)

	successor, _ := newTerminatingSuccessor(t, f, stateDir)
	f.identity.Store(successor.adminIdentity)
	successor.cfg.TerminationGrace = 60 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := handleExit(ctx, old, exit)

	// The surge pod is deleted: its supervisor gets the SIGTERM, with its
	// Envoy LIVE at its own epoch.
	shutdown := make(chan error, 1)
	go func() { shutdown <- successor.handleShutdown(cancelledCtx()) }()

	select {
	case got := <-handled:
		require.NoError(t, got.err)
		require.False(t, got.done, "the superseded supervisor must keep running, with an envoy again")
	case <-time.After(15 * time.Second):
		t.Fatal("the superseded supervisor never started an envoy again: it is waiting for a pod deletion " +
			"that will not come, while the node's only envoy belongs to a pod that is terminating")
	}
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 },
		5*time.Second, 20*time.Millisecond)
	assert.Equal(t, []string{"0", "2"}, recordedEpochs(t, recordPath),
		"it must hot-restart from the terminating successor's epoch 1, not start beside it")
	assert.Equal(t, 2, old.currentEpoch())
	assert.True(t, old.childTracked(2))
	// A cross-pod handoff like any other: the pod is not Ready before the
	// envoy it attached to has been terminated.
	old.mu.Lock()
	gated, peer := old.gatedEpoch, old.handoffPeer
	old.mu.Unlock()
	assert.Equal(t, 2, gated, "the take-back must be gated as a cross-pod successor")
	assert.Equal(t, 1, peer)
	assert.Zero(t, f.drainHits.Load(), "nothing may drain the envoy that is being handed over")

	// The old pod's new Envoy takes over and terminates the successor's, which
	// is what the terminating supervisor was waiting for.
	successor.signalEpoch(1, syscall.SIGTERM)
	select {
	case err := <-shutdown:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the terminating supervisor did not return after its envoy was taken over")
	}
	assert.NoFileExists(t, filepath.Join(stateDir, successorWantedFile),
		"the request must be withdrawn once the wait is over")
}

// TestTerminatingSupervisorAsksForASuccessorWhileItsEnvoyIsLive pins what the
// terminating side writes, and when. While it waits for a successor with its
// Envoy LIVE at its own epoch it keeps the node epoch heartbeat fresh (nothing
// else does after the SIGTERM) and writes the request. With a successor
// already attached (the admin answers at a newer epoch) it writes neither.
func TestTerminatingSupervisorAsksForASuccessorWhileItsEnvoyIsLive(t *testing.T) {
	requireShell(t)

	t.Run("live at its own epoch", func(t *testing.T) {
		f := newFakeAdmin(t, adminLiveState, 0)
		s := newShutdownSupervisor(t, f)
		s.cfg.TerminationGrace = 60 * time.Second
		wanted := filepath.Join(s.cfg.StateDir, successorWantedFile)

		done := make(chan error, 1)
		go func() { done <- s.handleShutdown(cancelledCtx()) }()

		require.Eventually(t, func() bool {
			epoch, heartbeat, ok := s.readState()
			return ok && epoch == 0 && time.Since(heartbeat) < 2*time.Second
		}, 5*time.Second, 50*time.Millisecond, "the waiting supervisor did not keep the node epoch heartbeat fresh")
		b, err := os.ReadFile(wanted)
		require.NoError(t, err, "the waiting supervisor did not ask for a successor")
		var epoch int
		var millis int64
		_, err = fmt.Sscanf(string(b), "%d %d\n", &epoch, &millis)
		require.NoError(t, err, "want \"<epoch> <unixMillis>\", got %q", b)
		assert.Equal(t, 0, epoch)
		assert.WithinDuration(t, time.Now(), time.UnixMilli(millis), 3*time.Second)

		// It is rewritten, like the heartbeat: a reader ages it out.
		require.Eventually(t, func() bool {
			again, err := os.ReadFile(wanted)
			return err == nil && string(again) != string(b)
		}, 5*time.Second, 50*time.Millisecond, "the request is written once and would go stale during the wait")

		s.signalEpoch(0, syscall.SIGTERM)
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("handleShutdown did not return after the successor took over")
		}
		assert.NoFileExists(t, wanted)
	})

	t.Run("a successor is already attached", func(t *testing.T) {
		f := newFakeAdmin(t, adminLiveState, 1) // answers at a NEWER epoch than ours
		s := newShutdownSupervisor(t, f)
		s.cfg.TerminationGrace = 60 * time.Second

		done := make(chan error, 1)
		go func() { done <- s.handleShutdown(cancelledCtx()) }()

		time.Sleep(2500 * time.Millisecond)
		assert.NoFileExists(t, filepath.Join(s.cfg.StateDir, successorWantedFile),
			"a second successor was asked for while one is attached")
		_, _, ok := s.readState()
		assert.False(t, ok, "the heartbeat was written for an epoch that is not the node's LIVE one")

		s.signalEpoch(0, syscall.SIGTERM)
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("handleShutdown did not return after the successor took over")
		}
	})
}

// TestSupersededSupervisorStartsAgainWhenNoEnvoyIsLeftOnTheNode is the other
// recovery: the successor is gone and never said so (its pod was killed
// outright, or its supervisor does not write the request). Nothing answers the
// node's admin and no supervisor confirms a LIVE Envoy, so after
// AdminUnresponsiveDeadline the superseded supervisor starts a fresh Envoy at
// epoch 0. Before the fix it never did.
func TestSupersededSupervisorStartsAgainWhenNoEnvoyIsLeftOnTheNode(t *testing.T) {
	requireShell(t)

	stateDir := t.TempDir()
	f := newFakeAdmin(t, adminLiveState, 1)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The successor's Envoy is gone: connections to the admin are refused.
	f.srv.Close()
	gone := time.Now()
	handled := handleExit(ctx, old, exit)

	// Not at once: an admin that is silent for a moment is what a successor
	// starting up looks like. The bound is the admin watchdog's (2s here).
	time.Sleep(1500 * time.Millisecond)
	require.Len(t, recordedEpochs(t, recordPath), 1, "an envoy was started before the node was known to have none")

	select {
	case got := <-handled:
		require.NoError(t, got.err)
		require.False(t, got.done)
	case <-time.After(20 * time.Second):
		t.Fatal("the superseded supervisor never started an envoy again on a node that has none")
	}
	assert.GreaterOrEqual(t, time.Since(gone), 2*time.Second)
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 },
		5*time.Second, 20*time.Millisecond)
	assert.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath), "a fresh lineage starts at epoch 0")
	assert.Equal(t, 0, old.currentEpoch())
	old.mu.Lock()
	gated := old.gatedEpoch
	old.mu.Unlock()
	assert.Equal(t, -1, gated, "a fresh start has no predecessor to gate readiness on")
}

// TestSupersededSupervisorLeavesAHealthySuccessorAlone is the roll that
// completes, which is every roll but the reverted one: the successor serves,
// and the superseded supervisor must do nothing until its pod is deleted. Were
// it to start an Envoy, it would take the node from the new pod, which would
// then be the superseded one, NotReady, with the roll waiting on it for good.
func TestSupersededSupervisorLeavesAHealthySuccessorAlone(t *testing.T) {
	requireShell(t)

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fakeAdminServer, stateDir string, old *Supervisor)
	}{
		{
			// The successor's supervisor has not confirmed LIVE yet (or its
			// probes time out): no heartbeat, but its Envoy answers.
			name:  "the admin answers and no heartbeat is written",
			setup: func(*testing.T, *fakeAdminServer, string, *Supervisor) {},
		},
		{
			// The successor's admin is silent for longer than the bound (its
			// main thread is busy), but its supervisor still confirms it.
			name: "the admin is silent and the heartbeat is fresh",
			setup: func(t *testing.T, f *fakeAdminServer, _ string, old *Supervisor) {
				f.hang.Store(true)
				stop := make(chan struct{})
				t.Cleanup(func() { close(stop) })
				go func() {
					for {
						old.writeState(1)
						select {
						case <-stop:
							return
						case <-time.After(200 * time.Millisecond):
						}
					}
				}()
			},
		},
		{
			// A request left behind by a pod deleted long ago.
			name: "a stale request for a successor",
			setup: func(t *testing.T, _ *fakeAdminServer, stateDir string, _ *Supervisor) {
				writeRawSuccessorWanted(t, stateDir, 1, time.Minute)
			},
		},
		{
			// A fresh request, but the Envoy that answers is not the one
			// that asked: a newer epoch has taken over since.
			name: "a request from an epoch that no longer holds the node",
			setup: func(t *testing.T, f *fakeAdminServer, stateDir string, _ *Supervisor) {
				f.set(adminLiveState, 2)
				writeRawSuccessorWanted(t, stateDir, 1, 0)
			},
		},
	} {
		// Not parallel: every subtest writes a stub Envoy script and execs it,
		// and a child forked by a sibling in between holds the script open for
		// writing (exec fails with "text file busy").
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			f := newFakeAdmin(t, adminLiveState, 1)
			recordPath := filepath.Join(t.TempDir(), "epochs.txt")
			old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)
			tc.setup(t, f, stateDir, old)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handled := handleExit(ctx, old, exit)

			select {
			case got := <-handled:
				t.Fatalf("the superseded supervisor stopped waiting for its deletion: %+v", got)
			case <-time.After(6 * time.Second):
			}
			assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath),
				"an envoy was started against a successor that serves the node")
			assert.Equal(t, 0, old.currentEpoch(),
				"epoch detection was run again against a successor that serves the node")

			// The roll completes: the DaemonSet deletes the old pod.
			cancel()
			select {
			case got := <-handled:
				assert.NoError(t, got.err)
				assert.True(t, got.done, "a deleted pod's supervisor must exit")
			case <-time.After(10 * time.Second):
				t.Fatal("the superseded supervisor did not exit when its pod was deleted")
			}
		})
	}
}

// TestSupersededRelaunchThatDiesAtOnceIsNotRetriedThroughEpochDetection covers
// what the supervisor does when the Envoy it started again exits at once (the
// stub exits 1; what a real Envoy does to the node in that position is another
// matter, and the reason the fork is guarded: see envoyHoldsBaseID and
// //agent/test/hotrestartrevert). The ordinary answer to a quick exit is
// retryBindCollision: run epoch detection again and attach to whoever is LIVE
// by then. Here that would hot-restart from a healthy successor and take the
// node from the new pod, so the supervisor waits for its deletion again.
func TestSupersededRelaunchThatDiesAtOnceIsNotRetriedThroughEpochDetection(t *testing.T) {
	requireShell(t)

	stateDir := t.TempDir()
	f := newFakeAdmin(t, adminLiveState, 5)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	blockerPath := filepath.Join(t.TempDir(), "socket-held")
	old, exit := newSupersededSupervisor(t, f, failingThenServingEnvoy(t, recordPath, blockerPath), stateDir)

	// The successor's Envoy lives (it holds the socket) but its admin does not
	// answer and nothing confirms it: the node looks empty.
	require.NoError(t, os.WriteFile(blockerPath, []byte("x"), 0o644))
	f.hang.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := handleExit(ctx, old, exit)
	select {
	case got := <-handled:
		require.NoError(t, got.err)
		require.False(t, got.done)
	case <-time.After(30 * time.Second):
		t.Fatal("the superseded supervisor never tried to start an envoy on a node that looked empty")
	}
	var collided childExit
	select {
	case collided = <-old.childExited:
		require.Error(t, collided.err, "the relaunched stub must fail to bind")
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunched envoy did not exit on the bind collision")
	}
	require.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath))

	// The successor answers again, LIVE and confirmed: a bind-collision retry
	// would now attach to it at epoch 6.
	f.hang.Store(false)
	old.writeState(5)
	handled = handleExit(ctx, old, collided)
	select {
	case got := <-handled:
		t.Fatalf("handleChildExit returned instead of waiting for the pod's deletion: %+v", got)
	case <-time.After(bindCollisionRetryPause + 3*time.Second):
	}
	assert.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath),
		"the exit was retried through epoch detection, attaching to the healthy successor's envoy")
	assert.False(t, old.anyChildTracked())
	assert.Equal(t, 1, old.supersededLost)

	cancel()
	select {
	case got := <-handled:
		assert.NoError(t, got.err)
		assert.True(t, got.done)
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded supervisor did not exit when its pod was deleted")
	}
}

// TestSupersededRelaunchDoesNotAttachToAnEnvoyThatWentLiveMeanwhile: between
// the decision to start again and epoch detection, another supervisor's Envoy
// went LIVE (a new pod started on a node that looked empty). Epoch detection
// then answers "hot-restart from it", which no recovery asked for: the epoch
// is put back, nothing is forked, and the pod is not made Ready by the other
// Envoy's answers.
func TestSupersededRelaunchDoesNotAttachToAnEnvoyThatWentLiveMeanwhile(t *testing.T) {
	requireShell(t)

	for _, tc := range []struct {
		trigger string
		peer    int
	}{
		{trigger: triggerSuccessorGone, peer: -1},
		// The epoch that asked was 1; the one that is LIVE now is 5.
		{trigger: triggerSuccessorTerminating, peer: 1},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			stateDir := t.TempDir()
			// The other Envoy runs another worker count: unrestricted epoch
			// detection would drain it and stop it (issue #1136) before
			// anything here could refuse.
			f := newPredecessor(t, 4)
			f.set(adminLiveState, 5)
			recordPath := filepath.Join(t.TempDir(), "epochs.txt")
			old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)
			old.cfg.ExtraArgs = []string{"--concurrency", "2"}
			old.reap(exit.epoch)
			old.writeState(5)
			holdEnvoySocket(t, old.cfg.BaseID, 5)

			relaunched, err := old.relaunchSuperseded(context.Background(), exit.epoch, tc.trigger, tc.peer)
			require.NoError(t, err)
			assert.False(t, relaunched)
			assert.Zero(t, f.drainHits.Load(), "an envoy that did not ask for a successor was drained")
			assert.Zero(t, f.quitHits.Load(), "an envoy that did not ask for a successor was stopped")
			assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath), "an envoy was hot-restarted from a pod that serves the node")
			assert.Equal(t, 0, old.currentEpoch(), "the epoch must be back at the one that was terminated")
			old.mu.Lock()
			gated, peer := old.gatedEpoch, old.handoffPeer
			old.mu.Unlock()
			assert.Equal(t, -1, gated)
			assert.Equal(t, -1, peer)
		})
	}
}

// TestRunStartsNoEnvoyForAPodAlreadyTerminated: a surge pod deleted while its
// image is pulled gets its SIGTERM within milliseconds of starting. If that is
// before the first fork there is nothing to gain from forking: on a node with
// a live predecessor the new Envoy would take the node over only to be
// drained. Before the fix Run forked and then waited for a successor.
func TestRunStartsNoEnvoyForAPodAlreadyTerminated(t *testing.T) {
	requireShell(t)

	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          1 * time.Second,
		ParentShutdownTime: 1 * time.Second,
		StateDir:           t.TempDir(),
	}, slog.New(slog.DiscardHandler), nil)
	t.Cleanup(func() { s.signalEpoch(0, syscall.SIGKILL) })

	done := make(chan error, 1)
	go func() { done <- s.Run(cancelledCtx()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for a context that was cancelled before it started")
	}
	assert.Empty(t, recordedEpochs(t, recordPath), "an envoy was forked for a pod that was already terminating")
	assert.False(t, s.anyChildTracked())
}

// TestSupersededSupervisorStartsNoFreshEnvoyWhileAnEnvoyHoldsTheBaseID: the
// node looks empty (the admin is silent, the heartbeat is stale) and is not: a
// stopped or wedged Envoy answers nothing and is still there. A fresh epoch 0
// beside it is not a harmless probe. Unless the live epoch is a multiple of 3
// the new Envoy binds its own hot-restart sockets, replaces the base id's
// shared memory, dies on the admin address and leaves the "initializing" flag
// set, and no pod can hot-restart from the live Envoy again. So the recovery
// waits for as long as any process holds a hot-restart socket of the base id,
// and starts once none does.
func TestSupersededSupervisorStartsNoFreshEnvoyWhileAnEnvoyHoldsTheBaseID(t *testing.T) {
	requireShell(t)

	for _, liveEpoch := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("live epoch %d", liveEpoch), func(t *testing.T) {
			stateDir := t.TempDir()
			f := newFakeAdmin(t, adminLiveState, liveEpoch)
			recordPath := filepath.Join(t.TempDir(), "epochs.txt")
			old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)
			old.cfg.AdminUnresponsiveDeadline = 1 * time.Second
			var logged syncBuffer
			old.log = slog.New(slog.NewTextHandler(&logged, nil))
			release := holdEnvoySocket(t, old.cfg.BaseID, liveEpoch)
			f.hang.Store(true) // the live Envoy's admin answers nothing

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handled := handleExit(ctx, old, exit)
			select {
			case got := <-handled:
				t.Fatalf("the superseded supervisor stopped waiting: %+v", got)
			case <-time.After(5 * time.Second):
			}
			require.Equal(t, []string{"0"}, recordedEpochs(t, recordPath),
				"a fresh envoy was started beside one that holds the base id")
			assert.Equal(t, 0, old.currentEpoch())
			// And it did not get as far as deciding to: the held socket is
			// part of what "the node has an Envoy" means.
			assert.Zero(t, logged.count("not starting envoy again"))

			// The Envoy exits: the kernel drops its socket with it.
			release()
			select {
			case got := <-handled:
				require.NoError(t, got.err)
				require.False(t, got.done)
			case <-time.After(15 * time.Second):
				t.Fatal("no envoy was started once nothing held the base id")
			}
			require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 },
				5*time.Second, 20*time.Millisecond)
			assert.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath))
		})
	}
}

// TestSupersededRelaunchForAStaleHeartbeatStartsNothingBesideTheAsker: a
// request read just inside its age limit, and the heartbeat written with it
// read just outside. Epoch detection then answers "no live predecessor, start
// fresh at epoch 0", while the Envoy that asked is alive. That fresh start is
// refused for the same reason as any other beside a live Envoy.
func TestSupersededRelaunchForAStaleHeartbeatStartsNothingBesideTheAsker(t *testing.T) {
	requireShell(t)

	stateDir := t.TempDir()
	f := newFakeAdmin(t, adminLiveState, 1)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)
	old.reap(exit.epoch)
	writeRawState(t, old, 1, predecessorStale+time.Second)
	writeRawSuccessorWanted(t, stateDir, 1, predecessorStale-time.Second)
	release := holdEnvoySocket(t, old.cfg.BaseID, 1)

	relaunched, err := old.relaunchSuperseded(context.Background(), exit.epoch, triggerSuccessorTerminating, 1)
	require.NoError(t, err)
	assert.False(t, relaunched)
	assert.Equal(t, []string{"0"}, recordedEpochs(t, recordPath), "a fresh epoch 0 was started beside the envoy that asked")
	assert.Equal(t, 0, old.currentEpoch())

	// The asker has exited: now the fresh start is the right answer.
	release()
	relaunched, err = old.relaunchSuperseded(context.Background(), exit.epoch, triggerSuccessorTerminating, 1)
	require.NoError(t, err)
	assert.True(t, relaunched)
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 }, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath))
}

// crashingThenServingEnvoy is failingThenServingEnvoy dying on a fatal signal
// instead of exiting 1.
func crashingThenServingEnvoy(t *testing.T, recordPath, blockerPath string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"epoch=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"echo \"$epoch\" >> \"" + recordPath + "\"\n" +
		"if [ -e \"" + blockerPath + "\" ]; then\n" +
		"  kill -SEGV $$\n" +
		"fi\n" +
		"trap 'exit 0' TERM INT\n" +
		"while true; do sleep 0.05; done\n"
	path := filepath.Join(t.TempDir(), "stub-envoy-crash.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestSupersededRelaunchLostAtOnceIsPacedAndBounded: an Envoy started again
// that dies at once, by a fatal signal as much as by an exit, sends the
// supervisor back to waiting and never through retryBindCollision; the next
// attempt comes no sooner than bindCollisionRetryPause; and after
// maxSupersededRelaunches in a row there is no further attempt, only the wait
// for the pod's deletion.
func TestSupersededRelaunchLostAtOnceIsPacedAndBounded(t *testing.T) {
	requireShell(t)

	stateDir := t.TempDir()
	f := newFakeAdmin(t, adminLiveState, 1)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	blockerPath := filepath.Join(t.TempDir(), "crash")
	old, exit := newSupersededSupervisor(t, f, crashingThenServingEnvoy(t, recordPath, blockerPath), stateDir)
	old.cfg.AdminUnresponsiveDeadline = 1 * time.Second
	require.NoError(t, os.WriteFile(blockerPath, []byte("x"), 0o644))
	f.srv.Close() // nothing answers, nothing holds the base id: successor_gone

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nextLoss := func(exit childExit, within time.Duration) childExit {
		t.Helper()
		handled := handleExit(ctx, old, exit)
		select {
		case got := <-handled:
			require.NoError(t, got.err)
			require.False(t, got.done)
		case <-time.After(within):
			t.Fatal("no envoy was started again")
		}
		select {
		case lost := <-old.childExited:
			require.True(t, isCrashSignal(lost.err), "the stub must die on a fatal signal, got %v", lost.err)
			return lost
		case <-time.After(10 * time.Second):
			t.Fatal("the relaunched stub did not crash")
			return childExit{}
		}
	}

	first := nextLoss(exit, 20*time.Second)
	require.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath))

	// The second attempt: after the pause, and again at epoch 0 (a crash
	// retried through retryBindCollision would have come from there too, but
	// with epoch detection; the count below is what tells them apart).
	started := time.Now()
	second := nextLoss(first, 20*time.Second)
	// The pause, then the silence bound again (1 s here).
	assert.GreaterOrEqual(t, time.Since(started), bindCollisionRetryPause+time.Second, "the next attempt did not wait")
	assert.Equal(t, 1, old.supersededLost, "the first loss was not counted as a superseded relaunch")

	// The budget: with all but one attempt spent, one more loss ends them.
	old.supersededLost = maxSupersededRelaunches - 1
	attempts := len(recordedEpochs(t, recordPath))
	handled := handleExit(ctx, old, second)
	select {
	case got := <-handled:
		t.Fatalf("handleChildExit returned with the budget spent: %+v", got)
	case <-time.After(bindCollisionRetryPause + 4*time.Second):
	}
	assert.Len(t, recordedEpochs(t, recordPath), attempts, "an envoy was started after the budget was spent")
	cancel()
	select {
	case got := <-handled:
		assert.NoError(t, got.err)
		assert.True(t, got.done)
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not exit when its pod was deleted")
	}
}

// TestAdvertiseWindow: how long a terminating supervisor asks for a successor.
func TestAdvertiseWindow(t *testing.T) {
	s := New(Config{ParentShutdownTime: 15 * time.Second}, slog.New(slog.DiscardHandler), nil)
	for _, tc := range []struct {
		budget  time.Duration
		window  time.Duration
		bounded bool
	}{
		{budget: 0, window: 0, bounded: false},                                // an unbounded wait: no drain follows it
		{budget: 155 * time.Second, window: 132 * time.Second, bounded: true}, // the chart's defaults
		{budget: 23 * time.Second, window: 0, bounded: true},
		{budget: 5 * time.Second, window: 0, bounded: true},
	} {
		window, bounded := s.advertiseWindow(tc.budget)
		assert.Equal(t, tc.window, window, "budget %s", tc.budget)
		assert.Equal(t, tc.bounded, bounded, "budget %s", tc.budget)
	}
}

// TestTerminatingSupervisorStopsAskingBeforeItDrains: no successor comes. By
// the time the supervisor drains its Envoy, the request is withdrawn and the
// heartbeat is stale, so no pod that starts then is sent to hot-restart from an
// Envoy that is about to be SIGTERMed under it. (Advertised up to the drain, a
// new pod attached during it and its Envoy looped on "hot restart sendmsg()
// connection refused" until its own watchdog killed it.)
func TestTerminatingSupervisorStopsAskingBeforeItDrains(t *testing.T) {
	requireShell(t)

	f := newFakeAdmin(t, adminLiveState, 0)
	s := newShutdownSupervisor(t, f)
	// budget = 28s - (1s drain + 5s grace) - 10s margin = 12s;
	// window = 12s - (8s stale + 1s parent shutdown) = 3s.
	s.cfg.TerminationGrace = 28 * time.Second
	require.Equal(t, 12*time.Second, s.successorWaitBudget())
	wanted := filepath.Join(s.cfg.StateDir, successorWantedFile)

	done := make(chan error, 1)
	go func() { done <- s.handleShutdown(cancelledCtx()) }()

	require.Eventually(t, func() bool { _, err := os.Stat(wanted); return err == nil },
		2500*time.Millisecond, 20*time.Millisecond, "the request was never written")
	require.Eventually(t, func() bool { _, err := os.Stat(wanted); return os.IsNotExist(err) },
		6*time.Second, 20*time.Millisecond, "the request was not withdrawn when the window ended")

	// The moment the drain is requested.
	require.Eventually(t, func() bool { return f.drainHits.Load() > 0 }, 15*time.Second, 5*time.Millisecond,
		"the supervisor never drained")
	_, heartbeat, ok := s.readState()
	require.True(t, ok)
	assert.GreaterOrEqual(t, time.Since(heartbeat), predecessorStale,
		"the heartbeat is fresh as the drain starts: a pod starting now would hot-restart from the draining envoy")
	assert.NoFileExists(t, wanted)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("handleShutdown did not return")
	}
	assert.NoFileExists(t, wanted)
}

// switchAdmin is an Envoy admin on ONE fixed address that can go away and come
// back, as the node's shared admin does when one Envoy dies and another starts.
type switchAdmin struct {
	address string
	state   atomic.Value // string
	epoch   atomic.Int64
	mu      sync.Mutex
	srv     *http.Server
}

func newSwitchAdmin(t *testing.T, state string, epoch int) *switchAdmin {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	a := &switchAdmin{address: l.Addr().String()}
	a.set(state, epoch)
	a.serve(l)
	t.Cleanup(a.down)
	return a
}

func (a *switchAdmin) set(state string, epoch int) {
	a.state.Store(state)
	a.epoch.Store(int64(epoch))
}

func (a *switchAdmin) serve(l net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/server_info", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"state":%q,"command_line_options":{"restart_epoch":%d}}`,
			a.state.Load().(string), a.epoch.Load())
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		st := a.state.Load().(string)
		if st != adminLiveState {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintf(w, "%s\n", st)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	a.mu.Lock()
	a.srv = srv
	a.mu.Unlock()
	go func() { _ = srv.Serve(l) }()
}

func (a *switchAdmin) down() {
	a.mu.Lock()
	srv := a.srv
	a.srv = nil
	a.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

func (a *switchAdmin) up(t *testing.T) {
	t.Helper()
	var l net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if l, err = net.Listen("tcp", a.address); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, err)
	a.serve(l)
}

// TestSuccessorGoneRelaunchIsNotKilledByItsOwnAdminWatchdog drives the real Run
// loop, with watchLiveness running through the superseded wait as it does in
// the pod, through the successor_gone recovery (from the adversarial review of
// this change). The liveness loop has seen the node's admin silent for longer
// than the bound by then: that silence is the very condition of the recovery.
// Charged to the Envoy the recovery forks, it fired the admin watchdog on the
// first tick, before that Envoy had opened its admin: the supervisor killed
// what it had just started and exited 1. The fresh Envoy must get what any
// fresh Envoy gets, and the pod must go Ready with it.
func TestSuccessorGoneRelaunchIsNotKilledByItsOwnAdminWatchdog(t *testing.T) {
	requireShell(t)

	admin := newSwitchAdmin(t, adminLiveState, 0)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	marker := filepath.Join(t.TempDir(), "ready")
	s := New(Config{
		EnvoyPath:                 stubEnvoy(t, recordPath),
		ConfigPath:                filepath.Join(t.TempDir(), "envoy.yaml"),
		BaseID:                    testBaseID(),
		DrainTime:                 1 * time.Second,
		ParentShutdownTime:        1 * time.Second,
		StateDir:                  t.TempDir(),
		ReadyMarkerPath:           marker,
		AdminAddress:              admin.address,
		AdminUnresponsiveDeadline: 2 * time.Second,
	}, slog.New(slog.DiscardHandler), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for e := 0; e < 4; e++ {
			s.signalEpoch(e, syscall.SIGKILL)
		}
	})

	// The kept pod: epoch 0, LIVE, Ready.
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		10*time.Second, 20*time.Millisecond, "the kept pod never went Ready at epoch 0")

	// The surge pod's Envoy (epoch 1) takes the node and terminates ours.
	admin.set(adminLiveState, 1)
	s.signalEpoch(0, syscall.SIGTERM)
	require.Eventually(t, func() bool { return !s.anyChildTracked() }, 10*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return os.IsNotExist(err) },
		5*time.Second, 20*time.Millisecond, "a pod with no envoy stayed Ready")

	// The surge pod is killed outright: nothing answers on the node any more.
	admin.down()
	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 2 },
		30*time.Second, 5*time.Millisecond, "successor_gone never started an envoy again")
	require.Equal(t, []string{"0", "0"}, recordedEpochs(t, recordPath))

	// The new Envoy's admin starts listening 1.2 s after its fork, LIVE at
	// epoch 0: at least one liveness tick finds it silent first.
	time.Sleep(1200 * time.Millisecond)
	admin.set(adminLiveState, 0)
	admin.up(t)

	select {
	case err := <-done:
		t.Fatalf("Run returned %v: the admin watchdog killed the envoy that the successor_gone recovery had just started", err)
	case <-time.After(4 * time.Second):
	}
	_, err := os.Stat(marker)
	require.NoError(t, err, "the kept pod did not go Ready with its fresh envoy")
	assert.True(t, s.childTracked(0))
}

// TestLostTakeBackLeavesTheSupervisorAtItsOwnEpoch: two superseded pods answer
// the same request, both fork the next epoch, and one loses (its Envoy cannot
// bind and exits). The loser must go back to the epoch it last ran. Left at
// the epoch it forked, its liveness loop asks the node's admin about exactly
// the epoch the winner now runs, reads LIVE, and marks a pod with no Envoy
// Ready (seen with real Envoys in //agent/test/hotrestartrevert,
// TestThreeGenerations).
func TestLostTakeBackLeavesTheSupervisorAtItsOwnEpoch(t *testing.T) {
	requireShell(t)

	stateDir := t.TempDir()
	f := newFakeAdmin(t, adminLiveState, 1)
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")
	blockerPath := filepath.Join(t.TempDir(), "socket-held")
	old, exit := newSupersededSupervisor(t, f, failingThenServingEnvoy(t, recordPath, blockerPath), stateDir)
	require.NoError(t, os.WriteFile(blockerPath, []byte("x"), 0o644))

	successor, _ := newTerminatingSuccessor(t, f, stateDir)
	f.identity.Store(successor.adminIdentity)
	successor.cfg.TerminationGrace = 60 * time.Second
	go func() { _ = successor.handleShutdown(cancelledCtx()) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := handleExit(ctx, old, exit)
	select {
	case got := <-handled:
		require.NoError(t, got.err)
		require.False(t, got.done)
	case <-time.After(15 * time.Second):
		t.Fatal("the request was not answered")
	}
	var lost childExit
	select {
	case lost = <-old.childExited:
		require.Error(t, lost.err)
		require.Equal(t, 2, lost.epoch)
	case <-time.After(10 * time.Second):
		t.Fatal("the relaunched stub did not exit")
	}
	require.Equal(t, 2, old.currentEpoch())

	// The other pod won: its Envoy is LIVE at epoch 2.
	f.set(adminLiveState, 2)
	handled = handleExit(ctx, old, lost)
	require.Eventually(t, func() bool { return old.currentEpoch() == 0 }, 2*time.Second, 10*time.Millisecond,
		"the supervisor still asks about the epoch another pod's envoy runs")
	old.mu.Lock()
	gated, peer := old.gatedEpoch, old.handoffPeer
	old.mu.Unlock()
	assert.Equal(t, -1, gated)
	assert.Equal(t, -1, peer)
	select {
	case got := <-handled:
		t.Fatalf("handleChildExit returned: %+v", got)
	case <-time.After(1 * time.Second):
	}
	successor.signalEpoch(1, syscall.SIGTERM)
}
