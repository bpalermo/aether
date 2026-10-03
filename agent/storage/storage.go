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

// Reloader is a Storage whose in-memory view can be re-synchronised with disk,
// reporting what another writer changed since this process last looked.
//
// It exists for the node agent's surge handoff (proposal 041): a standby agent
// loads storage at its start, but until it takes the node over the OLD agent is
// the one serving CNI ADD/DEL and writing these files. On takeover the standby
// reloads and applies only the difference — a full listener rebuild costs
// 0.25-0.98 s on talos, inside the window the node's proxy has no ADS stream.
type Reloader[T proto.Message] interface {
	// Reload re-reads every resource from disk, replaces the in-memory view with
	// it, and returns how it differs from the view it replaced. Every value in
	// the returned Delta is a deep copy the caller owns.
	Reload(ctx context.Context) (Delta[T], error)
}

// Delta is the difference between two views of a storage directory, by key.
type Delta[T proto.Message] struct {
	// Added holds resources present on disk that the previous view did not have.
	Added []T
	// Updated holds resources present in both views whose content differs; the
	// value is the one now on disk.
	Updated []T
	// Removed holds resources the previous view had that are no longer on disk;
	// the value is the one the previous view held (what the caller built from).
	Removed []T
}

// Empty reports whether the two views were identical.
func (d Delta[T]) Empty() bool {
	return len(d.Added) == 0 && len(d.Updated) == 0 && len(d.Removed) == 0
}
