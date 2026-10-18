package cache

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/require"
)

func listenerChainNames(l *listenerv3.Listener) []string {
	out := make([]string, 0, len(l.GetFilterChains()))
	for _, fc := range l.GetFilterChains() {
		out = append(out, fc.GetName())
	}
	return out
}

// TestAddPod_RacingSetServiceRanges: AddPod builds the pod's capture listener
// before it takes listenerMu to insert the entry, and SetServiceRanges
// rebuilds only the entries already inserted. A range set in that window must
// still reach the new pod's listener.
//
// The window is forced: AddPod is parked on clusterMu inside
// generateUDPCaptureListener (routableUDPRoutes), after its capture listener
// was built and before the insert, and SetServiceRanges runs to completion
// meanwhile.
func TestAddPod_RacingSetServiceRanges(t *testing.T) {
	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	c.depMu.Lock()
	c.udpServiceRoutes = map[string][]proxy.L4Backend{"demo/u": {{Cluster: "udp:u.demo.aether.internal", Weight: 1}}}
	c.depMu.Unlock()
	pod := &cniv1.CNIPod{Name: "p", Namespace: "demo", ServiceAccount: "sa", NetworkNamespace: "/var/run/netns/cni-forced"}

	c.clusterMu.Lock()
	done := make(chan struct{})
	go func() { defer close(done); _ = c.AddPod(ctx, pod, "aether.internal") }()
	// Let AddPod build its capture listener and park on clusterMu.
	time.Sleep(200 * time.Millisecond)
	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")})
	}()
	select {
	case <-setDone:
	case <-time.After(5 * time.Second):
		c.clusterMu.Unlock()
		t.Fatal("SetServiceRanges blocked on clusterMu: the interleaving could not be forced")
	}
	c.clusterMu.Unlock()
	<-done

	c.listenerMu.RLock()
	l := c.listeners[pod.GetNetworkNamespace()].capture.(*listenerv3.Listener)
	c.listenerMu.RUnlock()
	require.Contains(t, listenerChainNames(l), proxy.CapturePassthroughServiceChainName,
		"the cache holds a Service range, but the pod added concurrently has a capture listener without the Service-range branch")
}

// TestSetServiceRanges_ConcurrentCallsConverge: concurrent SetServiceRanges
// calls, with a concurrent mesh-Service projection, leave the pod's listener
// built from the range the cache ends with.
func TestSetServiceRanges_ConcurrentCallsConverge(t *testing.T) {
	ctx := context.Background()
	a := []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}
	b := []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12"), netip.MustParsePrefix("100.64.5.5/32")}
	for i := range 100 {
		c := a043Cache(t, a043Fixture())
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, r := range [][]netip.Prefix{a, b, nil, a, b} {
			wg.Go(func() { <-start; c.SetServiceRanges(r) })
		}
		wg.Go(func() { <-start; a043Project(c, a043Fixture()[:3]) })
		close(start)
		wg.Wait()
		require.NoError(t, c.generateSnapshot(ctx))
		final := c.ServiceRanges()
		has := slices.Contains(listenerChainNames(podCapture(t, c)), proxy.CapturePassthroughServiceChainName)
		require.Equal(t, len(final) > 0, has, "iteration %d: final range %v, listener has the branch=%v", i, final, has)
	}
}
