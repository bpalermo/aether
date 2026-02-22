package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/types"
	"github.com/bpalermo/aether/common/file"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

type CachedLocalStorage[T proto.Message] struct {
	basePath    string
	cache       map[types.ContainerID]T
	mu          sync.RWMutex
	newFunc     func() T // Factory function to create new instances
	initialized bool
	initOnce    sync.Once
	initDone    chan struct{}
	initErr     error
}

// NewCachedLocalStorage creates a new cached file storage
// Resources are stored as individual files in the specified basePath and in memory
func NewCachedLocalStorage[T proto.Message](basePath string, newFunc func() T) *CachedLocalStorage[T] {
	return &CachedLocalStorage[T]{
		basePath: basePath,
		cache:    make(map[types.ContainerID]T),
		newFunc:  newFunc,
		initDone: make(chan struct{}),
	}
}

// Initialize loads all resources from the disk into the cache.
// Safe to call multiple times - only the first call performs initialization.
func (f *CachedLocalStorage[T]) Initialize(ctx context.Context) error {
	f.initOnce.Do(func() {
		_, f.initErr = f.loadAll(ctx)
		if f.initErr == nil {
			f.mu.Lock()
			f.initialized = true
			f.mu.Unlock()
		}
		close(f.initDone)
	})
	return f.initErr
}

// WaitUntilReady blocks until the cache is populated or context is canceled.
func (f *CachedLocalStorage[T]) WaitUntilReady(ctx context.Context) error {
	select {
	case <-f.initDone:
		return f.initErr
	case <-ctx.Done():
		return fmt.Errorf("initialization cancelled: %w", ctx.Err())
	}
}

func (f *CachedLocalStorage[T]) AddResource(_ context.Context, key types.ContainerID, resource T) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Marshal the protobuf message
	data, err := protojson.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to marshal resource: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(f.basePath, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	filePath := filepath.Join(f.basePath, key.String()+".json")

	// Write to disk atomically
	if err := file.WriteFileAtomic(filePath, data); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Update in-memory cache
	f.cache[key] = resource

	return nil
}

func (f *CachedLocalStorage[T]) RemoveResource(_ context.Context, key types.ContainerID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Remove from disk
	filePath := filepath.Join(f.basePath, key.String()+".json")
	if err := os.Remove(filePath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file: %w", err)
		}
	}

	// Remove from cache
	delete(f.cache, key)

	return nil
}

func (f *CachedLocalStorage[T]) GetResource(_ context.Context, key types.ContainerID) (T, error) {
	f.mu.RLock()

	// Check cache first
	if resource, ok := f.cache[key]; ok {
		f.mu.RUnlock()
		return resource, nil
	}
	f.mu.RUnlock()

	// Not in cache, load from disk
	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check cache after acquiring write lock
	if resource, ok := f.cache[key]; ok {
		return resource, nil
	}

	var resource T
	filePath := filepath.Join(f.basePath, key.String()+".json")

	data, err := os.ReadFile(filePath)
	if err != nil {
		return resource, fmt.Errorf("failed to read file: %w", err)
	}

	// Create a new instance of T using the factory function
	resource = f.newFunc()
	if err := unmarshalOpts.Unmarshal(data, resource); err != nil {
		return resource, fmt.Errorf("failed to unmarshal resource: %w", err)
	}

	// Store in the cache for future access
	f.cache[key] = resource

	return resource, nil
}

// GetAll returns all resources from the cache as a slice
func (f *CachedLocalStorage[T]) GetAll(_ context.Context) ([]T, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Create a slice with all cached values
	result := make([]T, 0, len(f.cache))
	for _, v := range f.cache {
		result = append(result, v)
	}

	return result, nil
}

// loadAll loads all resources from the disk into the memory cache
func (f *CachedLocalStorage[T]) loadAll(_ context.Context) ([]T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	entries, err := os.ReadDir(f.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	var resources []T
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		key := entry.Name()[:len(entry.Name())-5] // Remove .json extension
		filePath := filepath.Join(f.basePath, entry.Name())

		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
		}

		resource := f.newFunc()
		if err := unmarshalOpts.Unmarshal(data, resource); err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource from %s: %w", filePath, err)
		}

		f.cache[types.ContainerID(key)] = resource
		resources = append(resources, resource)
	}

	return resources, nil
}
