package cache

import (
	"sync"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/log"
)

// FallbackSnapshotCache wraps a SnapshotCache to provide wildcard fallback
type FallbackSnapshotCache struct {
	cache.SnapshotCache
	mu       sync.RWMutex
	wildcard string
}

// NewFallbackSnapshotCache creates a cache with wildcard fallback support
func NewFallbackSnapshotCache(ads bool, logger log.Logger, wildcardID string) *FallbackSnapshotCache {
	return &FallbackSnapshotCache{
		SnapshotCache: cache.NewSnapshotCache(ads, cache.IDHash{}, logger),
		wildcard:      wildcardID,
	}
}

// GetSnapshot returns a node-specific snapshot or falls back to wildcard
func (c *FallbackSnapshotCache) GetSnapshot(node string) (cache.ResourceSnapshot, error) {
	// Try the node-specific snapshot first
	if snap, err := c.SnapshotCache.GetSnapshot(node); err == nil {
		return snap, nil
	}

	// Fall back to wildcard
	return c.SnapshotCache.GetSnapshot(c.wildcard)
}
