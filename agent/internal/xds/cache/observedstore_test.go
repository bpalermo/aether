package cache

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "aethermesh.dev/api/aether/agent/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	storeTestService  = "aether-test/echo"
	storeTestService2 = "aether-test/svc-2"
	restoredCtr       = "aether.agent.upstreams.restored"
	missCtr           = "aether.agent.upstreams.miss"
	eventuallyWait    = 2 * time.Second
	eventuallyTick    = 5 * time.Millisecond
)

// storePath returns the persisted observed-set file under a test storage dir,
// laid out exactly as the agent lays it out (the state/ subdirectory).
func storePath(dir string) string { return filepath.Join(dir, ObservedUpstreamsFile) }

// storedEntry builds one persisted entry expiring at exp.
func storedEntry(service string, exp time.Time) *agentv1.ObservedUpstream {
	return agentv1.ObservedUpstream_builder{
		Service:    service,
		ObservedAt: timestamppb.New(exp.Add(-defaultObservedTTL)),
		ExpiresAt:  timestamppb.New(exp),
	}.Build()
}

// writeStore persists entries to path the way a previous agent would have.
func writeStore(t *testing.T, path string, entries ...*agentv1.ObservedUpstream) {
	t.Helper()
	data, err := observedMarshal.Marshal(agentv1.ObservedUpstreams_builder{Upstreams: entries}.Build())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// readStore decodes the persisted set at path.
func readStore(t *testing.T, path string) *agentv1.ObservedUpstreams {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	stored := &agentv1.ObservedUpstreams{}
	require.NoError(t, observedUnmarshal.Unmarshal(data, stored))
	return stored
}

// storedServices returns the service keys persisted at path, in file order.
// A polling probe for Eventually: a missing or (still) undecodable file is nil,
// never a failure — the strict read is readStore.
func storedServices(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	stored := &agentv1.ObservedUpstreams{}
	if err := observedUnmarshal.Unmarshal(data, stored); err != nil {
		return nil
	}
	var out []string
	for _, e := range stored.GetUpstreams() {
		out = append(out, e.GetService())
	}
	return out
}

// newStoreTestCache builds a cache with captured logs and metrics, a short
// write debounce, and persistence enabled at the given file (restoring it).
func newStoreTestCache(t *testing.T, path string) (*SnapshotCache, *recorder, *sdkmetric.ManualReader) {
	t.Helper()
	c, rec, reader := newBindingTestCache(t)
	c.SetMeshDomain("aether.internal")
	c.observedFlushDebounce = 10 * time.Millisecond
	c.EnableObservedUpstreamsStore(context.Background(), path)
	return c, rec, reader
}

// TestObservedUpstreamsStore_PersistsOnMiss: an ODCDS miss lands in the file
// after the debounce, with the deadline the cache itself computed.
func TestObservedUpstreamsStore_PersistsOnMiss(t *testing.T) {
	path := storePath(t.TempDir())
	c, _, reader := newStoreTestCache(t, path)
	require.NoFileExists(t, path, "nothing to restore, nothing written")

	before := time.Now()
	require.True(t, c.ObserveDependency(context.Background(), storeTestService))
	require.Eventually(t, func() bool { return len(storedServices(t, path)) == 1 }, eventuallyWait, eventuallyTick)

	entry := readStore(t, path).GetUpstreams()[0]
	assert.Equal(t, storeTestService, entry.GetService())
	observed := entry.GetObservedAt().AsTime()
	assert.False(t, observed.Before(before.Truncate(0)), "observed_at is the observation timestamp")
	assert.False(t, observed.After(time.Now()))
	assert.True(t, entry.GetExpiresAt().AsTime().Equal(observed.Add(c.observedTTLValue())),
		"expires_at is observed_at + TTL, exactly as the cache expires it")
	assert.EqualValues(t, 1, counterValue(t, reader, missCtr), "a real miss is still a miss")
}

// TestObservedUpstreamsStore_RestoreCarriesTheDeadlineOver: a persisted entry
// re-enters the set with its ORIGINAL deadline (never a fresh TTL), counts as
// restored — not as a miss — and is named in the one INFO line; an expired or
// malformed entry is skipped and scrubbed from the file.
func TestObservedUpstreamsStore_RestoreCarriesTheDeadlineOver(t *testing.T) {
	path := storePath(t.TempDir())
	// Truncate so the protojson round trip is exact.
	exp := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	writeStore(
		t, path,
		storedEntry(storeTestService, exp),
		storedEntry("aether-test/stale", time.Now().Add(-time.Minute)),
		storedEntry("", exp),
	)

	c, rec, reader := newStoreTestCache(t, path)

	set := c.DependencySet()
	assert.Contains(t, set, storeTestService)
	assert.NotContains(t, set, "aether-test/stale", "an entry past its deadline is not restored")
	assert.Len(t, set, 1)

	c.depMu.RLock()
	last := c.observedDeps[storeTestService]
	c.depMu.RUnlock()
	assert.True(t, last.Add(c.observedTTLValue()).Equal(exp), "the persisted deadline carries over; the TTL is NOT reset")

	assert.EqualValues(t, 1, counterValue(t, reader, restoredCtr))
	assert.EqualValues(t, 0, counterValue(t, reader, missCtr), "a restore is not a miss")
	_, observed := c.dependencyCounts()
	assert.Equal(t, 1, observed, "restored entries count as observed upstreams")

	lines := rec.with("restored 1 observed upstreams from local storage")
	require.Len(t, lines, 1)
	assert.Equal(t, slog.LevelInfo, lines[0].level)
	assert.Contains(t, lines[0].attrs["services"], storeTestService)

	// The dead entries were skipped, so the file is rewritten without them.
	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual([]string{storeTestService}, storedServices(t, path))
	}, eventuallyWait, eventuallyTick)
}

// TestObservedUpstreamsStore_RestoreLogsCountOnlyPastTheCap: more than 50 keys
// are reported as a count, not a list.
func TestObservedUpstreamsStore_RestoreLogsCountOnlyPastTheCap(t *testing.T) {
	path := storePath(t.TempDir())
	exp := time.Now().Add(time.Hour)
	entries := make([]*agentv1.ObservedUpstream, 0, restoredKeysLogCap+1)
	for i := range restoredKeysLogCap + 1 {
		entries = append(entries, storedEntry(fmt.Sprintf("ns/svc-%02d", i), exp))
	}
	writeStore(t, path, entries...)

	c, rec, _ := newStoreTestCache(t, path)
	assert.Len(t, c.DependencySet(), restoredKeysLogCap+1)
	lines := rec.with("restored 51 observed upstreams from local storage")
	require.Len(t, lines, 1)
	assert.Equal(t, "51", lines[0].attrs["count"])
	assert.NotContains(t, lines[0].attrs, "services", "past the cap the line carries the count only")
}

// TestObservedUpstreamsStore_CorruptFileStartsCold: garbage on disk is a WARN
// and an empty set, never an error — and persistence still works afterwards.
func TestObservedUpstreamsStore_CorruptFileStartsCold(t *testing.T) {
	path := storePath(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	c, rec, reader := newStoreTestCache(t, path)
	assert.Empty(t, c.DependencySet())
	assert.EqualValues(t, 0, counterValue(t, reader, restoredCtr))
	warns := rec.with("ignoring corrupt persisted observed upstreams; starting cold")
	require.Len(t, warns, 1)
	assert.Equal(t, slog.LevelWarn, warns[0].level)

	require.True(t, c.ObserveDependency(context.Background(), storeTestService))
	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual([]string{storeTestService}, storedServices(t, path))
	}, eventuallyWait, eventuallyTick, "the corrupt file is overwritten by the next change")
}

// TestObservedUpstreamsStore_ShrinkRemovesTheEntry: an entry that ages out
// of the demand set leaves the file on the next debounced write.
func TestObservedUpstreamsStore_ShrinkRemovesTheEntry(t *testing.T) {
	path := storePath(t.TempDir())
	c, _, _ := newStoreTestCache(t, path)
	c.observedTTL = 30 * time.Millisecond

	require.True(t, c.ObserveDependency(context.Background(), storeTestService))
	require.Eventually(t, func() bool { return len(storedServices(t, path)) == 1 }, eventuallyWait, eventuallyTick)

	time.Sleep(40 * time.Millisecond)
	c.PruneObservedDependencies()
	assert.NotContains(t, c.DependencySet(), storeTestService)
	require.Eventually(t, func() bool { return len(storedServices(t, path)) == 0 }, eventuallyWait, eventuallyTick,
		"the expired entry is gone from the file")
}

// TestObservedUpstreamsStore_UnionsWithResumeHeldClusters: the file and the
// proxy's held-cluster re-seed (#698) are a union — either may add, neither
// removes — and both halves end up persisted.
func TestObservedUpstreamsStore_UnionsWithResumeHeldClusters(t *testing.T) {
	path := storePath(t.TempDir())
	writeStore(t, path, storedEntry(storeTestService, time.Now().Add(time.Hour)))
	c, _, reader := newStoreTestCache(t, path)
	ctx := context.Background()

	assert.False(t, c.RestoreDependency(ctx, storeTestService), "already restored from the file: not new")
	assert.True(t, c.RestoreDependency(ctx, storeTestService2), "the proxy's held cluster adds to the union")

	set := c.DependencySet()
	assert.Contains(t, set, storeTestService)
	assert.Contains(t, set, storeTestService2)
	assert.EqualValues(t, 0, counterValue(t, reader, missCtr))
	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual([]string{storeTestService, storeTestService2}, storedServices(t, path))
	}, eventuallyWait, eventuallyTick, "both sources are persisted, in key order")
}

// TestObservedUpstreamsStore_FlushShortCircuitsTheDebounce: the shutdown flush
// writes a pending change immediately, and a second flush has nothing to do.
func TestObservedUpstreamsStore_FlushShortCircuitsTheDebounce(t *testing.T) {
	path := storePath(t.TempDir())
	c, _, _ := newStoreTestCache(t, path)
	c.observedFlushDebounce = time.Hour

	require.True(t, c.ObserveDependency(context.Background(), storeTestService))
	require.NoFileExists(t, path, "still inside the debounce window")

	c.FlushObservedUpstreams()
	assert.Equal(t, []string{storeTestService}, storedServices(t, path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	c.FlushObservedUpstreams()
	again, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime(), again.ModTime(), "nothing dirty: no rewrite")
}

// TestObservedUpstreamsStore_DisabledWritesNothing: without a store path the
// observed set is process-local, exactly as before.
func TestObservedUpstreamsStore_DisabledWritesNothing(t *testing.T) {
	dir := t.TempDir()
	c := newTestCache("node-1")
	c.observedFlushDebounce = 5 * time.Millisecond

	require.True(t, c.ObserveDependency(context.Background(), storeTestService))
	c.FlushObservedUpstreams()
	time.Sleep(20 * time.Millisecond)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	assert.False(t, c.observedDirty)
	assert.Nil(t, c.observedFlushTimer)
}
