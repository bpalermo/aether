package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/meshdns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNodeWriteGate_StandbyWritesNoObservedSet is the "Persisted state, two
// writers" row of proposal 041: a standby keeps observations in memory and
// writes nothing — not on the debounce, not on the shutdown flush — while the
// agent that owns the node is the file's writer. At takeover it first merges
// what that agent persisted last (its final flush included), then writes the
// union once.
func TestNodeWriteGate_StandbyWritesNoObservedSet(t *testing.T) {
	ctx := context.Background()
	path := storePath(t.TempDir())
	owned := make(chan struct{})

	c, _, _ := newBindingTestCache(t)
	c.SetMeshDomain("aether.internal")
	c.observedFlushDebounce = 5 * time.Millisecond
	c.SetNodeWriteGate(owned)
	c.EnableObservedUpstreamsStore(ctx, path)

	require.True(t, c.ObserveDependency(ctx, storeTestService))
	time.Sleep(50 * time.Millisecond) // ten debounce periods
	c.FlushObservedUpstreams()        // the shutdown flush of a standby that never took over
	assert.NoFileExists(t, path, "a standby must not write the observed set")

	// Meanwhile the old owner flushed its own observation, then exited.
	writeStore(t, path, storedEntry(storeTestService2, time.Now().Add(time.Hour)))

	require.NoError(t, c.ReloadNodeState(ctx))
	close(owned)

	require.Eventually(t, func() bool {
		got := storedServices(t, path)
		return len(got) == 2
	}, flushFireWait, eventuallyTick)
	assert.ElementsMatch(t, []string{storeTestService, storeTestService2}, storedServices(t, path),
		"the first write after takeover is the union of both agents' observations")

	// Owned: ordinary debounced writes again.
	require.True(t, c.ObserveDependency(ctx, "aether-test/svc-3"))
	waitObservedFlush(t, c)
	assert.Contains(t, storedServices(t, path), "aether-test/svc-3")
}

// TestNodeWriteGate_StandbyWritesNoMeshDNSSnapshot: the mesh-DNS record table
// is kept while a standby, and re-stamped at takeover, so the resolver daemon
// sees a live writer again right away.
func TestNodeWriteGate_StandbyWritesNoMeshDNSSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh-dns", "records.json")
	owned := make(chan struct{})
	c := newTestCache("node-1")
	c.SetNodeWriteGate(owned)
	c.SetMeshDNSSnapshotPath(path)

	c.SetMeshDNSRecords(map[string]string{"default/echo": "10.111.0.6"})
	c.RewriteMeshDNSSnapshot() // the heartbeat
	assert.NoFileExists(t, path, "a standby must not write the mesh-DNS snapshot")

	close(owned)
	require.Eventually(t, func() bool {
		snap, err := meshdns.ReadSnapshot(path)
		return err == nil && snap.Records["default/echo"] == "10.111.0.6"
	}, 5*time.Second, eventuallyTick)
}

// TestNodeWriteGate_NilGateWritesAsBefore keeps the edge and every
// pre-041 caller byte-identical.
func TestNodeWriteGate_NilGateWritesAsBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh-dns", "records.json")
	c := newTestCache("node-1")
	c.SetNodeWriteGate(nil)
	c.SetMeshDNSSnapshotPath(path)
	c.SetMeshDNSRecords(map[string]string{"default/echo": "10.111.0.6"})
	assert.FileExists(t, path)
}

// TestReloadNodeState_RestartsTheQUICFetchWindow: the #1033 prune window is
// measured from when the agent starts SERVING; a standby serves nothing.
func TestReloadNodeState_RestartsTheQUICFetchWindow(t *testing.T) {
	c := newTestCache("node-1")
	c.depMu.Lock()
	c.quicStart = time.Now().Add(-2 * time.Hour)
	c.depMu.Unlock()
	require.NoError(t, c.ReloadNodeState(context.Background()))
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	assert.WithinDuration(t, time.Now(), c.quicStart, time.Minute)
}
