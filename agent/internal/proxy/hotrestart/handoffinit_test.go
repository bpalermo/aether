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

// Issue #1085. The proxy bootstrap now sets initial_fetch_timeout: 0s on CDS
// and LDS, so a hot-restart successor forked while the node agent is down
// stays in init — admin answering INITIALIZING at its own epoch — until the
// agent is back, instead of going LIVE with zero listeners and draining the
// parent. The handoff watchdog must not turn that wait into an outage: before
// the fix it fired HandoffDeadline after the fork whatever the successor was
// doing, and for an in-pod pair its SIGTERM reached the parent that was still
// serving the node.

// newInitializingPair is an in-pod hot restart on a fake clock: parent epoch 0
// (LIVE and answering until the fork), successor epoch 1 forked at the
// returned time. Both are tracked; no processes are started.
func newInitializingPair(t *testing.T, clk *fakeClock) (*Supervisor, *syncBuffer, time.Time) {
	t.Helper()
	s := New(Config{StateDir: t.TempDir()}, slog.New(slog.DiscardHandler), nil)
	logs := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	s.now = clk.now
	s.noteAdminAnswer(0)
	fork := clk.now()
	s.mu.Lock()
	s.children[0] = &exec.Cmd{}
	s.children[1] = &exec.Cmd{}
	s.nextEpoch = 2
	s.epochLaunched = fork
	s.epochLive = false
	s.mu.Unlock()
	return s, logs, fork
}

// TestHandoffWatchdogHoldsSuccessorWaitingOnXDS: a successor that answers its
// admin at its own epoch every tick is never declared wedged, however long
// its init takes (here five handoff deadlines: the agent down for ten
// minutes). It is logged once, when it first outlives the deadline.
func TestHandoffWatchdogHoldsSuccessorWaitingOnXDS(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, logs, fork := newInitializingPair(t, clk)
	ctx := context.Background()

	for clk.now().Sub(fork) < 5*defaultHandoffDeadline {
		clk.advance(readyPollInterval)
		s.noteAdminAnswer(1) // /server_info: {"state":"INITIALIZING","restart_epoch":1}
		require.False(t, s.checkWedgeWatchdogs(ctx, 1, true, true, time.Time{}),
			"successor answering at its epoch %s after the fork is waiting on xDS, not wedged",
			clk.now().Sub(fork))
	}
	select {
	case fire := <-s.watchdogFired:
		t.Fatalf("watchdog fired on a successor that kept answering: %v", fire.err)
	default:
	}
	assert.Equal(t, 1, logs.count("hot-restart successor still initializing past the handoff deadline"),
		"the wait is reported exactly once per epoch")
	assert.True(t, s.childTracked(0), "the parent is still serving")
}

// TestHandoffWatchdogFiresWhenInitializingSuccessorGoesSilent: the #1085
// carve-out does not blind the watchdog to the wedge it exists for. A
// successor that answered while in init and then stopped (its main thread
// blocked in a hot-restart RPC against a dead parent) trips it HandoffDeadline
// after its last answer.
//
// everLive is false here, as for a cross-pod successor pod that has never seen
// LIVE: that isolates the handoff watchdog from the admin watchdog, which in
// an in-pod pair (everLive) would already have fired 30 s into the silence.
func TestHandoffWatchdogFiresWhenInitializingSuccessorGoesSilent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, _, _ := newInitializingPair(t, clk)
	ctx := context.Background()

	clk.advance(3 * time.Minute)
	s.noteAdminAnswer(1) // last answer, past the fork-anchored deadline
	clk.advance(s.handoffDeadline())
	require.False(t, s.checkWedgeWatchdogs(ctx, 1, false, false, clk.now().Add(-s.handoffDeadline())),
		"exactly the deadline since the last answer is not past it")

	clk.advance(time.Second)
	require.True(t, s.checkWedgeWatchdogs(ctx, 1, false, false, clk.now().Add(-s.handoffDeadline())))
	fire := <-s.watchdogFired
	require.Error(t, fire.err)
	assert.Contains(t, fire.err.Error(), "handoff watchdog")
	assert.Contains(t, fire.err.Error(), "its last admin answer")
}

// TestHandoffWatchdogParentAnswersDoNotCount: only answers AT THE SUCCESSOR'S
// epoch hold the watchdog. The pre-#1085 shape — the admin keeps answering as
// the parent while the successor never does — still fires HandoffDeadline
// after the fork.
func TestHandoffWatchdogParentAnswersDoNotCount(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, _, fork := newInitializingPair(t, clk)
	ctx := context.Background()

	for clk.now().Sub(fork) <= s.handoffDeadline() {
		require.False(t, s.checkWedgeWatchdogs(ctx, 1, true, true, time.Time{}))
		clk.advance(readyPollInterval)
		s.noteAdminAnswer(0)
	}
	require.True(t, s.checkWedgeWatchdogs(ctx, 1, true, true, time.Time{}))
	fire := <-s.watchdogFired
	require.Error(t, fire.err)
	assert.Contains(t, fire.err.Error(), "since launch")
}

// TestHandoffWatchdogRunHoldsInitializingCrossPodSuccessor drives the real Run
// loop through the #1085 timeline with a 1 s handoff deadline: a cross-pod
// successor (epoch 1) is forked against a LIVE predecessor, then the admin
// answers INITIALIZING at epoch 1 — its CDS/LDS are waiting on an absent
// agent. Run must keep supervising well past the deadline; when the admin then
// stops answering altogether (a genuine wedge) the watchdog still fires.
func TestHandoffWatchdogRunHoldsInitializingCrossPodSuccessor(t *testing.T) {
	requireShell(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("v0\n"), 0o644))
	recordPath := filepath.Join(t.TempDir(), "epochs.txt")

	f := newFakeAdmin(t, "LIVE", 0)
	s := New(Config{
		EnvoyPath:          stubEnvoy(t, recordPath),
		ConfigPath:         configPath,
		DrainTime:          time.Second,
		ParentShutdownTime: time.Second,
		StateDir:           t.TempDir(),
		ReadyMarkerPath:    filepath.Join(t.TempDir(), "ready"),
		AdminAddress:       f.addr(),
		HandoffDeadline:    time.Second,
	}, slog.New(slog.DiscardHandler), nil)
	writeRawState(t, s, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	require.Eventually(t, func() bool { return len(recordedEpochs(t, recordPath)) == 1 },
		10*time.Second, 20*time.Millisecond, "successor never forked")
	require.Equal(t, []string{"1"}, recordedEpochs(t, recordPath))
	f.set("INITIALIZING", 1)

	select {
	case err := <-runErr:
		t.Fatalf("Run returned while the successor was answering INITIALIZING at its epoch: %v", err)
	case <-time.After(5 * time.Second):
	}
	assert.True(t, s.childTracked(1), "the initializing successor must not be stopped")

	f.hang.Store(true) // the successor's main thread wedges
	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "handoff watchdog")
	case <-time.After(15 * time.Second):
		t.Fatal("handoff watchdog did not fire once the successor stopped answering")
	}
}
