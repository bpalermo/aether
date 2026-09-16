package util_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"aethermesh.dev/cni/internal/util"
	"github.com/stretchr/testify/require"
)

// requireNoExtraGoroutines polls until the live goroutine count is back to
// baseline. Polling (rather than a single sample) keeps the assertion honest:
// a goroutine returning from a select is not instantaneous, and the runtime's
// own workers come and go.
func requireNoExtraGoroutines(t *testing.T, baseline int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: %d goroutines, baseline %d\n%s", n, baseline, buf)
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWatcherCloseStopsGoroutineParkedInSend is the regression test for issue
// #772 finding S31: watchFiles sent on an unbuffered channel with no stop arm,
// so once a caller gave up on Wait (its context was cancelled) the goroutine
// parked in the send forever. Close closed the fsnotify watcher, but a
// goroutine blocked in a send never returns to the select to notice.
//
// The production shape is exactly this: cni/internal/install/cniconfig.go's
// getCNIConfigFilepath does `defer watcher.Close()` and abandons Wait on
// context cancellation.
func TestWatcherCloseStopsGoroutineParkedInSend(t *testing.T) {
	baseline := runtime.NumGoroutine()

	dir := t.TempDir()
	w, err := util.CreateFileWatcher(slog.New(slog.DiscardHandler), dir)
	require.NoError(t, err)

	// Nobody is inside Wait, so this write parks the goroutine in its send.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "10-aether.conflist"), []byte("{}"), 0o600))

	// Give the goroutine time to pick the event up and block on the send.
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() > baseline
	}, 5*time.Second, 10*time.Millisecond, "watch goroutine never started")
	time.Sleep(100 * time.Millisecond)

	w.Close()

	requireNoExtraGoroutines(t, baseline)
}

// TestWatcherCloseIsIdempotent covers the second Close a deferred cleanup can
// produce alongside an explicit one.
func TestWatcherCloseIsIdempotent(t *testing.T) {
	baseline := runtime.NumGoroutine()

	w, err := util.CreateFileWatcher(slog.New(slog.DiscardHandler), t.TempDir())
	require.NoError(t, err)

	w.Close()
	w.Close()

	requireNoExtraGoroutines(t, baseline)
}

// TestWatcherWaitDelivers is the happy path: the added stop arm must not
// swallow real events.
func TestWatcherWaitDelivers(t *testing.T) {
	baseline := runtime.NumGoroutine()

	dir := t.TempDir()
	w, err := util.CreateFileWatcher(slog.New(slog.DiscardHandler), dir)
	require.NoError(t, err)

	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		waited <- w.Wait(ctx)
	}()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "10-aether.conflist"), []byte("{}"), 0o600))
	select {
	case err := <-waited:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not observe the file modification")
	}

	w.Close()
	requireNoExtraGoroutines(t, baseline)
}

// TestWatcherWaitHonoursContext is the abandonment path the leak depends on:
// Wait returns the context error and the caller walks away, leaving the
// goroutine with nobody to send to. Uses its own quiescent watcher — a select
// with a ready Events arm picks between arms at random, so this assertion must
// not share a directory with a test that writes files.
func TestWatcherWaitHonoursContext(t *testing.T) {
	baseline := runtime.NumGoroutine()

	w, err := util.CreateFileWatcher(slog.New(slog.DiscardHandler), t.TempDir())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.Wait(ctx), context.Canceled)

	w.Close()
	requireNoExtraGoroutines(t, baseline)
}
