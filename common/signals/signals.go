// Package signals turns the process's termination signals into a context.
//
// It exists so a binary does not have to link controller-runtime for
// `ctrl.SetupSignalHandler()` alone. That one import used to drag all of
// client-go, apimachinery, kube-openapi and prometheus/client_golang into
// //cni/cmd/cni-install and //prober/cmd/prober — 589 external packages and 56
// modules for a ten-line signal handler (issue #772, phase B1). This package has
// ZERO external modules: keep it that way, it is the whole point.
//
// The behaviour is deliberately identical to controller-runtime's handler and to
// the copy //agent/cmd/mesh-dns grew independently:
//
//   - the FIRST SIGINT/SIGTERM cancels the returned context, so the process runs
//     its own graceful shutdown;
//   - the SECOND one is the operator saying "stop waiting". By default that exits
//     the process with status 1 (what controller-runtime does); a caller with a
//     drain window of its own — mesh-DNS's lame duck, issue #729 — passes
//     NotifyContextFunc a callback and decides for itself.
//
// Signal notification stays registered after cancellation, exactly as
// signal.NotifyContext leaves it: a third signal then lands in the (full) buffered
// channel and is swallowed rather than killing the process mid-drain.
package signals

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// shutdownSignals are the signals that mean "terminate": SIGINT (an interactive
// Ctrl-C) and SIGTERM (what the kubelet sends at the start of pod termination).
var shutdownSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// exitFunc is os.Exit, indirected so a test can observe the second-signal exit
// without taking the test binary down with it.
var exitFunc = os.Exit

// NotifyContext returns a context cancelled by the first SIGINT/SIGTERM; a second
// one exits the process with status 1. This is the drop-in replacement for
// controller-runtime's ctrl.SetupSignalHandler() and matches it exactly, including
// the hard exit.
//
// Unlike controller-runtime's, it may be called more than once per process (that
// one panics on a second call) and it takes a parent context, so a caller can
// compose it with a deadline or a cobra command context.
//
// The returned stop() unregisters the handlers and cancels the context; call it
// (deferred) when the caller is done, or the goroutine lives until process exit.
func NotifyContext(parent context.Context) (context.Context, context.CancelFunc) {
	return NotifyContextFunc(parent, func() { exitFunc(1) })
}

// NotifyContextFunc is NotifyContext with a caller-supplied reaction to the second
// signal instead of the default exit. onSecond runs at most once, on the goroutine
// watching the signals, and must not block for long — the operator sent a second
// signal because the first one did not get them out fast enough.
//
// A nil onSecond makes the second signal inert (it is still consumed, so it does
// not kill the process).
func NotifyContextFunc(parent context.Context, onSecond func()) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	// Buffered for two: the first signal cancels ctx, the second reaches onSecond
	// even if it arrives while the goroutine is still handling the first.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, shutdownSignals...)

	// done makes stop() release the goroutine instead of leaving it parked on a
	// signal that will never come.
	done := make(chan struct{})

	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
			// Parent cancelled, or stop() was called; keep watching for the
			// operator's "stop waiting" signal either way.
		case <-done:
			return
		}
		if onSecond == nil {
			return
		}
		select {
		case <-ch:
			onSecond()
		case <-done:
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
		})
		cancel()
	}
	return ctx, stop
}
