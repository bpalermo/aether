package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

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

	// Cache for deduplication
	processedFiles sync.Map // map[string]time.Time

	// Buffer pool for file reading
	bufferPool sync.Pool
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
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 4096) // 4KB buffer
			},
		},
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
	if err := rw.processExistingFiles(ctx); err != nil {
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
		if err := rw.unmarshalFileOptimized(event.Name, entry); err != nil {
			rw.logger.Error(err, "failed to process created file", "file", event.Name)
			return
		}
		registryEvent = eventFromEntry(registryv1.Event_CREATED, entry)

	case event.Op&fsnotify.Write == fsnotify.Write:
		rw.logger.Info("file modified", "file", event.Name)
		entry := &registryv1.RegistryEntry{}
		if err := rw.unmarshalFileOptimized(event.Name, entry); err != nil {
			rw.logger.Error(err, "failed to process modified file", "file", event.Name)
			return
		}
		registryEvent = eventFromEntry(registryv1.Event_UPDATED, entry)

	case event.Op&fsnotify.Remove == fsnotify.Remove:
		rw.logger.Info("file removed", "file", event.Name)
		entry := &registryv1.RegistryEntry{}
		if err := rw.unmarshalFileOptimized(event.Name, entry); err != nil {
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

func (rw *CNIWatcher) processExistingFiles(ctx context.Context) error {
	files, err := filepath.Glob(filepath.Join(rw.registryPath, "*.json"))
	if err != nil {
		return fmt.Errorf("failed to list existing files: %w", err)
	}

	// Process files concurrently with the worker pool
	const workers = 4
	fileChan := make(chan string, len(files))
	errChan := make(chan error, len(files))
	var wg sync.WaitGroup

	// Start workers
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for file := range fileChan {
				select {
				case <-ctx.Done():
					return
				default:
					if err := rw.processFile(file); err != nil {
						errChan <- err
					}
				}
			}
		}()
	}

	// Queue files
	for _, file := range files {
		fileChan <- file
	}
	close(fileChan)

	// Wait for workers
	wg.Wait()
	close(errChan)

	// Collect errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	rw.initWg.Done()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (rw *CNIWatcher) processFile(file string) error {
	entry := &registryv1.RegistryEntry{}
	if err := rw.unmarshalFileOptimized(file, entry); err != nil {
		rw.logger.Error(err, "failed to process existing file", "file", file)
		return fmt.Errorf("failed to process %s: %w", file, err)
	}

	registryEvent := eventFromEntry(registryv1.Event_CREATED, entry)

	// Try to send with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	select {
	case rw.eventChan <- registryEvent:
	case <-ctx.Done():
		rw.logger.Info("event channel full, dropping event", "path", registryEvent.GetNetworkNs().GetPath())
	}

	return nil
}

func (rw *CNIWatcher) unmarshalFileOptimized(file string, entry *registryv1.RegistryEntry) error {
	// Get buffer from the pool
	buf := rw.bufferPool.Get().([]byte)
	defer rw.bufferPool.Put(buf)

	// Open file
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File was deleted, ignore
		}
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	// Read file with size limit
	const maxSize = 10 * 1024 // 10KB max
	data := buf[:0]
	n, err := io.ReadFull(f, buf[:cap(buf)])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && err != io.EOF {
		return fmt.Errorf("failed to read file: %w", err)
	}
	data = buf[:n]

	// Skip empty files
	if len(data) == 0 {
		rw.logger.Info("skipping empty file", "file", file)
		return nil
	}

	// Check if the file is too large
	if n == cap(buf) {
		// Try to read one more byte to check if the file is larger
		if _, err := f.Read(make([]byte, 1)); err != io.EOF {
			return fmt.Errorf("file too large (>%d bytes)", maxSize)
		}
	}

	if err := protojson.Unmarshal(data, entry); err != nil {
		return fmt.Errorf("failed to unmarshal: %w", err)
	}

	rw.logger.Info("processed pod",
		"name", entry.PodName,
		"namespace", entry.PodNs,
		"netns", entry.NetworkNs,
	)

	// Mark as processed
	rw.processedFiles.Store(file, time.Now())

	return nil
}

func eventFromEntry(op registryv1.Event_Operation, entry *registryv1.RegistryEntry) *registryv1.Event {
	return &registryv1.Event{
		Operation: op,
		Resource: &registryv1.Event_NetworkNs{
			NetworkNs: &registryv1.Event_NetworkNamespace{
				Pod: &registryv1.Event_KubernetesPod{
					Name:      entry.PodName,
					Namespace: entry.PodNs,
				},
				Path: entry.NetworkNs,
			},
		},
	}

}

func (rw *CNIWatcher) cleanupCache(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			rw.processedFiles.Range(func(key, value interface{}) bool {
				if now.Sub(value.(time.Time)) > 10*time.Minute {
					rw.processedFiles.Delete(key)
				}
				return true
			})
		}
	}
}
