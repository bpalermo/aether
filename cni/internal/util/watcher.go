package util

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"aethermesh.dev/common/file"
	"github.com/fsnotify/fsnotify"
)

// Watcher wraps an fsnotify watcher and logs through the caller's logger. It
// used to log through controller-runtime's global logger, which nothing in cni/
// ever binds, so every one of those records was discarded (issue #696).
type Watcher struct {
	watcher *fsnotify.Watcher
	logger  *slog.Logger
	Events  chan struct{}
	Errors  chan error

	// done is closed by Close to unblock the watchFiles goroutine. Both of its
	// sends are on unbuffered channels with no guaranteed receiver — callers
	// use Wait with a context and walk away on cancellation — so without a stop
	// arm the goroutine parks in a send forever (issue #772, finding S31).
	done      chan struct{}
	closeOnce sync.Once
}

// Wait waits until a file is modified (returns nil), the context is cancelled (returns context error), or returns error
func (w *Watcher) Wait(ctx context.Context) error {
	select {
	case <-w.Events:
		return nil
	case err := <-w.Errors:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the watch goroutine and releases the fsnotify watcher. It is
// idempotent and safe to call while a Wait is in flight.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		// Close done first: the goroutine may be parked in a send that closing
		// the fsnotify watcher would not release.
		close(w.done)
		if err := w.watcher.Close(); err != nil {
			w.logger.Debug("failed to close file watcher", "error", err)
		}
	})
}

// CreateFileWatcher creates a file watcher that watches for any changes to the directory
func CreateFileWatcher(logger *slog.Logger, paths ...string) (*Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher create: %v", err)
	}

	fileModified, errChan := make(chan struct{}), make(chan error)
	done := make(chan struct{})
	go watchFiles(logger, watcher, fileModified, errChan, done)

	for _, path := range paths {
		if !file.Exists(path) {
			logger.Info("file watcher skipping watch on non-existent path", "path", path)
			continue
		}
		if err := watcher.Add(path); err != nil {
			if closeErr := watcher.Close(); closeErr != nil {
				err = fmt.Errorf("%s: %w", closeErr.Error(), err)
			}
			// The caller never gets a Watcher on this path, so stop the
			// goroutine here rather than leaking it.
			close(done)
			return nil, err
		}
	}

	return &Watcher{
		watcher: watcher,
		logger:  logger,
		Events:  fileModified,
		Errors:  errChan,
		done:    done,
	}, nil
}

// sendOrStop delivers v on ch, or gives up if done closes first. It reports
// whether the value was delivered; false means the watcher was closed and the
// caller must return rather than park in the send.
func sendOrStop[T any](ch chan<- T, v T, done <-chan struct{}) bool {
	select {
	case ch <- v:
		return true
	case <-done:
		return false
	}
}

// deliverEvent handles one receive from the fsnotify event channel. It reports
// whether the watch loop should keep going: false means the channel closed or
// the watcher was closed mid-send.
func deliverEvent(logger *slog.Logger, event fsnotify.Event, ok bool, fileModified chan<- struct{}, done <-chan struct{}) bool {
	if !ok {
		return false
	}
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove) == 0 {
		return true
	}
	logger.Info("file modified", "filename", event.Name)
	return sendOrStop(fileModified, struct{}{}, done)
}

// deliverError is deliverEvent's counterpart for the fsnotify error channel.
func deliverError(err error, ok bool, errChan chan<- error, done <-chan struct{}) bool {
	if !ok {
		return false
	}
	return sendOrStop(errChan, err, done)
}

func watchFiles(logger *slog.Logger, watcher *fsnotify.Watcher, fileModified chan struct{}, errChan chan error, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case event, ok := <-watcher.Events:
			if !deliverEvent(logger, event, ok, fileModified, done) {
				return
			}
		case err, ok := <-watcher.Errors:
			if !deliverError(err, ok, errChan, done) {
				return
			}
		}
	}
}
