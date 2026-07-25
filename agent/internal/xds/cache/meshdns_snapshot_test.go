package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/meshdns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetMeshDNSRecordsGeneration: the generation advances only when the record
// CONTENT changes, so the resolver daemon can tell a real update apart from the
// freshness heartbeat's re-stamp of an unchanged table.
func TestSetMeshDNSRecordsGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh-dns", "records.json")
	c := newTestCache("node-1")
	c.SetMeshDNSSnapshotPath(path)

	c.SetMeshDNSRecords(map[string]string{"default/echo": "10.111.0.6"})
	first, err := meshdns.ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.Generation, "the first projection is generation 1")

	// Content-equal replacement: still written (freshness), same generation.
	c.SetMeshDNSRecords(map[string]string{"default/echo": "10.111.0.6"})
	same, err := meshdns.ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), same.Generation, "an unchanged table does not bump the generation")

	c.SetMeshDNSRecords(map[string]string{"default/echo": "10.222.0.9"})
	changed, err := meshdns.ReadSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), changed.Generation, "a content change bumps the generation")
	assert.Equal(t, map[string]string{"default/echo": "10.222.0.9"}, changed.Records)
}

// TestRewriteMeshDNSSnapshot: the heartbeat re-stamps writtenAt on the last projected
// table without touching its records or generation.
func TestRewriteMeshDNSSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh-dns", "records.json")
	c := newTestCache("node-1")
	c.SetMeshDNSSnapshotPath(path)

	records := map[string]string{"default/echo": "10.111.0.6"}
	c.SetMeshDNSRecords(records)
	before, err := meshdns.ReadSnapshot(path)
	require.NoError(t, err)

	// Back-date the snapshot on disk so a re-stamp is observable at one-second
	// resolution (writtenAt lives in the file content, not the mtime).
	stale := time.Now().Add(-5 * time.Minute)
	data, err := json.Marshal(&meshdns.Snapshot{
		WrittenAt:  stale.Unix(),
		Generation: before.Generation,
		Records:    records,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	c.RewriteMeshDNSSnapshot()

	after, err := meshdns.ReadSnapshot(path)
	require.NoError(t, err)
	assert.Greater(t, after.WrittenAt, stale.Unix(), "the heartbeat re-stamps freshness")
	assert.Equal(t, before.Generation, after.Generation, "a heartbeat is not a content change")
	assert.Equal(t, records, after.Records, "records are unchanged")
}

// TestRewriteMeshDNSSnapshotBeforeFirstProjection: the heartbeat must NOT create a
// snapshot before the capture reconciler has ever projected. Writing an empty table
// would flip the resolver ready and NXDOMAIN the entire mesh.
func TestRewriteMeshDNSSnapshotBeforeFirstProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh-dns", "records.json")
	c := newTestCache("node-1")
	c.SetMeshDNSSnapshotPath(path)

	c.RewriteMeshDNSSnapshot()

	_, err := meshdns.ReadSnapshot(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "no snapshot should have been created")
}

// TestMeshDNSSnapshotDisabled: with mesh DNS off (no snapshot path) both the projection
// and the heartbeat are inert.
func TestMeshDNSSnapshotDisabled(t *testing.T) {
	c := newTestCache("node-1")
	assert.NotPanics(t, func() {
		c.SetMeshDNSRecords(map[string]string{"default/echo": "10.111.0.6"})
		c.RewriteMeshDNSSnapshot()
	})
}
