package cache

import (
	"context"
	"time"
)

// SetNodeWriteGate holds every node-local file write of this cache until owned
// is closed (proposal 041, "Persisted state, two writers"). Call once at boot,
// before the manager starts; nil (the default) never holds anything.
//
// While a surge-rolled standby agent builds its first snapshot, the agent that
// still owns the node keeps writing the observed demand set with its QUIC
// pairs (#701, #1033) and the mesh-DNS record snapshot (#578, #586), and
// flushes the observed set once more on its way out. A standby that wrote too
// would race it — and could land a set from before the old agent's last
// observations over the file it is about to restore from. So the standby keeps
// those changes in memory (marking them pending), and when owned closes:
//
//   - the observed set is flushed once, whatever is pending, after
//     ReloadNodeState has merged in the old agent's final flush;
//   - the mesh-DNS table is re-stamped from the last projection, so the
//     resolver daemon sees a live writer again within milliseconds of the
//     handoff rather than at the next heartbeat.
func (c *SnapshotCache) SetNodeWriteGate(owned <-chan struct{}) {
	if owned == nil {
		return
	}
	c.nodeWrites = owned
	go func() {
		<-owned
		c.depMu.Lock()
		if c.observedStorePath != "" {
			c.observedDirty = true
		}
		c.depMu.Unlock()
		c.FlushObservedUpstreams()
		c.RewriteMeshDNSSnapshot()
	}()
}

// nodeWritesAllowed reports whether this cache may write node-local files.
func (c *SnapshotCache) nodeWritesAllowed() bool {
	if c.nodeWrites == nil {
		return true
	}
	select {
	case <-c.nodeWrites:
		return true
	default:
		return false
	}
}

// ReloadNodeState merges what the previous owner of the node persisted after
// this agent started — its observed upstreams, QUIC pairs and their
// confirmations, including the final flush it made on exit — into this cache.
// A takeover step (proposal 041): it runs after the node lock is held, so that
// flush has landed, and before ownership is announced, so the first write this
// agent makes is the union. The merge is the restore's own union semantics:
// nothing this agent observed itself is overridden.
//
// It also restarts the QUIC pair fetch window (#1033): the window is "how long
// this agent has SERVED without seeing a pair used", and a standby serves
// nothing.
func (c *SnapshotCache) ReloadNodeState(ctx context.Context) error {
	c.depMu.Lock()
	path := c.observedStorePath
	c.quicStart = time.Now()
	c.depMu.Unlock()
	if path != "" {
		c.restoreObservedUpstreams(ctx, path)
		c.signalDependencyChange()
	}
	return nil
}
