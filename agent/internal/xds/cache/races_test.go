package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentMutationsFinalSnapshotComplete is the R1 regression test
// (docs/proposals/002): generateSnapshot serializes version generation, map reads
// and SetSnapshot, so whichever caller publishes last has read the final map
// state — the resulting snapshot must contain every concurrently added pod's
// listeners. Without the serialization, a snapshot built from a stale read can
// land last and silently drop newer mutations. Run under -race this also guards
// the map-access discipline.
func TestConcurrentMutationsFinalSnapshotComplete(t *testing.T) {
	const pods = 50
	c := newTestCache("node-1")

	var wg sync.WaitGroup
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pod := makeCNIPod(fmt.Sprintf("pod-%d", i), "default", fmt.Sprintf("/proc/%d/ns/net", 1000+i))
			pod.Ips = []string{fmt.Sprintf("10.0.0.%d", i+1)}
			require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))
		}(i)
	}
	wg.Wait()

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	listeners := snap.GetResources(resourcev3.ListenerType)
	// Two listeners (inbound + outbound) per pod; a lost update would leave fewer.
	assert.Len(t, listeners, 2*pods, "final snapshot must reflect every concurrent AddPod")
}

// TestLoadListenersFromStorageMergesLocalWorkloads is the R3 regression test
// (docs/proposals/002): the startup storage load must merge into localWorkloads,
// not replace it — a pod whose AddPod landed between the storage read and the
// write would otherwise lose its netns→SPIFFE-ID mapping and present the node
// certificate instead of its own SVID on outbound mTLS.
func TestLoadListenersFromStorageMergesLocalWorkloads(t *testing.T) {
	c := newTestCache("node-1")

	// A pod added via CNI ADD that is not (yet) visible in the storage snapshot.
	racer := makeCNIPod("racer", "default", "/proc/999/ns/net")
	racer.Ips = []string{"10.0.0.99"}
	require.NoError(t, c.AddPod(context.Background(), racer, "example.org"))

	// The storage load sees only a different pod.
	stored := makeCNIPod("stored", "default", "/proc/100/ns/net")
	stored.Ips = []string{"10.0.0.1"}
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{stored}, nil
	})
	require.NoError(t, c.LoadListenersFromStorage(context.Background(), store, "example.org"))

	c.localMu.RLock()
	defer c.localMu.RUnlock()
	assert.Contains(t, c.localWorkloads, "/proc/999/ns/net", "concurrently added pod's workload mapping must survive the storage load")
	assert.Contains(t, c.localWorkloads, "/proc/100/ns/net", "stored pod's workload mapping must be present")
}
