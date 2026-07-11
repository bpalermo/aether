// Package replicator is the registrar's cross-region etcd replicator
// (proposal 006 Phase 2a). Leader-elected (one mirror stream per region), it
// watches this region's OWN authoritative subtree
// (<root>/<region>/clusters/<cluster>/…) in the local etcd and replays every
// change verbatim (Put→Put, Delete→Delete) into each peer region's etcd.
//
// Partitions are origin-first and disjoint, so mirroring is loop-free by
// construction: a peer's replicator watches only its own subtree and never
// re-mirrors keys this region wrote into it. Peer registrars see the mirrored
// keys as read-only foreign endpoints (EDS priority 2, proposal 004 locality).
//
// Phase 2a is the mirror loop only: mirrored writes carry no lease yet, so a
// dead origin's keys persist on peers until it returns. The origin-heartbeat
// lease (whole-region failover) is Phase 2b.
package replicator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	commonlog "github.com/bpalermo/aether/common/log"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	defaultDialTimeout   = 5 * time.Second
	defaultResyncBackoff = 5 * time.Second
)

// Source is the local authoritative store the replicator mirrors from: the
// etcd-backed registry's client plus its own-partition root. Implemented by
// registry.EtcdRegistry; other backends don't, which is what gates the
// replicator to the etcd backend.
type Source interface {
	Client() *clientv3.Client
	OwnPrefix() string
}

// Peer is one peer region's etcd, the mirror destination.
type Peer struct {
	Region    string
	Endpoints []string
}

// ParsePeers parses repeated --peer-etcd values of the form
// <region>=<endpoint>[,<endpoint>...] into Peers. The region is the
// replication unit, so the own region must be explicit (not the single-region
// default) and peer regions must be distinct from it and from each other.
func ParsePeers(entries []string, ownRegion string) ([]Peer, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if ownRegion == "" {
		return nil, errors.New("--region must be set explicitly when peer replication is enabled")
	}
	seen := make(map[string]struct{}, len(entries))
	peers := make([]Peer, 0, len(entries))
	for _, entry := range entries {
		region, eps, ok := strings.Cut(entry, "=")
		if !ok || region == "" || eps == "" {
			return nil, fmt.Errorf("malformed peer %q: want <region>=<endpoint>[,<endpoint>...]", entry)
		}
		if region == ownRegion {
			return nil, fmt.Errorf("peer region %q is this registrar's own region", region)
		}
		if _, dup := seen[region]; dup {
			return nil, fmt.Errorf("duplicate peer region %q", region)
		}
		seen[region] = struct{}{}
		endpoints := strings.Split(eps, ",")
		for i := range endpoints {
			endpoints[i] = strings.TrimSpace(endpoints[i])
			if endpoints[i] == "" {
				return nil, fmt.Errorf("peer %q has an empty endpoint", entry)
			}
		}
		peers = append(peers, Peer{Region: region, Endpoints: endpoints})
	}
	return peers, nil
}

// Replicator mirrors the local own-partition subtree into each peer's etcd.
// It is a leader-elected manager Runnable: exactly one replica per region
// holds the mirror streams.
type Replicator struct {
	Source Source
	Peers  []Peer
	Log    *slog.Logger

	// DialTimeout bounds each peer dial; ResyncBackoff spaces mirror-loop
	// restarts after an error. Zero values take the defaults.
	DialTimeout   time.Duration
	ResyncBackoff time.Duration

	log *slog.Logger
}

// NeedLeaderElection makes the replicator run only on the elected leader
// (one mirror stream per origin region; two would double-write peers).
func (r *Replicator) NeedLeaderElection() bool { return true }

// Start runs one independent mirror loop per peer until the context is
// cancelled. Peers are isolated: a slow or unreachable peer resyncs on its
// own backoff without stalling the others.
func (r *Replicator) Start(ctx context.Context) error {
	r.log = commonlog.Named(r.Log, "replicator")
	if r.DialTimeout == 0 {
		r.DialTimeout = defaultDialTimeout
	}
	if r.ResyncBackoff == 0 {
		r.ResyncBackoff = defaultResyncBackoff
	}
	r.log.InfoContext(ctx, "starting cross-region replication",
		"ownPrefix", r.Source.OwnPrefix(), "peers", len(r.Peers))

	var wg sync.WaitGroup
	for _, p := range r.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runPeer(ctx, r.log.With("peerRegion", p.Region), p)
		}()
	}
	wg.Wait()
	return nil
}

// runPeer redials and re-mirrors one peer until the context ends.
func (r *Replicator) runPeer(ctx context.Context, log *slog.Logger, p Peer) {
	for {
		if err := r.mirrorPeer(ctx, log, p); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "peer mirror failed; will redial and resync", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.ResyncBackoff):
		}
	}
}

// mirrorPeer dials the peer and runs sync→watch cycles until the context ends
// or an error forces a redial.
func (r *Replicator) mirrorPeer(ctx context.Context, log *slog.Logger, p Peer) error {
	peerCli, err := clientv3.New(clientv3.Config{
		Endpoints:   p.Endpoints,
		DialTimeout: r.DialTimeout,
		Context:     ctx,
	})
	if err != nil {
		return fmt.Errorf("dial peer %s: %w", p.Region, err)
	}
	defer func() { _ = peerCli.Close() }()

	for {
		rev, err := r.syncFull(ctx, peerCli)
		if err != nil {
			return fmt.Errorf("full sync: %w", err)
		}
		log.InfoContext(ctx, "peer mirror synced; watching", "revision", rev)
		err = r.watchMirror(ctx, peerCli, rev+1)
		if ctx.Err() != nil {
			return nil
		}
		// A compacted start revision just means the watch fell behind: re-range
		// and resume. Anything else (peer write failure, watch stream error)
		// redials.
		if errors.Is(err, rpctypes.ErrCompacted) {
			log.WarnContext(ctx, "watch revision compacted; resyncing")
			continue
		}
		return err
	}
}

// syncFull makes the peer's copy of the own-partition subtree equal the local
// one — put missing/changed keys, delete extras — and returns the local
// revision the sync is consistent at (the watch resumes right after it).
// Reconciling against the peer's existing copy rather than blind re-putting
// makes restarts resume-safe: deletes that happened while the replicator was
// down still converge.
func (r *Replicator) syncFull(ctx context.Context, peerCli *clientv3.Client) (int64, error) {
	prefix := r.Source.OwnPrefix()
	localResp, err := r.Source.Client().Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, fmt.Errorf("range local: %w", err)
	}
	peerResp, err := peerCli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, fmt.Errorf("range peer: %w", err)
	}

	local := make(map[string][]byte, len(localResp.Kvs))
	for _, kv := range localResp.Kvs {
		local[string(kv.Key)] = kv.Value
	}
	for _, kv := range peerResp.Kvs {
		key := string(kv.Key)
		want, ok := local[key]
		if !ok {
			if _, err = peerCli.Delete(ctx, key); err != nil {
				return 0, fmt.Errorf("delete stale peer key: %w", err)
			}
			continue
		}
		if bytes.Equal(kv.Value, want) {
			delete(local, key) // already converged; skip the put below
		}
	}
	for key, value := range local {
		if _, err = peerCli.Put(ctx, key, string(value)); err != nil {
			return 0, fmt.Errorf("put peer key: %w", err)
		}
	}
	return localResp.Header.Revision, nil
}

// watchMirror replays local watch events verbatim into the peer, starting at
// startRev. Returns when the watch or a peer write fails; the caller decides
// between compaction-resync and redial.
func (r *Replicator) watchMirror(ctx context.Context, peerCli *clientv3.Client, startRev int64) error {
	wch := r.Source.Client().Watch(clientv3.WithRequireLeader(ctx), r.Source.OwnPrefix(),
		clientv3.WithPrefix(), clientv3.WithRev(startRev))
	for resp := range wch {
		if err := resp.Err(); err != nil {
			return err
		}
		for _, ev := range resp.Events {
			key := string(ev.Kv.Key)
			switch ev.Type {
			case clientv3.EventTypePut:
				if _, err := peerCli.Put(ctx, key, string(ev.Kv.Value)); err != nil {
					return fmt.Errorf("mirror put: %w", err)
				}
			case clientv3.EventTypeDelete:
				if _, err := peerCli.Delete(ctx, key); err != nil {
					return fmt.Errorf("mirror delete: %w", err)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("local watch channel closed")
}
