package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/require"
)

// TestWaitersReadThePublishedVersionWhileTheCachePublishes is for
// the race detector: waiters read the published version (GetSnapshot and the
// snapshot's version map) while pods are added and removed (#1559 review).
func TestWaitersReadThePublishedVersionWhileTheCachePublishes(t *testing.T) {
	ctx := context.Background()
	c := newTestCache("node-1")
	tracker := ack.NewTracker(c.log)
	tracker.SetPublishedVersion(ack.SnapshotVersions(c, c.nodeName))
	tracker.SetAckObserver(c.ResponseAccepted)
	tracker.SetDeliveryObserver(c.ResponseDelivery)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		pod := makeDepPod(fmt.Sprintf("p-%d", w), "svc-a", fmt.Sprintf("/proc/%d/ns/net", w+1), "")
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				require.NoError(t, c.AddPod(ctx, pod, "example.org"))
				require.NoError(t, c.RemovePod(ctx, pod.GetNetworkNamespace()))
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				waitCtx, cancel := context.WithTimeout(ctx, 200*time.Microsecond)
				_ = tracker.WaitListenerPresent(waitCtx, proxy.OutboundListenerName(pod))
				cancel()
			}
		}()
	}
	wg.Wait()
}
