package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/encoding/protojson"
)

type CNIWatcher struct {
	watcher      *fsnotify.Watcher
	registryPath string
	logger       logr.Logger

	initWg    *sync.WaitGroup
	eventChan chan<- *registryv1.Event
}

func NewCNIWatcher(registryPath string, initWg *sync.WaitGroup, eventChan chan<- *registryv1.Event, logger logr.Logger) (*CNIWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	rw := &CNIWatcher{
		watcher:      watcher,
		registryPath: registryPath,
		logger:       logger.WithName("cni-watcher"),
		initWg:       initWg,
		eventChan:    eventChan,
	}

	// Add the registry path to watch
	if err = watcher.Add(registryPath); err != nil {
		err = watcher.Close()
		if err != nil {
			rw.logger.Error(err, "failed to close watcher")
			return nil, err
		}

		rw.logger.Error(err, "failed to add registry path to watcher")
		return nil, err
	}

	return rw, nil
}

func (rw *CNIWatcher) Start(ctx context.Context) error {
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

func (rw *CNIWatcher) handleEvent(event fsnotify.Event) {
	// Only process JSON files
	if filepath.Ext(event.Name) != ".json" {
		return
	}

	var registryEvent *registryv1.Event

	switch {
	case event.Op&fsnotify.Create == fsnotify.Create:
		rw.logger.Info("file created", "file", event.Name)
		entry := &registryv1.RegistryEntry{}
		if err := rw.unmarshalFile(event.Name, entry); err != nil {
			rw.logger.Error(err, "failed to process created file", "file", event.Name)
			return
		}
		registryEvent = eventFromEntry(registryv1.Event_CREATED, entry)

	case event.Op&fsnotify.Write == fsnotify.Write:
		rw.logger.Info("file modified", "file", event.Name)
		entry := &registryv1.RegistryEntry{}
		if err := rw.unmarshalFile(event.Name, entry); err != nil {
			rw.logger.Error(err, "failed to process modified file", "file", event.Name)
			return
		}
		registryEvent = eventFromEntry(registryv1.Event_UPDATED, entry)

	case event.Op&fsnotify.Remove == fsnotify.Remove:
		rw.logger.Info("file removed", "file", event.Name)
		entry := &registryv1.RegistryEntry{}
		if err := rw.unmarshalFile(event.Name, entry); err != nil {
			rw.logger.Error(err, "failed to process deleted file", "file", event.Name)
			return
		}
		registryEvent = eventFromEntry(registryv1.Event_DELETED, entry)
	}

	// Send event to the channel if created
	if registryEvent != nil {
		select {
		case rw.eventChan <- registryEvent:
			// Event sent successfully
		default:
			rw.logger.Info("event channel full, dropping event", "file", event.Name)
		}
	}
}

func (rw *CNIWatcher) processExistingFiles() error {
	files, err := filepath.Glob(filepath.Join(rw.registryPath, "*.json"))
	if err != nil {
		return fmt.Errorf("failed to list existing files: %w", err)
	}

	var errs []error
	for _, file := range files {
		// Process each existing file as if it was just created
		entry := &registryv1.RegistryEntry{}
		if err = rw.unmarshalFile(file, entry); err != nil {
			rw.logger.Error(err, "failed to process existing file", "file", file)
			errs = append(errs, fmt.Errorf("failed to process %s: %w", file, err))
			continue
		}
		registryEvent := eventFromEntry(registryv1.Event_CREATED, entry)
		select {
		case rw.eventChan <- registryEvent:
			// Event sent successfully
		default:
			rw.logger.Info("event channel full, dropping event", "path", registryEvent.GetNetworkNs().GetPath())
		}
	}

	rw.initWg.Done()

	return errors.Join(errs...)
}

func (rw *CNIWatcher) unmarshalFile(file string, entry *registryv1.RegistryEntry) error {
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

	if err := protojson.Unmarshal(data, entry); err != nil {
		return fmt.Errorf("failed to unmarshal file %s: %w", file, err)
	}

	rw.logger.Info("processing pod",
		"name", entry.PodName,
		"namespace", entry.PodNs,
		"netns", entry.NetworkNs,
	)

	return nil
}

func eventFromEntry(op registryv1.Event_Operation, entry *registryv1.RegistryEntry) *registryv1.Event {
	return &registryv1.Event{
		Operation: op,
		Resource: &registryv1.Event_NetworkNs{
			NetworkNs: &registryv1.Event_NetworkNamespace{
				Pod: &registryv1.Event_Pod{
					Name:      entry.PodName,
					Namespace: entry.PodNs,
				},
				Path: entry.NetworkNs,
			},
		},
	}

}
