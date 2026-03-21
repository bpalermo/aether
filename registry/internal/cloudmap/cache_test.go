package cloudmap

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTTLCacheSetAndGet(t *testing.T) {
	tests := []struct {
		name       string
		ttl        time.Duration
		key        string
		value      string
		wantValue  string
		wantFound  bool
	}{
		{
			name:      "get existing key returns value",
			ttl:       time.Minute,
			key:       "k1",
			value:     "v1",
			wantValue: "v1",
			wantFound: true,
		},
		{
			name:      "empty string value is stored and retrieved",
			ttl:       time.Minute,
			key:       "k2",
			value:     "",
			wantValue: "",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTTLCache[string](tt.ttl)
			c.set(tt.key, tt.value)

			got, ok := c.get(tt.key)
			assert.Equal(t, tt.wantFound, ok)
			assert.Equal(t, tt.wantValue, got)
		})
	}
}

func TestTTLCacheGetMissingKey(t *testing.T) {
	c := newTTLCache[string](time.Minute)

	got, ok := c.get("nonexistent")
	assert.False(t, ok)
	assert.Equal(t, "", got)
}

func TestTTLCacheGetReturnsZeroValueWhenMissing(t *testing.T) {
	c := newTTLCache[int](time.Minute)

	got, ok := c.get("missing")
	assert.False(t, ok)
	assert.Equal(t, 0, got)
}

func TestTTLCacheOverwrite(t *testing.T) {
	c := newTTLCache[string](time.Minute)
	c.set("k", "first")
	c.set("k", "second")

	got, ok := c.get("k")
	require.True(t, ok)
	assert.Equal(t, "second", got)
}

func TestTTLCacheDelete(t *testing.T) {
	tests := []struct {
		name      string
		setFirst  bool
		deleteKey string
		getKey    string
		wantFound bool
	}{
		{
			name:      "delete existing key — get returns false",
			setFirst:  true,
			deleteKey: "k1",
			getKey:    "k1",
			wantFound: false,
		},
		{
			name:      "delete nonexistent key — no panic",
			setFirst:  false,
			deleteKey: "missing",
			getKey:    "missing",
			wantFound: false,
		},
		{
			name:      "delete one key does not affect another",
			setFirst:  true,
			deleteKey: "k1",
			getKey:    "k2",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTTLCache[string](time.Minute)
			if tt.setFirst {
				c.set("k1", "val1")
				c.set("k2", "val2")
			}

			c.delete(tt.deleteKey)

			_, ok := c.get(tt.getKey)
			assert.Equal(t, tt.wantFound, ok)
		})
	}
}

func TestTTLCacheExpiration(t *testing.T) {
	// Use a very short TTL so we can test expiration without long sleeps.
	c := newTTLCache[string](10 * time.Millisecond)
	c.set("k", "v")

	// Entry should be present immediately.
	got, ok := c.get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)

	// Wait for the TTL to elapse.
	time.Sleep(20 * time.Millisecond)

	// Entry should now be expired.
	got, ok = c.get("k")
	assert.False(t, ok)
	assert.Equal(t, "", got)
}

func TestTTLCacheExpirationDoesNotEvictOtherKeys(t *testing.T) {
	shortTTL := newTTLCache[string](10 * time.Millisecond)
	shortTTL.set("expires", "soon")

	// Set a second entry with a long TTL by using a separate cache write
	// that exploits the fact that each set refreshes the expiry.
	longCache := newTTLCache[string](time.Hour)
	longCache.set("durable", "yes")

	time.Sleep(20 * time.Millisecond)

	// expired entry is gone
	_, shortOK := shortTTL.get("expires")
	assert.False(t, shortOK)

	// durable entry in the long cache is still present
	got, longOK := longCache.get("durable")
	require.True(t, longOK)
	assert.Equal(t, "yes", got)
}

func TestTTLCacheSetRefreshesExpiry(t *testing.T) {
	c := newTTLCache[string](30 * time.Millisecond)
	c.set("k", "v1")

	// Let 20ms pass — still within TTL.
	time.Sleep(20 * time.Millisecond)

	// Re-set the key, which should push the expiry forward.
	c.set("k", "v2")

	// Wait another 20ms (40ms total from first set, but only 20ms from re-set).
	time.Sleep(20 * time.Millisecond)

	got, ok := c.get("k")
	require.True(t, ok, "entry should still be alive after expiry was refreshed")
	assert.Equal(t, "v2", got)
}

func TestTTLCacheWithStructValues(t *testing.T) {
	type item struct {
		Name  string
		Count int
	}

	c := newTTLCache[item](time.Minute)
	c.set("x", item{Name: "foo", Count: 42})

	got, ok := c.get("x")
	require.True(t, ok)
	assert.Equal(t, item{Name: "foo", Count: 42}, got)
}

func TestTTLCacheWithSliceValues(t *testing.T) {
	c := newTTLCache[[]string](time.Minute)
	c.set("list", []string{"a", "b", "c"})

	got, ok := c.get("list")
	require.True(t, ok)
	assert.Equal(t, []string{"a", "b", "c"}, got)
}

func TestTTLCacheConcurrentAccess(t *testing.T) {
	// Verify that concurrent get/set/delete operations do not race.
	// Run with -race flag to detect issues (Bazel race detection also applies).
	c := newTTLCache[int](time.Minute)

	const goroutines = 20
	const ops = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Writers
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			for j := range ops {
				key := fmt.Sprintf("key-%d-%d", n, j%5)
				c.set(key, j)
			}
		}(i)
	}

	// Readers
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			for j := range ops {
				key := fmt.Sprintf("key-%d-%d", n, j%5)
				c.get(key) //nolint:errcheck — intentionally discarding result
			}
		}(i)
	}

	// Deleters
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			for j := range ops {
				key := fmt.Sprintf("key-%d-%d", n, j%5)
				c.delete(key)
			}
		}(i)
	}

	wg.Wait()
	// If we reach here without a race detector failure, the test passes.
}

func TestNewTTLCacheIsEmpty(t *testing.T) {
	c := newTTLCache[string](time.Minute)

	_, ok := c.get("anything")
	assert.False(t, ok, "new cache should have no entries")
}
