package signals

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitFor is the ceiling on signal delivery; every wait below is expected to
// complete in microseconds, so this only bounds a failure.
const waitFor = 5 * time.Second

// raise sends sig to this process. Every test that calls it holds a live
// registration first — an unhandled SIGTERM would take the test binary down.
func raise(t *testing.T, sig syscall.Signal) {
	t.Helper()
	require.NoError(t, syscall.Kill(syscall.Getpid(), sig))
}

// TestFirstSignalCancelsSecondInvokesCallback pins the two-stage contract this
// package exists to preserve: the first signal only cancels (so the caller's own
// drain runs), and the second is what escalates.
func TestFirstSignalCancelsSecondInvokesCallback(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			second := make(chan struct{})
			ctx, stop := NotifyContextFunc(t.Context(), func() { close(second) })
			defer stop()

			raise(t, sig)
			select {
			case <-ctx.Done():
			case <-time.After(waitFor):
				t.Fatal("first signal did not cancel the context")
			}

			// The first signal must NOT escalate: the whole point is that the
			// caller gets a drain window.
			select {
			case <-second:
				t.Fatal("first signal invoked the second-signal callback")
			case <-time.After(50 * time.Millisecond):
			}

			raise(t, sig)
			select {
			case <-second:
			case <-time.After(waitFor):
				t.Fatal("second signal did not invoke the callback")
			}
		})
	}
}

// TestNotifyContextExitsOnSecondSignal asserts the default reaction is
// controller-runtime's: os.Exit(1), not a graceful return.
func TestNotifyContextExitsOnSecondSignal(t *testing.T) {
	codes := make(chan int, 1)
	orig := exitFunc
	exitFunc = func(code int) { codes <- code }
	t.Cleanup(func() { exitFunc = orig })

	ctx, stop := NotifyContext(t.Context())
	defer stop()

	raise(t, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-time.After(waitFor):
		t.Fatal("first signal did not cancel the context")
	}

	raise(t, syscall.SIGTERM)
	select {
	case code := <-codes:
		require.Equal(t, 1, code, "second signal must exit non-zero")
	case <-time.After(waitFor):
		t.Fatal("second signal did not exit")
	}
}

// TestStopCancelsAndIsIdempotent covers the deferred-stop path: no signal is ever
// raised, so this also proves the handler is released rather than leaked.
func TestStopCancelsAndIsIdempotent(t *testing.T) {
	ctx, stop := NotifyContextFunc(context.Background(), func() { t.Error("callback ran without a signal") })
	stop()
	stop() // must not panic on a double close

	select {
	case <-ctx.Done():
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	case <-time.After(waitFor):
		t.Fatal("stop did not cancel the context")
	}
}

// TestParentCancellationPropagates keeps the context composable: a caller that
// wires in its own deadline must still see it.
func TestParentCancellationPropagates(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, stop := NotifyContextFunc(parent, nil)
	defer stop()

	cancelParent()
	select {
	case <-ctx.Done():
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	case <-time.After(waitFor):
		t.Fatal("parent cancellation did not propagate")
	}
}
