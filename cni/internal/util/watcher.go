package util

import (
	"context"
	"fmt"
	"log/slog"

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

func (w *Watcher) Close() {
	if err := w.watcher.Close(); err != nil {
		w.logger.Debug("failed to close file watcher", "error", err)
	}
}

// CreateFileWatcher creates a file watcher that watches for any changes to the directory
func CreateFileWatcher(logger *slog.Logger, paths ...string) (*Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher create: %v", err)
	}

	fileModified, errChan := make(chan struct{}), make(chan error)
	go watchFiles(logger, watcher, fileModified, errChan)

	for _, path := range paths {
		if !file.Exists(path) {
			logger.Info("file watcher skipping watch on non-existent path", "path", path)
			continue
		}
		if err := watcher.Add(path); err != nil {
			if closeErr := watcher.Close(); closeErr != nil {
				err = fmt.Errorf("%s: %w", closeErr.Error(), err)
			}
			return nil, err
		}
	}

	return &Watcher{
		watcher: watcher,
		logger:  logger,
		Events:  fileModified,
		Errors:  errChan,
	}, nil
}

func watchFiles(logger *slog.Logger, watcher *fsnotify.Watcher, fileModified chan struct{}, errChan chan error) {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove) != 0 {
				logger.Info("file modified", "filename", event.Name)
				fileModified <- struct{}{}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			errChan <- err
		}
	}
}
