package hotrestart

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
			case <-time.After(5 * time.Second):
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

// TestSupersededRelaunchThatCollidesGoesBackToWaiting covers the recovery
// being wrong about the node: it starts a fresh Envoy, and the base-id socket
// is held, so an Envoy is there after all (one whose admin was silent). The
// ordinary answer to a bind collision is to retry epoch detection and attach
// to whoever is LIVE by then. Here that would hot-restart from a healthy
// successor and take the node from the new pod, so the supervisor goes back to
// waiting for its deletion instead.
func TestSupersededRelaunchThatCollidesGoesBackToWaiting(t *testing.T) {
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
		"the collision was retried, attaching to the healthy successor's envoy")
	assert.False(t, old.anyChildTracked())

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
			f := newFakeAdmin(t, adminLiveState, 5)
			recordPath := filepath.Join(t.TempDir(), "epochs.txt")
			old, exit := newSupersededSupervisor(t, f, stubEnvoy(t, recordPath), stateDir)
			old.reap(exit.epoch)
			old.writeState(5)

			relaunched, err := old.relaunchSuperseded(context.Background(), exit.epoch, tc.trigger, tc.peer)
			require.NoError(t, err)
			assert.False(t, relaunched)
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
