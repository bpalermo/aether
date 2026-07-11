package replicator

import (
	"context"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// DefaultLeaseTTLSeconds is the origin-heartbeat lease TTL (proposal 006
// Phase 2b). It bounds two windows at once: how long a dead origin's mirror
// lingers on peers (failover cleanup latency) and how long a leader handoff
// has to re-sync before the mirror expires mid-roll. 30s comfortably covers
// registrar leader election while keeping whole-region cleanup prompt.
const DefaultLeaseTTLSeconds = 30

// peerLease is one peer etcd's origin-heartbeat lease: every key this
// replicator mirrors into that peer is attached to it, and this replicator's
// KeepAlive is the only thing refreshing it. Origin liveness == mirror
// liveness: if this region (or its replicator leader) dies, the lease lapses
// and the peer drops the whole mirrored subtree together — failover cleanup
// without any peer judging the origin dead.
type peerLease struct {
	id clientv3.LeaseID
	// done closes when the keepalive stream ends — context cancellation on
	// clean shutdown, or a lost/expired lease. The mirror loop treats it as
	// fatal for the connection: redial, re-grant, resync.
	done <-chan struct{}
}

// startLease grants a fresh lease on the peer and holds its keepalive until
// ctx ends. Deliberately NO Revoke on shutdown: revoking deletes every
// attached key immediately, which would wipe this region's mirror off peers
// on every registrar roll / leader handoff. Instead the lease rides out its
// TTL — the next leader re-syncs under a fresh lease well inside it (puts
// re-attach the keys), and the old lease then expires holding nothing. Only
// a region that truly stays down lets the TTL lapse with keys attached.
func startLease(ctx context.Context, peerCli *clientv3.Client, ttlSeconds int64) (*peerLease, error) {
	grant, err := peerCli.Grant(ctx, ttlSeconds)
	if err != nil {
		return nil, fmt.Errorf("grant: %w", err)
	}
	ka, err := peerCli.KeepAlive(ctx, grant.ID)
	if err != nil {
		return nil, fmt.Errorf("keepalive: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Drain until the client closes the stream (ctx end or lease loss).
		for range ka {
		}
	}()
	return &peerLease{id: grant.ID, done: done}, nil
}
