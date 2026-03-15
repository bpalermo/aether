package cache

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeCNIPod builds a minimal CNIPod that satisfies proxy.GenerateListenersFromRegistryPod.
// A non-empty NetworkNamespace is the only field required for listener generation.
func makeCNIPod(name, namespace, networkNamespace string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             name,
		Namespace:        namespace,
		NetworkNamespace: networkNamespace,
		ContainerId:      "container-" + name,
	}
}

// initListeners initialises the listeners map on c when it is nil. This is
// necessary because LoadListenersFromStorage writes into the map without a nil
// guard (unlike LoadClustersFromRegistry which explicitly checks for nil).
// Tests that call LoadListenersFromStorage directly must call this first.
func initListeners(c *SnapshotCache) {
	if c.listeners == nil {
		c.listeners = make(map[string]listenerEntry)
	}
}

// seedListeners pre-populates the cache by calling LoadListenersFromStorage with
// the given pods.
func seedListeners(c *SnapshotCache, pods ...*cniv1.CNIPod) {
	initListeners(c)
	store := storage.NewMockStorage[*cniv1.CNIPod]()
	store.GetAllFunc = func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return pods, nil
	}
	_ = c.LoadListenersFromStorage(context.Background(), store, "example.org")
}

// ─── Listeners() ─────────────────────────────────────────────────────────────

// TestSnapshotCache_Listeners verifies the Listeners accessor under varying
// cache states.
func TestSnapshotCache_Listeners(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(c *SnapshotCache)
		wantLen   int
	}{
		{
			name:      "empty cache returns empty slice",
			setupFunc: func(_ *SnapshotCache) {},
			wantLen:   0,
		},
		{
			name: "single pod yields two resources (inbound + outbound)",
			setupFunc: func(c *SnapshotCache) {
				seedListeners(c, makeCNIPod("pod-a", "default", "/proc/100/ns/net"))
			},
			wantLen: 2,
		},
		{
			name: "two pods yield four resources",
			setupFunc: func(c *SnapshotCache) {
				seedListeners(c,
					makeCNIPod("pod-a", "default", "/proc/100/ns/net"),
					makeCNIPod("pod-b", "default", "/proc/200/ns/net"),
				)
			},
			wantLen: 4,
		},
		{
			name: "three pods yield six resources",
			setupFunc: func(c *SnapshotCache) {
				seedListeners(c,
					makeCNIPod("pod-a", "ns-a", "/proc/100/ns/net"),
					makeCNIPod("pod-b", "ns-b", "/proc/200/ns/net"),
					makeCNIPod("pod-c", "ns-c", "/proc/300/ns/net"),
				)
			},
			wantLen: 6,
		},
		{
			name: "direct map injection also returns two resources per entry",
			setupFunc: func(c *SnapshotCache) {
				c.listeners = map[string]listenerEntry{
					"/proc/1/ns/net": {inbound: nil, outbound: nil},
				}
			},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)
			got := c.Listeners()
			assert.Len(t, got, tt.wantLen)
		})
	}
}

// ─── RemovePod() ─────────────────────────────────────────────────────────────

// TestSnapshotCache_RemovePod_NonExisting verifies that removing a pod that is
// not in the cache is a no-op: it returns nil and does not alter the map.
func TestSnapshotCache_RemovePod_NonExisting(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(c *SnapshotCache)
		netns     string
	}{
		{
			name:      "empty cache, remove unknown netns",
			setupFunc: func(_ *SnapshotCache) {},
			netns:     "/proc/999/ns/net",
		},
		{
			name: "cache with other entries, remove non-present netns",
			setupFunc: func(c *SnapshotCache) {
				c.listeners = map[string]listenerEntry{
					"/proc/1/ns/net": {},
				}
			},
			netns: "/proc/999/ns/net",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)
			sizeBefore := len(c.listeners)

			err := c.RemovePod(context.Background(), tt.netns)

			require.NoError(t, err)
			assert.Len(t, c.listeners, sizeBefore, "map size should not change for unknown netns")
		})
	}
}

// TestSnapshotCache_RemovePod_ExistingPod verifies that removing a pod that
// exists deletes it from the map and generates a valid listener snapshot.
func TestSnapshotCache_RemovePod_ExistingPod(t *testing.T) {
	tests := []struct {
		name          string
		initialPods   []*cniv1.CNIPod
		removeNetns   string
		wantMapLen    int
		wantGone      string
		wantRemaining []string
	}{
		{
			name: "remove the only pod results in empty listener map",
			initialPods: []*cniv1.CNIPod{
				makeCNIPod("pod-a", "default", "/proc/100/ns/net"),
			},
			removeNetns: "/proc/100/ns/net",
			wantMapLen:  0,
			wantGone:    "/proc/100/ns/net",
		},
		{
			name: "remove one of two pods leaves the other intact",
			initialPods: []*cniv1.CNIPod{
				makeCNIPod("pod-a", "default", "/proc/100/ns/net"),
				makeCNIPod("pod-b", "default", "/proc/200/ns/net"),
			},
			removeNetns:   "/proc/100/ns/net",
			wantMapLen:    1,
			wantGone:      "/proc/100/ns/net",
			wantRemaining: []string{"/proc/200/ns/net"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			seedListeners(c, tt.initialPods...)

			err := c.RemovePod(context.Background(), tt.removeNetns)
			require.NoError(t, err)

			assert.Len(t, c.listeners, tt.wantMapLen)
			_, stillExists := c.listeners[tt.wantGone]
			assert.False(t, stillExists, "removed netns %q should be absent from map", tt.wantGone)

			for _, key := range tt.wantRemaining {
				_, ok := c.listeners[key]
				assert.True(t, ok, "netns %q should still be present in map", key)
			}
		})
	}
}

// ─── LoadListenersFromStorage() ──────────────────────────────────────────────

// TestLoadListenersFromStorage_StorageError verifies that an error from storage
// GetAll is immediately propagated before any listener generation occurs.
func TestLoadListenersFromStorage_StorageError(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	storageErr := errors.New("disk unavailable")
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return nil, storageErr
	})

	err := c.LoadListenersFromStorage(context.Background(), store, "example.org")

	require.Error(t, err)
	assert.ErrorIs(t, err, storageErr)
	assert.Empty(t, c.listeners, "listener map must remain empty when storage fails")
}

// TestLoadListenersFromStorage_EmptyStorage verifies that an empty storage
// produces no listeners and a successful empty snapshot.
func TestLoadListenersFromStorage_EmptyStorage(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	store := storage.NewMockStorage[*cniv1.CNIPod]()

	err := c.LoadListenersFromStorage(context.Background(), store, "example.org")

	require.NoError(t, err)
	assert.Empty(t, c.listeners)
	assert.Empty(t, c.Listeners())
}

// TestLoadListenersFromStorage_ValidPodsMapPopulation verifies that valid pods
// produce the expected number of listener entries in the in-memory cache.
//
// Note: snapshot generation after populating the map consistently produces a
// "snapshot inconsistency" error because the outbound listener's RDS reference
// to the "out_http" RouteConfiguration is not present in a listener-only snapshot.
// This mirrors the behaviour documented for LoadClustersFromRegistry. The test
// therefore ignores the snapshot error and focuses on the map state — the same
// approach used in TestLoadClustersFromRegistry_MapPopulation.
func TestLoadListenersFromStorage_ValidPodsMapPopulation(t *testing.T) {
	tests := []struct {
		name        string
		pods        []*cniv1.CNIPod
		wantEntries int
	}{
		{
			name: "single valid pod populates one entry",
			pods: []*cniv1.CNIPod{
				makeCNIPod("pod-a", "default", "/proc/100/ns/net"),
			},
			wantEntries: 1,
		},
		{
			name: "two valid pods populate two entries",
			pods: []*cniv1.CNIPod{
				makeCNIPod("pod-a", "default", "/proc/100/ns/net"),
				makeCNIPod("pod-b", "kube-system", "/proc/200/ns/net"),
			},
			wantEntries: 2,
		},
		{
			name: "three pods across different namespaces all cached",
			pods: []*cniv1.CNIPod{
				makeCNIPod("pod-a", "ns-a", "/proc/100/ns/net"),
				makeCNIPod("pod-b", "ns-b", "/proc/200/ns/net"),
				makeCNIPod("pod-c", "ns-c", "/proc/300/ns/net"),
			},
			wantEntries: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			initListeners(c)
			store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return tt.pods, nil
			})

			err := c.LoadListenersFromStorage(context.Background(), store, "example.org")
			require.NoError(t, err)

			assert.Len(t, c.listeners, tt.wantEntries)
			// Each entry contributes both an inbound and outbound resource.
			assert.Len(t, c.Listeners(), tt.wantEntries*2)
		})
	}
}

// TestLoadListenersFromStorage_ValidPodsSnapshotInconsistency verifies that
// TestLoadListenersFromStorage_ValidPodsSucceeds verifies that loading valid pods
// produces a successful snapshot.
func TestLoadListenersFromStorage_ValidPodsSucceeds(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{makeCNIPod("pod-a", "default", "/proc/100/ns/net")}, nil
	})

	err := c.LoadListenersFromStorage(context.Background(), store, "example.org")

	require.NoError(t, err)
	assert.Len(t, c.listeners, 1)
}

// TestLoadListenersFromStorage_ValidPods_NetnsKeyed verifies that the listener
// entries are stored under the pod's network namespace key, and that both
// inbound and outbound listeners are non-nil.
func TestLoadListenersFromStorage_ValidPods_NetnsKeyed(t *testing.T) {
	c := newTestCache("node-1")
	initListeners(c)
	pod := makeCNIPod("my-pod", "default", "/proc/42/ns/net")
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})

	require.NoError(t, c.LoadListenersFromStorage(context.Background(), store, "example.org"))

	entry, ok := c.listeners["/proc/42/ns/net"]
	require.True(t, ok, "listener entry must be keyed by network namespace")
	assert.NotNil(t, entry.inbound)
	assert.NotNil(t, entry.outbound)
}

// TestLoadListenersFromStorage_PodFailsGeneration verifies that when one pod
// fails listener generation (empty network namespace or nil pod), an error is
// returned but the remaining valid pods are still processed and cached.
func TestLoadListenersFromStorage_PodFailsGeneration(t *testing.T) {
	tests := []struct {
		name              string
		pods              []*cniv1.CNIPod
		wantEntries       int
		wantErrSubstrings []string
	}{
		{
			name: "single invalid pod (empty netns) returns error, no entries cached",
			pods: []*cniv1.CNIPod{
				// Empty NetworkNamespace causes GenerateListenersFromRegistryPod to fail.
				{Name: "bad-pod", Namespace: "default", NetworkNamespace: "", ContainerId: "ctr-bad"},
			},
			wantEntries:       0,
			wantErrSubstrings: []string{"network namespace"},
		},
		{
			name: "nil pod returns error",
			pods: []*cniv1.CNIPod{
				nil,
			},
			wantEntries:       0,
			wantErrSubstrings: []string{"pod is required"},
		},
		{
			name: "mix of valid and invalid pods: valid ones still cached",
			pods: []*cniv1.CNIPod{
				makeCNIPod("good-pod", "default", "/proc/100/ns/net"),
				// Invalid: empty NetworkNamespace.
				{Name: "bad-pod", Namespace: "default", NetworkNamespace: "", ContainerId: "ctr-bad"},
			},
			wantEntries:       1,
			wantErrSubstrings: []string{"network namespace"},
		},
		{
			name: "two invalid pods accumulate two errors",
			pods: []*cniv1.CNIPod{
				{Name: "bad-1", Namespace: "default", NetworkNamespace: "", ContainerId: "ctr-1"},
				{Name: "bad-2", Namespace: "default", NetworkNamespace: "", ContainerId: "ctr-2"},
			},
			wantEntries:       0,
			wantErrSubstrings: []string{"network namespace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			initListeners(c)
			store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return tt.pods, nil
			})

			err := c.LoadListenersFromStorage(context.Background(), store, "example.org")

			// When any pod fails generation the function returns the
			// accumulated errors immediately (before snapshot generation).
			require.Error(t, err)
			assert.Len(t, c.listeners, tt.wantEntries)
			for _, sub := range tt.wantErrSubstrings {
				assert.Contains(t, err.Error(), sub)
			}
		})
	}
}

// ─── Thread safety ────────────────────────────────────────────────────────────

// TestSnapshotCache_Listeners_ThreadSafety exercises concurrent reads on
// Listeners to verify there are no data races on the listener map.
func TestSnapshotCache_Listeners_ThreadSafety(t *testing.T) {
	c := newTestCache("node-1")
	c.listeners = map[string]listenerEntry{
		"/proc/100/ns/net": {inbound: nil, outbound: nil},
		"/proc/200/ns/net": {inbound: nil, outbound: nil},
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			result := c.Listeners()
			assert.Len(t, result, 4)
		}()
	}
	wg.Wait()
}

// TestSnapshotCache_Listeners_ConcurrentReadWrite verifies no data races when
// reads and writes to the listener map happen simultaneously.
func TestSnapshotCache_Listeners_ConcurrentReadWrite(t *testing.T) {
	c := newTestCache("node-1")
	seedListeners(c, makeCNIPod("pod-a", "default", "/proc/100/ns/net"))

	const goroutines = 10
	var wg sync.WaitGroup

	// Concurrent readers.
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = c.Listeners()
		}()
	}

	// Concurrent RemovePod calls with a non-existing key: they acquire the
	// lock but return immediately without mutating state.
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = c.RemovePod(context.Background(), "/proc/999/ns/net")
		}()
	}

	wg.Wait()
}

// TestSnapshotCache_Listeners_ResultIsIndependentSlice verifies that mutating
// the returned slice does not affect the internal cache state.
func TestSnapshotCache_Listeners_ResultIsIndependentSlice(t *testing.T) {
	c := newTestCache("node-1")
	seedListeners(c, makeCNIPod("pod-a", "default", "/proc/100/ns/net"))

	first := c.Listeners()
	require.Len(t, first, 2)

	// Zero out the returned slice.
	for i := range first {
		first[i] = types.Resource(nil)
	}

	// A second call must still return the original two resources.
	second := c.Listeners()
	assert.Len(t, second, 2)
}
