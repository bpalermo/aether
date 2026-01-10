package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/util"
	"google.golang.org/protobuf/proto"
)

type CachedFileStorage[T proto.Message] struct {
	basePath string
	cache    map[string]T
	mu       sync.RWMutex
	newFunc  func() T // Factory function to create new instances
}

// NewFileStorage creates a new cached file storage
// Resources are stored as individual files in the specified basePath and in memory
func NewFileStorage[T proto.Message](basePath string, newFunc func() T) *CachedFileStorage[T] {
	return &CachedFileStorage[T]{
		basePath: basePath,
		cache:    make(map[string]T),
		newFunc:  newFunc,
	}
}

func (f *CachedFileStorage[T]) AddResource(key string, resource T) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Marshal the protobuf message
	data, err := proto.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to marshal resource: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(f.basePath, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	filePath := filepath.Join(f.basePath, key+".pb")

	// Write to disk atomically
	if err := util.WriteFileAtomic(filePath, data); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Update in-memory cache
	f.cache[key] = resource

	return nil
}

func (f *CachedFileStorage[T]) RemoveResource(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Remove from disk
	filePath := filepath.Join(f.basePath, key+".pb")
	if err := os.Remove(filePath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file: %w", err)
		}
	}

	// Remove from cache
	delete(f.cache, key)

	return nil
}

func (f *CachedFileStorage[T]) GetResource(key string) (T, error) {
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
	filePath := filepath.Join(f.basePath, key+".pb")

	data, err := os.ReadFile(filePath)
	if err != nil {
		return resource, fmt.Errorf("failed to read file: %w", err)
	}

	// Create a new instance of T using the factory function
	resource = f.newFunc()

	if err := proto.Unmarshal(data, resource); err != nil {
		return resource, fmt.Errorf("failed to unmarshal resource: %w", err)
	}

	// Store in the cache for future access
	f.cache[key] = resource

	return resource, nil
}

// LoadAll loads all resources from disk into memory cache
func (f *CachedFileStorage[T]) LoadAll() ([]T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	entries, err := os.ReadDir(f.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	var resources []T
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".pb" {
			continue
		}

		key := entry.Name()[:len(entry.Name())-3] // Remove .pb extension
		filePath := filepath.Join(f.basePath, entry.Name())

		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
		}

		resource := f.newFunc()
		if err := proto.Unmarshal(data, resource); err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource from %s: %w", filePath, err)
		}

		f.cache[key] = resource
		resources = append(resources, resource)
	}

	return resources, nil
}
