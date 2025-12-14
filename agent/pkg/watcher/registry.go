package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/encoding/protojson"
)

type RegistryWatcher struct {
	watcher      *fsnotify.Watcher
	registryPath string
	logger       logr.Logger

	initializationChan chan struct{} // bidirectional internally
	entryChan          chan *registryv1.RegistryEntry
}

func NewRegistryWatcher(registryPath string, logger logr.Logger) (*RegistryWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	rw := &RegistryWatcher{
		watcher:            watcher,
		registryPath:       registryPath,
		logger:             logger.WithName("registry-watcher"),
		initializationChan: make(chan struct{}),
		entryChan:          make(chan *registryv1.RegistryEntry),
	}

	// Add the registry path to watch
	if err := watcher.Add(registryPath); err != nil {
		err := watcher.Close()
		if err != nil {
			rw.logger.Error(err, "failed to close watcher")
			return nil, err
		}

		rw.logger.Error(err, "failed to add registry path to watcher")
		return nil, err
	}

	return rw, nil
}

func (rw *RegistryWatcher) Start(ctx context.Context) error {
	rw.logger.Info("starting registry watcher", "path", rw.registryPath)
	defer func(watcher *fsnotify.Watcher) {
		err := watcher.Close()
		if err != nil {
			rw.logger.Error(err, "failed to close watcher")
		}
	}(rw.watcher)

	// Process existing files first
	if err := rw.processExistingFiles(); err != nil {
		rw.logger.Error(err, "failed to process existing files")
		return err
	}

	for {
		select {
		case <-ctx.Done():
			rw.logger.Info("stopping registry watcher due to context cancellation")
			return ctx.Err()

		case event, ok := <-rw.watcher.Events:
			if !ok {
				return fmt.Errorf("watcher events channel closed")
			}
			rw.handleEvent(event)

		case err, ok := <-rw.watcher.Errors:
			if !ok {
				return fmt.Errorf("watcher errors channel closed")
			}
			rw.logger.Error(err, "watcher error")
		}
	}
}

func (rw *RegistryWatcher) GetInitializationChan() <-chan struct{} {
	return rw.initializationChan
}

func (rw *RegistryWatcher) GetEntryChan() <-chan *registryv1.RegistryEntry {
	return rw.entryChan
}

func (rw *RegistryWatcher) handleEvent(event fsnotify.Event) {
	// Only process JSON files
	if filepath.Ext(event.Name) != ".json" {
		return
	}

	switch {
	case event.Op&fsnotify.Create == fsnotify.Create:
		rw.logger.Info("file created", "file", event.Name)
		if err := rw.processFile(event.Name); err != nil {
			rw.logger.Error(err, "failed to process created file", "file", event.Name)
		}

	case event.Op&fsnotify.Write == fsnotify.Write:
		rw.logger.Info("file modified", "file", event.Name)
		if err := rw.processFile(event.Name); err != nil {
			rw.logger.Error(err, "failed to process modified file", "file", event.Name)
		}

	case event.Op&fsnotify.Remove == fsnotify.Remove:
		rw.logger.Info("file removed", "file", event.Name)
		// Handle pod deletion

	case event.Op&fsnotify.Rename == fsnotify.Rename:
		rw.logger.Info("file renamed", "file", event.Name)
		// Handle the rename if needed
	}
}

func (rw *RegistryWatcher) processExistingFiles() error {
	files, err := filepath.Glob(filepath.Join(rw.registryPath, "*.json"))
	if err != nil {
		return fmt.Errorf("failed to list existing files: %w", err)
	}

	var errs []error
	for _, file := range files {
		// Process each existing file as if it was just created
		if err = rw.processFile(file); err != nil {
			rw.logger.Error(err, "failed to process existing file", "file", file)
			errs = append(errs, fmt.Errorf("failed to process %s: %w", file, err))
		}
	}

	if rw.initializationChan != nil {
		close(rw.initializationChan)
		rw.initializationChan = nil
	}

	return errors.Join(errs...)
}

func (rw *RegistryWatcher) processFile(file string) error {
	rw.logger.Info("processing file", "file", file)

	// Read the file contents
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("failed to read file %s: %w", file, err)
	}

	// Skip empty files
	if len(data) == 0 {
		rw.logger.Info("skipping empty file", "file", file)
		return nil
	}

	entry := &registryv1.RegistryEntry{}
	if err := protojson.Unmarshal(data, entry); err != nil {
		return fmt.Errorf("failed to unmarshal file %s: %w", file, err)
	}

	rw.logger.Info("processing pod",
		"name", entry.PodName,
		"namespace", entry.PodNs,
		"netns", entry.NetworkNs,
	)

	// Send entry to channel
	rw.entryChan <- entry

	return nil
}
