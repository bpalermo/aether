package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/spire"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestSweepPruneUnsubscribesSVID: a pod the sweep prunes never gets the CNI DEL
// that would have ended its SVID subscription, so the prune has to end it. Left
// running, the subscription asks SPIRE for a pod that no longer exists and
// retries NotFound for the agent's lifetime (talos-main, 2026-09-18: a svc-3 pod
// deleted while the agent was down kept main-worker-02 retrying at ~2/min). A
// live pod's subscription is untouched.
func TestSweepPruneUnsubscribesSVID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orphan := validCNIPod("pod-orphan", "default", "container-orphan")
	orphan.Ips = []string{"10.0.0.9"}
	orphan.NetworkNamespace = "/proc/9/ns/net"
	live := validCNIPod("pod-live", "default", "container-live")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-orphan"), orphan))
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	k8s := fake.NewClientBuilder().WithObjects(validK8sPod("pod-live", "default")).Build()
	sc := cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler))
	s := newTestCNIServer(k8s, store, reg, sc, "")

	// A started bridge with no SPIRE behind it: the identity source never yields
	// an SVID and nothing listens on the socket, so both subscriptions sit in
	// their retry loop — the state the leaked one was stuck in.
	bridge := spire.NewBridge(filepath.Join(t.TempDir(), "broker.sock"), sc, spiretest.NewPendingIdentity(), slog.New(slog.DiscardHandler))
	s.spireBridge = bridge
	done := make(chan struct{})
	go func() { defer close(done); _ = bridge.Start(ctx) }()
	select {
	case <-bridge.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not start")
	}
	for _, p := range []*cniv1.CNIPod{orphan, live} {
		ref := spire.PodRef{Namespace: p.GetNamespace(), Name: p.GetName(), UID: "uid-" + p.GetName()}
		require.NoError(t, bridge.SubscribePod(p.GetNetworkNamespace(), "spiffe://example.org/ns/default/sa/default", ref))
	}
	require.True(t, bridge.Subscribed(orphan.GetNetworkNamespace()))

	s.sweepGhostEndpoints(ctx)

	_, err := store.GetResource(ctx, types.ContainerID("container-orphan"))
	require.Error(t, err, "orphan pruned from storage")
	require.False(t, bridge.Subscribed(orphan.GetNetworkNamespace()), "the pruned pod's SVID subscription must end with it")
	require.True(t, bridge.Subscribed(live.GetNetworkNamespace()), "a live pod keeps its subscription")

	cancel()
	<-done
}
