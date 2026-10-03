package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	cptypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// overlapPod is a mesh pod whose netns path really exists, so the cache keeps
// its listeners (a missing netns is filtered as stale).
func overlapPod(t *testing.T, dir, name, ip string) *cniv1.CNIPod {
	t.Helper()
	netns := filepath.Join(dir, "netns-"+name)
	require.NoError(t, os.WriteFile(netns, nil, 0o600))
	pod := validCNIPod(name, "default", "container-"+name)
	pod.NetworkNamespace = netns
	pod.Ips = []string{ip}
	pod.Uid = "uid-" + name
	return pod
}

func listenerNames(rs []cptypes.Resource) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		if named, ok := r.(interface{ GetName() string }); ok {
			out[named.GetName()] = true
		}
	}
	return out
}

func newStorage(t *testing.T, dir string) *storage.CachedLocalStorage[*cniv1.CNIPod] {
	t.Helper()
	s := storage.NewCachedLocalStorage[*cniv1.CNIPod](dir, func() *cniv1.CNIPod { return &cniv1.CNIPod{} })
	require.NoError(t, s.Initialize(context.Background()))
	return s
}

// TestReconcileStorage_AppliesTheOverlap is open question 1 and the "CNI ADD
// during the overlap" row of proposal 041, end to end over a real storage
// directory shared by two agents.
//
// The standby loads storage (pods a and b) and builds their listeners. Then,
// while the OLD agent still owns the node, it serves an ADD (c), a DEL (b) and
// rewrites a's record (its termination watch marking it Terminating). At
// takeover the standby must end up with exactly what is on the node: a
// rebuilt, c added, b gone — the DEL is the case LoadListenersFromStorage
// alone cannot express, since it merges and never removes.
func TestReconcileStorage_AppliesTheOverlap(t *testing.T) {
	ctx := context.Background()
	podDir := t.TempDir()
	storeDir := t.TempDir()

	a := overlapPod(t, podDir, "a", "10.0.0.1")
	b := overlapPod(t, podDir, "b", "10.0.0.2")
	c := overlapPod(t, podDir, "c", "10.0.0.3")

	old := newStorage(t, storeDir)
	require.NoError(t, old.AddResource(ctx, types.ContainerID(a.GetContainerId()), a))
	require.NoError(t, old.AddResource(ctx, types.ContainerID(b.GetContainerId()), b))

	// The standby starts: loads storage and builds the first snapshot from it.
	standbyStore := newStorage(t, storeDir)
	sc := cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler))
	require.NoError(t, sc.LoadListenersFromStorage(ctx, standbyStore, "example.org"))
	before := listenerNames(sc.Listeners())
	require.True(t, before[proxy.OutboundListenerName(a)])
	require.True(t, before[proxy.OutboundListenerName(b)])

	// The overlap: the old agent serves an ADD, a DEL and a record rewrite.
	require.NoError(t, old.AddResource(ctx, types.ContainerID(c.GetContainerId()), c))
	require.NoError(t, old.RemoveResource(ctx, types.ContainerID(b.GetContainerId())))
	terminating := cloneTerminating(a)
	require.NoError(t, old.AddResource(ctx, types.ContainerID(a.GetContainerId()), terminating))

	srv := newTestCNIServer(nil, standbyStore, &testRegistry{}, sc, "")
	require.NoError(t, srv.ReconcileStorage(ctx))

	after := listenerNames(sc.Listeners())
	assert.True(t, after[proxy.OutboundListenerName(a)], "a pod present throughout keeps its listeners")
	assert.True(t, after[proxy.OutboundListenerName(c)], "a pod ADDed during the overlap is served after takeover")
	assert.False(t, after[proxy.OutboundListenerName(b)], "a pod DELed during the overlap must not survive the takeover")
	assert.False(t, after[proxy.InboundListenerName(b)])

	stored, err := standbyStore.GetResource(ctx, types.ContainerID(a.GetContainerId()))
	require.NoError(t, err)
	assert.True(t, stored.GetTerminating(), "the standby's storage view follows the old agent's rewrite")
	_, err = standbyStore.GetResource(ctx, types.ContainerID(b.GetContainerId()))
	assert.Error(t, err)

	// Nothing more happened: a second pass is a no-op.
	require.NoError(t, srv.ReconcileStorage(ctx))
	assert.Equal(t, after, listenerNames(sc.Listeners()))
}

// TestReconcileStorage_ADDThenDELInsideTheOverlapLeavesNothing: a pod that
// came and went while this agent was a standby was never in its view and is
// not on the node; nothing may appear for it.
func TestReconcileStorage_ADDThenDELInsideTheOverlapLeavesNothing(t *testing.T) {
	ctx := context.Background()
	podDir, storeDir := t.TempDir(), t.TempDir()
	d := overlapPod(t, podDir, "d", "10.0.0.4")

	standbyStore := newStorage(t, storeDir)
	sc := cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler))
	require.NoError(t, sc.LoadListenersFromStorage(ctx, standbyStore, "example.org"))

	old := newStorage(t, storeDir)
	require.NoError(t, old.AddResource(ctx, types.ContainerID(d.GetContainerId()), d))
	require.NoError(t, old.RemoveResource(ctx, types.ContainerID(d.GetContainerId())))

	srv := newTestCNIServer(nil, standbyStore, &testRegistry{}, sc, "")
	require.NoError(t, srv.ReconcileStorage(ctx))
	assert.False(t, listenerNames(sc.Listeners())[proxy.OutboundListenerName(d)])
}

// TestReconcileStorage_StorageWithoutReloadIsANoOp: a store that cannot report
// another writer's changes (the mock) leaves everything as it was.
func TestReconcileStorage_StorageWithoutReloadIsANoOp(t *testing.T) {
	sc := cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler))
	srv := newTestCNIServer(nil, storage.NewMockStorage[*cniv1.CNIPod](), &testRegistry{}, sc, "")
	require.NoError(t, srv.ReconcileStorage(context.Background()))
}

func cloneTerminating(p *cniv1.CNIPod) *cniv1.CNIPod {
	out := proto.Clone(p).(*cniv1.CNIPod)
	out.Terminating = true
	return out
}
