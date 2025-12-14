package util

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/pkg/file"
	"github.com/fsnotify/fsnotify"
	ctrl "sigs.k8s.io/controller-runtime"
)

var utilLog = ctrl.Log

type Watcher struct {
	watcher *fsnotify.Watcher
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
	_ = w.watcher.Close()
}

// CreateFileWatcher creates a file watcher that watches for any changes to the directory
func CreateFileWatcher(paths ...string) (*Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher create: %v", err)
	}

	fileModified, errChan := make(chan struct{}), make(chan error)
	go watchFiles(watcher, fileModified, errChan)

	for _, path := range paths {
		if !file.Exists(path) {
			utilLog.Info("file watcher skipping watch on non-existent path", "path", path)
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
		Events:  fileModified,
		Errors:  errChan,
	}, nil
}

func watchFiles(watcher *fsnotify.Watcher, fileModified chan struct{}, errChan chan error) {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove) != 0 {
				utilLog.Info("file modified", "filename", event.Name)
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
