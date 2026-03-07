// Package storage provides interfaces and implementations for persisting and retrieving
// pod data. It supports both local file-based storage with in-memory caching and
// file watching via fsnotify.
package storage

import (
	"context"

	"github.com/bpalermo/aether/agent/pkg/types"
	"google.golang.org/protobuf/proto"
)

// Storage defines an interface for storing and retrieving protobuf messages.
// Implementations manage the lifecycle of resources and maintain consistency
// between persistent storage and in-memory caches.
type Storage[T proto.Message] interface {
	// Initialize prepares the storage for use, including creating directories
	// and loading initial data if necessary.
	Initialize(ctx context.Context) error
	// WaitUntilReady waits until the storage is ready to serve requests.
	// This may involve waiting for file watching mechanisms to be initialized.
	WaitUntilReady(ctx context.Context) error
	// AddResource stores a new resource with the given key.
	AddResource(ctx context.Context, key types.ContainerID, resource T) error
	// RemoveResource deletes a resource by key.
	RemoveResource(ctx context.Context, key types.ContainerID) error
	// GetResource retrieves a single resource by key.
	GetResource(ctx context.Context, key types.ContainerID) (T, error)
	// GetAll returns all stored resources from the in-memory cache.
	GetAll(_ context.Context) ([]T, error)
	// loadAll loads all resources from persistent storage. This is an internal method.
	loadAll(ctx context.Context) ([]T, error)
}
