package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/proxy"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/stretchr/testify/require"
)

// TestAckTrackerReadsThePublishedListenerVersionFromTheCache wires the ACK
// tracker to this cache as the node agent does
// (ack.Tracker.SetPublishedVersion(ack.SnapshotVersions(cache, node))) and
// holds the two to each other with the cache's own delta responses (#1624):
//
//   - the version the tracker reads as published for a pod's listener is the
//     one the cache's response carries, so the wait a CNI ADD makes is
//     answered by the ACK of that response and by nothing else;
//   - an agent that restarts publishes the same version for the same pod, so
//     a proxy that states it is not sent the listener again and the wait is
//     answered from what the proxy states (#1511);
//   - another pod under the same name publishes another version, and the wait
//     is for the acknowledgement of that one.
//
// If the cache stopped filling the snapshot's version map, or the agent
// stopped being deterministic about a listener's bytes across a restart, the
// tracker would answer no wait at all, or none after a restart: this fails.
func TestAckTrackerReadsThePublishedListenerVersionFromTheCache(t *testing.T) {
	const waitFor = 10 * time.Second
	ctx := context.Background()
	pod := makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "")
	listener := proxy.OutboundListenerName(pod)

	// agent is one agent process: a cache with the pod, and a tracker on it.
	agent := func(t *testing.T) (*SnapshotCache, *ack.Tracker) {
		t.Helper()
		c := newTestCache("node-1")
		require.NoError(t, c.AddPod(ctx, pod, "example.org"))
		tracker := ack.NewTracker(c.log)
		tracker.SetPublishedVersion(ack.SnapshotVersions(c, c.nodeName))
		return c, tracker
	}

	// exchange plays one request of a proxy that states `held`, and returns
	// the cache's answer after the tracker has seen both.
	exchange := func(t *testing.T, c *SnapshotCache, tracker *ack.Tracker, stream int64, nonce string, held map[string]string) *discoveryv3.DeltaDiscoveryResponse {
		t.Helper()
		req := &discoveryv3.DeltaDiscoveryRequest{
			Node:                    &corev3.Node{Id: c.nodeName},
			TypeUrl:                 resourcev3.ListenerType,
			InitialResourceVersions: held,
		}
		require.NoError(t, tracker.Callbacks().OnStreamDeltaRequest(stream, req))
		responses := make(chan cachev3.DeltaResponse, 1)
		cancel, err := c.CreateDeltaWatch(req, streamv3.NewDeltaSubscription(nil, nil, held, true), responses)
		require.NoError(t, err)
		if cancel != nil {
			defer cancel()
		}
		select {
		case raw := <-responses:
			resp, err := raw.GetDeltaDiscoveryResponse()
			require.NoError(t, err)
			resp.Nonce = nonce
			tracker.Callbacks().OnStreamDeltaResponse(stream, req, resp)
			return resp
		case <-time.After(waitFor):
			t.Fatal("no Listener response")
			return nil
		}
	}
	acknowledge := func(t *testing.T, tracker *ack.Tracker, stream int64, nonce string) {
		t.Helper()
		require.NoError(t, tracker.Callbacks().OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
			TypeUrl: resourcev3.ListenerType, ResponseNonce: nonce,
		}))
	}
	present := func(tracker *ack.Tracker, within time.Duration) error {
		waitCtx, cancel := context.WithTimeout(ctx, within)
		defer cancel()
		return tracker.WaitListenerPresent(waitCtx, listener)
	}
	versionsOf := func(resp *discoveryv3.DeltaDiscoveryResponse) map[string]string {
		versions := map[string]string{}
		for _, r := range resp.GetResources() {
			versions[r.GetName()] = r.GetVersion()
		}
		return versions
	}

	// A proxy with nothing: it is sent the listener, and the ACK answers.
	c, tracker := agent(t)
	sent := exchange(t, c, tracker, 1, "1", nil)
	held := versionsOf(sent)
	require.Contains(t, held, listener)
	require.Error(t, present(tracker, 50*time.Millisecond), "sent is not acknowledged")
	acknowledge(t, tracker, 1, "1")
	require.NoError(t, present(tracker, waitFor), "the version the response carried must be the one the tracker reads as published")

	// The agent restarts. The proxy states what it holds.
	c, tracker = agent(t)
	require.Error(t, present(tracker, 50*time.Millisecond), "no proxy has said anything to the new agent")
	answer := exchange(t, c, tracker, 1, "1", held)
	require.Empty(t, answer.GetResources(), "a restarted agent publishes the same version of the same listener")
	require.Empty(t, answer.GetRemovedResources())
	require.NoError(t, present(tracker, waitFor), "stated at the published version (#1511)")

	// Another pod takes the name: other content, so another version.
	replacement := makeDepPod("a-1", "svc-b", "/proc/1/ns/net", "")
	require.NoError(t, c.AddPod(ctx, replacement, "example.org"))
	require.Error(t, present(tracker, 50*time.Millisecond), "the proxy holds the listener of the pod that is gone")
}

// TestWaitersReadThePublishedVersionWhileTheCachePublishes is for the race
// detector: waiters read the published version (GetSnapshot and the
// snapshot's version map) while pods are added and removed.
func TestWaitersReadThePublishedVersionWhileTheCachePublishes(t *testing.T) {
	ctx := context.Background()
	c := newTestCache("node-1")
	tracker := ack.NewTracker(c.log)
	tracker.SetPublishedVersion(ack.SnapshotVersions(c, c.nodeName))
	tracker.SetAckObserver(c.ResponseAccepted)
	tracker.SetDeliveryObserver(c.ResponseDelivery)

	var wg sync.WaitGroup
	for w := range 4 {
		pod := makeDepPod(fmt.Sprintf("p-%d", w), "svc-a", fmt.Sprintf("/proc/%d/ns/net", w+1), "")
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 30 {
				require.NoError(t, c.AddPod(ctx, pod, "example.org"))
				require.NoError(t, c.RemovePod(ctx, pod.GetNetworkNamespace()))
			}
		}()
		go func() {
			defer wg.Done()
			for range 300 {
				waitCtx, cancel := context.WithTimeout(ctx, 200*time.Microsecond)
				_ = tracker.WaitListenerPresent(waitCtx, proxy.OutboundListenerName(pod))
				cancel()
			}
		}()
	}
	wg.Wait()
}
