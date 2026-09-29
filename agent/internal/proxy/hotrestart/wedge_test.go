package hotrestart

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1058. The #1050 deadlock left both Envoys' main threads parked in
// blocking hot-restart socket calls, where SIGTERM (handled on that thread)
// cannot land. These tests pin the two supervisor responses: SIGKILL at once
// when both epochs of the pair are silent, and a once-per-epoch child-silent
// signal well before the watchdog fires.

// termStubEnvoy is a stand-in Envoy that records "TERM <epoch>" to termLog when
// it receives SIGTERM. With honorTerm it then exits; without it, it keeps
// running — a main thread that cannot take the signal, as in #1050.
func termStubEnvoy(t *testing.T, termLog string, honorTerm bool) string {
	t.Helper()
	onTerm := "echo \"TERM $epoch\" >> \"" + termLog + "\""
	if honorTerm {
		onTerm += "; exit 0"
	}
	script := "#!/bin/sh\n" +
		"epoch=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --restart-epoch) epoch=\"$2\"; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"trap '" + strings.ReplaceAll(onTerm, "'", "'\\''") + "' TERM\n" +
		"while true; do sleep 0.05; done\n"
	path := filepath.Join(t.TempDir(), "term-stub-envoy.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func readTermLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "TERM ", "TERM_"))
}

// newInPodPair starts two real (stub) Envoy epochs, 0 and 1, through the real
// hotRestart path on a fake clock: an in-pod hot restart with both processes
// tracked by this supervisor. Epoch 0 answered the admin at start (it was LIVE
// before the restart was allowed); epoch 1 forks one second later.
func newInPodPair(t *testing.T, clk *fakeClock, envoy string, drain time.Duration) *Supervisor {
	t.Helper()
	s := New(Config{
		EnvoyPath:          envoy,
		ConfigPath:         filepath.Join(t.TempDir(), "envoy.yaml"),
		DrainTime:          drain,
		ParentShutdownTime: 15 * time.Second,
		StateDir:           t.TempDir(),
		ReadyMarkerPath:    filepath.Join(t.TempDir(), "ready"),
	}, slog.New(slog.DiscardHandler), nil)
	s.now = clk.now

	require.NoError(t, s.hotRestart())
	s.noteAdminAnswer(0)
	clk.advance(time.Second)
	require.NoError(t, s.hotRestart())
	require.Equal(t, 1, s.currentEpoch())
	require.True(t, s.childTracked(0))
	require.True(t, s.childTracked(1))
	t.Cleanup(func() {
		s.signalEpoch(0, syscall.SIGKILL)
		s.signalEpoch(1, syscall.SIGKILL)
	})
	// Give the shells time to install their traps before any test signals them.
	time.Sleep(200 * time.Millisecond)
	return s
}

// TestWatchdogKillsAtOnceWhenBothEpochsSilent is the #1050 shape: the child
// went LIVE, answered for a few seconds, then both main threads blocked. When
// the watchdog fires the parent has been silent since its admin was handed to
// the child and the child for the watchdog's whole bound. Neither process
// honours SIGTERM, so the pre-#1058 path sat out DrainTime + shutdownGrace
// (here 15 s) before its SIGKILL. The fix kills at once.
func TestWatchdogKillsAtOnceWhenBothEpochsSilent(t *testing.T) {
	requireShell(t)
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	termLog := filepath.Join(t.TempDir(), "term.log")
	s := newInPodPair(t, clk, termStubEnvoy(t, termLog, false), 10*time.Second)

	clk.advance(4 * time.Second)
	s.noteAdminAnswer(1) // the child's LIVE answers ...
	clk.advance(3 * time.Second)
	s.noteAdminAnswer(1) // ... and its last one before the deadlock
	clk.advance(s.adminUnresponsiveDeadline() + time.Second)

	silence := s.handoffSilence(clk.now())
	require.True(t, silence.allSilent, "both epochs silent past the bound: %s", silence)
	assert.Equal(t, "0,1", silence.epochList())

	start := time.Now()
	s.stopWedged(context.Background(), silence)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 3*time.Second,
		"both epochs silent: the supervisor must SIGKILL at once, not wait DrainTime+shutdownGrace (%s)",
		s.cfg.DrainTime+shutdownGrace)
	assert.False(t, s.childTracked(0), "parent epoch must be killed and reaped")
	assert.False(t, s.childTracked(1), "child epoch must be killed and reaped")
	assert.Empty(t, readTermLog(t, termLog), "no SIGTERM is sent to a pair whose main threads cannot take it")
}

// TestWatchdogKeepsSigtermGraceWhenOneEpochAnswers covers the handoff
// watchdog's shape: the child never went LIVE (silent since its fork) while
// the admin kept answering as the parent. Only one epoch is silent — the
// parent's main thread demonstrably runs — so the normal SIGTERM + grace path
// applies and the parent gets its SIGTERM.
func TestWatchdogKeepsSigtermGraceWhenOneEpochAnswers(t *testing.T) {
	requireShell(t)
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	termLog := filepath.Join(t.TempDir(), "term.log")
	s := newInPodPair(t, clk, termStubEnvoy(t, termLog, true), time.Second)

	clk.advance(defaultHandoffDeadline)
	s.noteAdminAnswer(0) // the parent still answers
	clk.advance(time.Second)

	silence := s.handoffSilence(clk.now())
	require.False(t, silence.allSilent, "the parent answered a second ago: %s", silence)
	require.Len(t, silence.epochs, 2)
	assert.False(t, silence.epochs[0].silent, "parent epoch 0 answered recently")
	assert.True(t, silence.epochs[1].silent, "child epoch 1 has been silent since its fork")

	s.stopWedged(context.Background(), silence)

	assert.ElementsMatch(t, []string{"TERM_0", "TERM_1"}, readTermLog(t, termLog),
		"one silent epoch keeps the SIGTERM-then-grace path for every epoch")
	assert.False(t, s.childTracked(0))
	assert.False(t, s.childTracked(1))
}

// TestHandoffSilenceNeedsAPair: a single epoch with no handoff in flight is not
// "both epochs silent", however long it has been dark — every case other than
// the #1050 pair keeps SIGTERM + grace.
func TestHandoffSilenceNeedsAPair(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s := New(Config{StateDir: t.TempDir()}, slog.New(slog.DiscardHandler), nil)
	s.now = clk.now
	s.mu.Lock()
	s.children[0] = &exec.Cmd{}
	s.nextEpoch = 1
	s.epochLaunched = clk.now()
	s.mu.Unlock()
	s.noteAdminAnswer(0)

	clk.advance(10 * time.Minute)
	silence := s.handoffSilence(clk.now())
	assert.False(t, silence.allSilent)
	require.Len(t, silence.epochs, 1)
	assert.True(t, silence.epochs[0].silent)
}

// TestHandoffSilenceCrossPodSuccessor is the successor pod of #1050
// (aether-proxy-dwmng): it tracks only its own child, but the predecessor it
// attached to is part of the pair until this pod goes Ready.
func TestHandoffSilenceCrossPodSuccessor(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	fork := clk.now()
	s, _ := newGatedSuccessor(t, clk, 15*time.Second) // predecessor 3 answered at fork
	s.mu.Lock()
	s.children[4] = &exec.Cmd{}
	s.mu.Unlock()

	clk.set(fork.Add(4 * time.Second))
	s.noteAdminAnswer(4)
	ready := s.onLiveEpoch(context.Background(), 4, false)
	require.False(t, ready, "gate not passed yet")
	clk.advance(7 * time.Second)
	s.noteAdminAnswer(4) // last answer before the deadlock

	clk.advance(s.adminUnresponsiveDeadline())
	silence := s.handoffSilence(clk.now())
	assert.Equal(t, "3,4", silence.epochList(), "the predecessor is part of the pair until this pod is Ready")
	assert.True(t, silence.allSilent, "%s", silence)

	// A successor that went Ready has no predecessor left: back to one epoch.
	s2, _ := newGatedSuccessor(t, clk, 15*time.Second)
	s2.mu.Lock()
	s2.children[4] = &exec.Cmd{}
	s2.mu.Unlock()
	s2.noteAdminAnswer(4)
	require.False(t, s2.onLiveEpoch(context.Background(), 4, false), "first LIVE re-anchors the gate")
	clk.advance(time.Minute)
	require.True(t, s2.onLiveEpoch(context.Background(), 4, false), "gate long passed")
	clk.advance(time.Minute)
	silence = s2.handoffSilence(clk.now())
	assert.Equal(t, "4", silence.epochList())
	assert.False(t, silence.allSilent)
}

// TestHandoffSilenceCrossPodParent is the old pod of #1050: its tracked Envoy
// is the hot-restart parent and the admin port last answered as the surge
// successor's epoch. Both are in the pair.
func TestHandoffSilenceCrossPodParent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	s, _ := newHoldingParent(t, clk) // tracks epoch 1
	s.noteAdminAnswer(1)
	clk.advance(time.Second)
	s.noteAdminAnswer(2) // the successor took the admin port

	clk.advance(s.adminUnresponsiveDeadline())
	silence := s.handoffSilence(clk.now())
	assert.Equal(t, "1,2", silence.epochList())
	assert.True(t, silence.allSilent, "%s", silence)

	// The watchdog carries this verdict to Run.
	require.True(t, s.checkWedgeWatchdogs(context.Background(), 1, true, false, clk.now().Add(-2*s.adminUnresponsiveDeadline())))
	fire := <-s.watchdogFired
	require.Error(t, fire.err)
	assert.True(t, fire.silence.allSilent)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(sub string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), sub)
}

// newChildSilentSupervisor is a gated cross-pod successor at epoch 4 with
// metrics and a captured log, whose child is tracked.
func newChildSilentSupervisor(t *testing.T, clk *fakeClock) (*Supervisor, *syncBuffer, func() int64) {
	t.Helper()
	s, _ := newGatedSuccessor(t, clk, 15*time.Second)
	logs := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	m, reader := newTestSupervisorMetrics(t)
	s.metrics = m
	s.mu.Lock()
	s.children[4] = &exec.Cmd{}
	s.mu.Unlock()
	count := func() int64 {
		v, ok := metricValue(t, reader, "aether.supervisor.child_silent")
		require.True(t, ok, "the child-silent counter must be exported (seeded at zero)")
		return v
	}
	return s, logs, count
}

// TestChildSilentCountsOncePerEpoch: a successor LIVE at fork+4s whose admin
// goes dark 3 s later (the #1050 timeline) is reported after 10 s of silence —
// 20 s before the admin watchdog — exactly once for the epoch, however many
// ticks or streaks follow. The next epoch is counted on its own.
func TestChildSilentCountsOncePerEpoch(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
	fork := clk.now()
	s, logs, count := newChildSilentSupervisor(t, clk)
	ctx := context.Background()
	require.Zero(t, count(), "seeded at zero")

	clk.set(fork.Add(4 * time.Second))
	s.onLiveEpoch(ctx, 4, false)
	dark := fork.Add(7 * time.Second)

	for i := 1; i <= 9; i++ {
		clk.set(dark.Add(time.Duration(i) * time.Second))
		s.checkChildSilent(ctx, 4, false, dark)
	}
	assert.Zero(t, count(), "9 s of silence is inside the budget")

	clk.set(dark.Add(childSilentAfter))
	s.checkChildSilent(ctx, 4, false, dark)
	assert.EqualValues(t, 1, count())
	assert.Equal(t, 1, logs.count("hot-restart child silent"))
	assert.Equal(t, 1, logs.count(`msg="hot-restart child silent" epoch=4 silentSeconds=10 sinceLiveSeconds=13`))

	for i := 1; i <= 25; i++ {
		clk.advance(time.Second)
		s.checkChildSilent(ctx, 4, false, dark)
	}
	// A second streak in the same epoch, still inside the window's start.
	restreak := fork.Add(9 * time.Second)
	clk.set(restreak.Add(2 * childSilentAfter))
	s.checkChildSilent(ctx, 4, false, restreak)
	assert.EqualValues(t, 1, count(), "counted once per epoch")
	assert.Equal(t, 1, logs.count("hot-restart child silent"))

	// A later in-pod hot restart: epoch 5 is its own child.
	s.mu.Lock()
	s.children[5] = &exec.Cmd{}
	s.nextEpoch = 6
	s.epochLaunched = clk.now()
	s.epochLive = false
	s.epochLiveAt = time.Time{}
	s.mu.Unlock()
	clk.advance(2 * time.Second)
	s.onLiveEpoch(ctx, 5, true)
	dark5 := clk.now().Add(time.Second)
	clk.set(dark5.Add(childSilentAfter))
	s.checkChildSilent(ctx, 5, false, dark5)
	assert.EqualValues(t, 2, count())
}

// TestChildSilentNoFalsePositives covers the healthy cases that must never be
// counted: a slow successor dark for 20 s BEFORE its first LIVE (xDS-gated
// init, the handoff watchdog's business), a busy child dark for 9.9 s after
// LIVE that then answers, a dark streak that starts after the hot-restart
// window, an admin answering at another epoch, and a fresh epoch 0.
func TestChildSilentNoFalsePositives(t *testing.T) {
	ctx := context.Background()

	t.Run("slow pre-LIVE child", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
		fork := clk.now()
		s, _, count := newChildSilentSupervisor(t, clk)
		clk.set(fork.Add(20 * time.Second))
		s.checkChildSilent(ctx, 4, false, fork)
		assert.Zero(t, count(), "not expected to serve before its first LIVE")
		s.onLiveEpoch(ctx, 4, false)
		clk.advance(time.Second)
		s.checkChildSilent(ctx, 4, true, time.Time{})
		assert.Zero(t, count())
	})

	t.Run("busy child within budget", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
		fork := clk.now()
		s, _, count := newChildSilentSupervisor(t, clk)
		clk.set(fork.Add(4 * time.Second))
		s.onLiveEpoch(ctx, 4, false)
		dark := clk.now().Add(time.Second)
		clk.set(dark.Add(childSilentAfter - 100*time.Millisecond))
		s.checkChildSilent(ctx, 4, false, dark)
		clk.advance(time.Second)
		s.checkChildSilent(ctx, 4, true, time.Time{}) // answered again
		assert.Zero(t, count())
	})

	t.Run("dark after the hot-restart window", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
		fork := clk.now()
		s, _, count := newChildSilentSupervisor(t, clk)
		clk.set(fork.Add(4 * time.Second))
		s.onLiveEpoch(ctx, 4, false)
		dark := clk.now().Add(s.cfg.ParentShutdownTime + liveGateBuffer + time.Second)
		clk.set(dark.Add(childSilentAfter))
		s.checkChildSilent(ctx, 4, false, dark)
		assert.Zero(t, count(), "no longer a hot-restart child; the admin watchdog owns this")
	})

	t.Run("admin answering at another epoch", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
		fork := clk.now()
		s, _, count := newChildSilentSupervisor(t, clk)
		clk.set(fork.Add(4 * time.Second))
		s.onLiveEpoch(ctx, 4, false)
		clk.advance(time.Minute)
		s.checkChildSilent(ctx, 4, true, time.Time{})
		assert.Zero(t, count())
	})

	t.Run("fresh epoch 0 is not a hot-restart child", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_900_000_000, 0)}
		s := New(Config{StateDir: t.TempDir(), ParentShutdownTime: 15 * time.Second}, slog.New(slog.DiscardHandler), nil)
		s.now = clk.now
		m, reader := newTestSupervisorMetrics(t)
		s.metrics = m
		s.mu.Lock()
		s.children[0] = &exec.Cmd{}
		s.nextEpoch = 1
		s.mu.Unlock()
		s.markEpochLive()
		dark := clk.now()
		clk.advance(time.Minute)
		s.checkChildSilent(ctx, 0, false, dark)
		v, _ := metricValue(t, reader, "aether.supervisor.child_silent")
		assert.Zero(t, v)
	})
}
