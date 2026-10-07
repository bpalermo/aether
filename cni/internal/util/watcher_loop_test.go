package util

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
)

// The watch loop's exits, one arm at a time (issue #1315).
//
// Watcher.Close closes `done` and then the fsnotify watcher, which closes its
// Events and Errors channels. The goroutine in watchFiles wakes with up to
// three arms ready and `select` picks among the ready ones at random, so which
// exit a Close takes — and which lines the tests in watcher_test.go cover — is
// decided by the scheduler: the closed-Errors exit was taken on the GitHub
// runner in every coverage run, in about a third of plain `go test` runs on a
// workstation, and in none under Bazel there. Six lines of the unit coverage
// number went with it.
//
// These tests hand watchFiles an fsnotify.Watcher that is only its two exported
// channels (no backend, never Closed), so exactly one arm is ready at a time
// and every exit is taken on every machine.

const loopTimeout = 5 * time.Second

type loop struct {
	events       chan fsnotify.Event
	errs         chan error
	fileModified chan struct{}
	errChan      chan error
	done         chan struct{}
	returned     chan struct{}
}

// startLoop runs watchFiles over bare channels. The fsnotify channels are
// unbuffered, so a completed send means the loop has received the value.
func startLoop(t *testing.T) *loop {
	t.Helper()

	l := &loop{
		events:       make(chan fsnotify.Event),
		errs:         make(chan error),
		fileModified: make(chan struct{}),
		errChan:      make(chan error),
		done:         make(chan struct{}),
		returned:     make(chan struct{}),
	}
	w := &fsnotify.Watcher{Events: l.events, Errors: l.errs}
	go func() {
		defer close(l.returned)
		watchFiles(slog.New(slog.DiscardHandler), w, l.fileModified, l.errChan, l.done)
	}()
	return l
}

func (l *loop) requireReturned(t *testing.T) {
	t.Helper()

	select {
	case <-l.returned:
	case <-time.After(loopTimeout):
		t.Fatal("watchFiles did not return")
	}
}

func (l *loop) requireRunning(t *testing.T) {
	t.Helper()

	select {
	case <-l.returned:
		t.Fatal("watchFiles returned; it should still be watching")
	case <-time.After(50 * time.Millisecond):
	}
}

func (l *loop) sendEvent(t *testing.T, ev fsnotify.Event) {
	t.Helper()

	select {
	case l.events <- ev:
	case <-time.After(loopTimeout):
		t.Fatal("watchFiles did not receive the event")
	}
}

func (l *loop) sendError(t *testing.T, err error) {
	t.Helper()

	select {
	case l.errs <- err:
	case <-time.After(loopTimeout):
		t.Fatal("watchFiles did not receive the error")
	}
}

// The exit #1315 is about: fsnotify closed its Errors channel and nothing else
// is ready.
func TestWatchFilesReturnsWhenErrorsChannelCloses(t *testing.T) {
	l := startLoop(t)

	close(l.errs)

	l.requireReturned(t)
}

// Its twin on the Events channel, which the same race decides.
func TestWatchFilesReturnsWhenEventsChannelCloses(t *testing.T) {
	l := startLoop(t)

	close(l.events)

	l.requireReturned(t)
}

func TestWatchFilesReturnsOnDone(t *testing.T) {
	l := startLoop(t)

	close(l.done)

	l.requireReturned(t)
}

// An fsnotify error reaches the caller's Errors channel unchanged and the loop
// keeps watching.
func TestWatchFilesForwardsError(t *testing.T) {
	l := startLoop(t)
	boom := errors.New("inotify queue overflow")

	l.sendError(t, boom)
	select {
	case got := <-l.errChan:
		require.ErrorIs(t, got, boom)
	case <-time.After(loopTimeout):
		t.Fatal("the error was not forwarded")
	}
	l.requireRunning(t)

	close(l.done)
	l.requireReturned(t)
}

// The error send has no guaranteed receiver either (issue #772, finding S31):
// done must release a loop parked in it.
func TestWatchFilesParkedInErrorSendStopsOnDone(t *testing.T) {
	l := startLoop(t)

	// Nobody reads errChan, so the loop parks in the send.
	l.sendError(t, errors.New("nobody is listening"))
	l.requireRunning(t)

	close(l.done)
	l.requireReturned(t)
}

// Only Create, Write and Remove are modifications; anything else (a Chmod) is
// dropped and the loop keeps watching.
func TestWatchFilesIgnoresOtherOps(t *testing.T) {
	l := startLoop(t)

	l.sendEvent(t, fsnotify.Event{Name: "10-aether.conflist", Op: fsnotify.Chmod})
	// A second receive proves the Chmod was not turned into a parked send.
	l.sendEvent(t, fsnotify.Event{Name: "10-aether.conflist", Op: fsnotify.Write})
	select {
	case <-l.fileModified:
	case <-time.After(loopTimeout):
		t.Fatal("the Write was not delivered")
	}
	select {
	case <-l.fileModified:
		t.Fatal("the Chmod was delivered as a modification")
	case <-time.After(50 * time.Millisecond):
	}

	close(l.done)
	l.requireReturned(t)
}
