// Package storage provides interfaces and implementations for persisting and
// retrieving pod data, as local file-based storage backed by an in-memory cache.
//
// Ownership contract: the cache owns its values. Every resource that crosses the
// interface — in or out — is deep-copied, so a value returned by GetResource or
// GetAll is the caller's private copy and a value handed to AddResource is not
// aliased by the cache afterwards. Callers are therefore free to mutate what they
// get back; to make a change stick they write it back with AddResource. See the
// CachedLocalStorage doc for why (a shared pointer here was a real data race
// between the CNI server's termination watch and its liveness loop).
package storage

import (
	"context"

	"aethermesh.dev/agent/types"
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
	// AddResource stores a new resource with the given key. The implementation
	// keeps a deep copy, so the caller may keep mutating its own value without
	// affecting what is stored.
	AddResource(ctx context.Context, key types.ContainerID, resource T) error
	// RemoveResource deletes a resource by key.
	RemoveResource(ctx context.Context, key types.ContainerID) error
	// GetResource retrieves a single resource by key. The returned message is a
	// deep copy the caller owns: mutating it does not affect the stored resource
	// (write it back with AddResource to persist a change).
	GetResource(ctx context.Context, key types.ContainerID) (T, error)
	// GetAll returns all stored resources from the in-memory cache. Each element
	// is a deep copy the caller owns — see GetResource.
	GetAll(_ context.Context) ([]T, error)
	// loadAll loads all resources from persistent storage. This is an internal method.
	loadAll(ctx context.Context) ([]T, error)
}
