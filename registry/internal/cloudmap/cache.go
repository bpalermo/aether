package cloudmap

import (
	"sync"
	"time"
)

// cacheEntry holds a cached value with an expiration time.
type cacheEntry[T any] struct {
	value     T
	expiresAt time.Time
}

// ttlCache is a simple thread-safe cache with per-entry TTL expiration.
type ttlCache[T any] struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry[T]
	ttl     time.Duration
}

// newTTLCache creates a new TTL cache with the given default TTL.
func newTTLCache[T any](ttl time.Duration) *ttlCache[T] {
	return &ttlCache[T]{
		entries: make(map[string]cacheEntry[T]),
		ttl:     ttl,
	}
}

// get retrieves a value from the cache. Returns the value and true if found and not expired.
func (c *ttlCache[T]) get(key string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		var zero T
		return zero, false
	}
	return entry.value, true
}

// set stores a value in the cache with the default TTL.
func (c *ttlCache[T]) set(key string, value T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry[T]{
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// delete removes an entry from the cache.
func (c *ttlCache[T]) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
}
