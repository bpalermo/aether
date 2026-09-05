package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	agentv1 "aethermesh.dev/api/aether/agent/v1"
	"aethermesh.dev/common/file"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ObservedUpstreamsFile is the path of the persisted observed demand set
// (issue #701), RELATIVE to the agent's local storage directory
// (--mounted-registry-dir, the same host-persistent directory the CNI pod
// records live in).
//
// It sits in a subdirectory on purpose: the storage package treats every
// top-level "*.json" in that directory as one CNI pod record (loadAll) and the
// ghost sweep prunes pod records it cannot match to a live container, so a
// file dropped beside them would be parsed as an empty pod and then deleted.
const ObservedUpstreamsFile = "state/observed-upstreams.json"

// defaultObservedFlushDebounce coalesces a burst of observed-set changes — a
// cold node warming several upstreams at once, a prune pass expiring many —
// into one write.
const defaultObservedFlushDebounce = time.Second

// restoredKeysLogCap bounds how many restored service keys the restore log
// line names before it degrades to a count.
const restoredKeysLogCap = 50

var (
	observedMarshal   = protojson.MarshalOptions{Multiline: true, Indent: "  "}
	observedUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// EnableObservedUpstreamsStore turns on persistence of the OBSERVED half of the
// node dependency set at path and restores whatever a previous agent on this
// node left there. Call once, before the first snapshot is generated — the
// point of the restore is that the very first push already carries the
// clusters the node's traffic was using when the previous agent stopped.
//
// Why (issue #701): the observed set is process-local memory. An agent-only
// restart is repaired by the proxy's held-cluster inventory
// (RestoreDependency, #698), but a FULL agent+proxy replacement — every Helm
// upgrade — leaves a fresh proxy with nothing to report and a fresh agent with
// nothing observed, so on a node with no local replica of an upstream the
// first requests take the cold ODCDS path: ~one bucket of 503s (+57/+65
// prober http_error, ~5s) per such node per roll on talos (rev195,
// 2026-09-05). Declared upstreams never pay this because an annotation
// survives the restart; this makes an observation survive it too.
//
// Restored entries are ordinary TTL'd observations — never live-subscription
// pins, and their original deadline carries over — so the demand set still
// shrinks exactly as it would have without the restart. A missing, corrupt or
// unreadable file logs and falls back to the cold path; it never fails the
// agent.
func (c *SnapshotCache) EnableObservedUpstreamsStore(ctx context.Context, path string) {
	if path == "" {
		return
	}
	c.depMu.Lock()
	c.observedStorePath = path
	c.depMu.Unlock()
	c.restoreObservedUpstreams(ctx, path)
}

// FlushObservedUpstreams writes the observed set now if a change is pending,
// short-circuiting the debounce. For shutdown: a change made in the last
// debounce window before SIGTERM would otherwise be lost.
func (c *SnapshotCache) FlushObservedUpstreams() {
	c.depMu.Lock()
	if t := c.observedFlushTimer; t != nil {
		// Stopping a fired timer is harmless: the flush below serializes with
		// the timer's own flush on observedWriteMu, and whichever runs second
		// finds nothing dirty.
		t.Stop()
	}
	c.depMu.Unlock()
	c.flushObservedUpstreams()
}

// restoreObservedUpstreams reads the persisted set at path into observedDeps,
// skipping entries already past their deadline, and reports what it did.
func (c *SnapshotCache) restoreObservedUpstreams(ctx context.Context, path string) {
	stored, ok := c.readObservedUpstreams(ctx, path)
	if !ok {
		return
	}
	restored, skipped := c.admitStoredUpstreams(time.Now(), c.observedTTLValue(), stored.GetUpstreams())
	if skipped > 0 {
		c.log.InfoContext(ctx, "skipped expired or malformed persisted observed upstreams", "count", skipped, "path", path)
	}
	if len(restored) == 0 {
		return
	}
	attrs := []any{"count", len(restored), "path", path}
	if len(restored) <= restoredKeysLogCap {
		attrs = append(attrs, "services", restored)
	}
	c.log.InfoContext(ctx, fmt.Sprintf("restored %d observed upstreams from local storage", len(restored)), attrs...)
	c.metrics.UpstreamsRestored(ctx, int64(len(restored)))
}

// readObservedUpstreams loads and decodes the persisted set. A missing file is
// the normal first boot (Debug); anything else unreadable is a WARN and the
// agent starts cold — fail open, never fail the agent.
func (c *SnapshotCache) readObservedUpstreams(ctx context.Context, path string) (*agentv1.ObservedUpstreams, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.log.DebugContext(ctx, "no persisted observed upstreams; starting cold", "path", path)
		} else {
			c.log.WarnContext(ctx, "cannot read persisted observed upstreams; starting cold", "path", path, "error", err)
		}
		return nil, false
	}
	stored := &agentv1.ObservedUpstreams{}
	if err := observedUnmarshal.Unmarshal(data, stored); err != nil {
		c.log.WarnContext(ctx, "ignoring corrupt persisted observed upstreams; starting cold", "path", path, "error", err)
		return nil, false
	}
	return stored, true
}

// admitStoredUpstreams merges the persisted entries into observedDeps. An entry
// is skipped when malformed or already expired; one for a service that is
// already observed (a RestoreDependency from the proxy's held clusters can
// legitimately run first) is left alone — union, never override. Returns the
// admitted service keys, sorted, and how many entries were skipped.
func (c *SnapshotCache) admitStoredUpstreams(now time.Time, ttl time.Duration, entries []*agentv1.ObservedUpstream) (restored []string, skipped int) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	for _, e := range entries {
		svc, exp := e.GetService(), e.GetExpiresAt()
		if svc == "" || exp == nil || !exp.AsTime().After(now) {
			skipped++
			continue
		}
		if _, known := c.observedDeps[svc]; known {
			continue
		}
		// Carry the ORIGINAL deadline over. The idle TTL is measured from the
		// observation timestamp, so store the instant that makes last+ttl land
		// exactly on the persisted expires_at — never "now": resetting the TTL
		// on restore would let a stale set outlive its hour by riding across
		// restarts.
		c.observedDeps[svc] = exp.AsTime().Add(-ttl)
		restored = append(restored, svc)
	}
	if len(restored) > 0 {
		c.bumpDepGenLocked()
	}
	if skipped > 0 {
		// Rewrite the file without the dead entries.
		c.markObservedDirtyLocked()
	}
	sort.Strings(restored)
	return restored, skipped
}

// markObservedDirtyLocked schedules a debounced write of the observed set.
// Every writer of observedDeps calls it after the write; the flush itself
// runs on the timer's goroutine, never on the read path. A no-op when
// persistence is off. Caller must hold depMu for writing.
func (c *SnapshotCache) markObservedDirtyLocked() {
	if c.observedStorePath == "" {
		return
	}
	c.observedDirty = true
	if c.observedFlushTimer != nil {
		return
	}
	c.observedFlushTimer = time.AfterFunc(c.observedFlushDebounceValue(), c.flushObservedUpstreams)
}

// flushObservedUpstreams writes the observed set if a change is pending.
// Flushes are serialized so a slow earlier write can never land over a newer
// one; the set is copied under depMu and encoded and written outside it.
func (c *SnapshotCache) flushObservedUpstreams() {
	c.observedWriteMu.Lock()
	defer c.observedWriteMu.Unlock()
	path, stored, ok := c.takeObservedSnapshot()
	if !ok {
		return
	}
	c.writeObservedUpstreams(path, stored)
}

// takeObservedSnapshot clears the pending-write state and returns the encoded
// form of the observed set, or ok=false when nothing is pending. Disarming the
// timer here means a change landing after this returns arms a fresh one.
func (c *SnapshotCache) takeObservedSnapshot() (path string, stored *agentv1.ObservedUpstreams, ok bool) {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	c.observedFlushTimer = nil
	if !c.observedDirty || c.observedStorePath == "" {
		return "", nil, false
	}
	c.observedDirty = false
	return c.observedStorePath, c.observedUpstreamsLocked(c.observedTTLValue()), true
}

// observedUpstreamsLocked encodes observedDeps in service-key order (a
// deterministic file diffs cleanly and restores reproducibly). Caller must
// hold depMu.
func (c *SnapshotCache) observedUpstreamsLocked(ttl time.Duration) *agentv1.ObservedUpstreams {
	names := make([]string, 0, len(c.observedDeps))
	for svc := range c.observedDeps {
		names = append(names, svc)
	}
	sort.Strings(names)
	entries := make([]*agentv1.ObservedUpstream, 0, len(names))
	for _, svc := range names {
		last := c.observedDeps[svc]
		entries = append(entries, agentv1.ObservedUpstream_builder{
			Service:    svc,
			ObservedAt: timestamppb.New(last),
			ExpiresAt:  timestamppb.New(last.Add(ttl)),
		}.Build())
	}
	return agentv1.ObservedUpstreams_builder{Upstreams: entries}.Build()
}

// writeObservedUpstreams encodes and atomically writes the set. A failure is a
// WARN and nothing more: the next change re-arms the write, and the worst case
// is the cold path this file exists to avoid.
func (c *SnapshotCache) writeObservedUpstreams(path string, stored *agentv1.ObservedUpstreams) {
	data, err := observedMarshal.Marshal(stored)
	if err != nil {
		c.log.Warn("failed to encode observed upstreams for persistence", "path", path, "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.log.Warn("failed to create the observed upstreams store directory", "path", path, "error", err)
		return
	}
	if err := file.WriteFileAtomic(path, data); err != nil {
		c.log.Warn("failed to persist observed upstreams", "path", path, "error", err)
		return
	}
	c.log.Debug("persisted observed upstreams", "count", len(stored.GetUpstreams()), "path", path)
}

// observedFlushDebounceValue returns the configured write debounce (test hook).
func (c *SnapshotCache) observedFlushDebounceValue() time.Duration {
	if c.observedFlushDebounce > 0 {
		return c.observedFlushDebounce
	}
	return defaultObservedFlushDebounce
}
