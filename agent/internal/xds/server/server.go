// Package server implements the agent-specific Envoy xDS server.
// It builds xDS snapshots from local pod storage and the service registry,
// generating Envoy listeners, clusters, endpoints, and routes.
package server

import (
	"context"
	"log/slog"
	"time"

	"aethermesh.dev/agent/constants"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/xds"
	"aethermesh.dev/registry"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

// AgentXdsServer is an xDS server that generates Envoy configuration from local pod storage
// and a service registry. It embeds xds.XdsServer and implements the ServerCallback interface
// to generate an initial snapshot before starting to accept connections.
//
// The server maintains versioned snapshots of Envoy resources (listeners, clusters, endpoints, routes)
// and serves them to local Envoy proxy instances via the xDS protocol.
type AgentXdsServer struct {
	xds.XdsServer

	log *slog.Logger

	clusterName string
	nodeName    string
	trustDomain string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	cache *cache.SnapshotCache

	// identity gates the first snapshot on this agent holding a mesh identity.
	// Optional: nil means "no identity to wait for" (--spire-enabled=false, and
	// the edge, whose source is still created synchronously).
	identity IdentityGate
}

// IdentityGate is the mesh identity as the xDS server needs to see it: is it
// here yet, and a channel to wait on until it is. commonspire.WaitingSource
// implements it.
type IdentityGate interface {
	// HasSVID reports whether this workload's identity (SVID and trust bundle)
	// is complete.
	HasSVID() bool
	// Ready returns a channel closed when that identity first arrives. It is
	// closed, not sent on, so any number of waiters may select on it.
	Ready() <-chan struct{}
}

// SetIdentityGate makes the initial snapshot wait for this agent's mesh
// identity. A setter rather than another positional argument to
// NewAgentXdsServer, for the same reason CNIServer.SetChainState is one: the
// gate is an interlock on WHEN the server may first serve, not something it
// needs in order to exist, and the edge builds the same server without one.
//
// Pass a nil gate (or never call this) to keep the pre-#740 behaviour.
func (s *AgentXdsServer) SetIdentityGate(gate IdentityGate) { s.identity = gate }

// NewAgentXdsServer creates a new AgentXdsServer.
// It initializes an xDS server with a snapshot cache and registers itself as a callback
// to generate the initial Envoy snapshot before listening for client connections.
// The server listens on a Unix domain socket at the default xDS socket path.
// callbacks (optional, may be nil) observe the discovery streams — the agent
// passes the ACK tracker's callbacks so pod lifecycle can await Envoy ACKs.
func NewAgentXdsServer(ctx context.Context, clusterName string, nodeName string, trustDomain string, registry registry.Registry, storage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache, callbacks serverv3.Callbacks, log *slog.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	// Watch the discovery streams for on-demand CDS subscriptions (ODCDS cold
	// path) alongside the caller's callbacks (the ACK tracker).
	observer := newOnDemandObserver(snapshotCache, registry, log)
	combined := combinedCallbacks{observer.Callbacks()}
	if callbacks != nil {
		combined = append(combined, callbacks)
	}

	// Store the registry in the cache so the edge reconciler can query mesh
	// service existence via HasRegistryService (registry-aware backend check).
	snapshotCache.SetRegistry(registry)

	aXdsServer := &AgentXdsServer{
		XdsServer:   xds.NewXdsServer(ctx, cfg, snapshotCache, combined, log),
		log:         commonlog.Named(log, "agent-xds"),
		clusterName: clusterName,
		nodeName:    nodeName,
		trustDomain: trustDomain,
		registry:    registry,
		storage:     storage,
		cache:       snapshotCache,
	}

	aXdsServer.AddCallback(aXdsServer)

	return aXdsServer, nil
}

// NeedLeaderElection returns false so the xDS server runs on EVERY replica, not
// just the leader. Each edge/agent pod serves xDS to its own co-located Envoy
// over a node-local UDS; leader-gating it would leave all non-leader proxies
// without a control plane and break data-plane HA. (The embedded xds.Server
// already declares this; AgentXdsServer states it explicitly so the
// per-pod-runnable contract is visible at this type.) On the node agent (leader
// election off) this method is a no-op.
func (s *AgentXdsServer) NeedLeaderElection() bool { return false }

// PreListen generates the initial Envoy snapshot from local pod storage and the service registry.
// It creates listeners, clusters, endpoints, and routes, then sets the snapshot in the cache
// before the server starts accepting xDS client connections.
//
// It first HOLDS until this agent has a mesh identity (see holdForIdentity):
// the whole point of the local-only fallback below is to survive a registrar
// blip, and without an SVID there is no such thing as a registrar blip — every
// handshake fails, so the fallback would fire on every restart and publish a
// snapshot with no cross-node endpoints at all.
func (s *AgentXdsServer) PreListen(ctx context.Context) error {
	if !s.holdForIdentity(ctx) {
		// Shutting down before identity arrived. Return nil rather than an error:
		// the manager is already stopping and a SPIRE outage must never be the
		// reason a shutdown is reported as a failure.
		return nil
	}

	s.log.DebugContext(ctx, "generating initial snapshot")

	if err := s.cache.LoadListenersFromStorage(ctx, s.storage, s.trustDomain); err != nil {
		s.log.ErrorContext(ctx, "failed to load listeners from storage", "error", err)
		return err
	}

	// The dependency set is now known (local pods loaded): scope the registry
	// watch to it before waiting on the watch cache, so the snapshot the
	// registrar streams is the filtered one (demand-scoped distribution).
	AssertWatchFilter(s.cache, s.registry)

	// Wait (bounded) for the registry watch cache to hold a complete snapshot
	// before deriving the initial config from it: a fresh agent that builds its
	// snapshot from an empty/partial cache opens the xDS socket serving a
	// route config with missing vhosts, and the reconnecting Envoy 404s live
	// traffic until the next refresh (rev-66 agent roll, 2026-06-11). On
	// timeout we proceed — the fallback below still serves local-only config
	// rather than crash-looping the node.
	if rw, ok := s.registry.(registry.ReadyWaiter); ok {
		waitCtx, cancel := context.WithTimeout(ctx, registryReadyTimeout)
		if err := rw.WaitReady(waitCtx); err != nil {
			s.log.InfoContext(ctx, "registry watch cache not complete in time; proceeding (RPC fallback / background retry will fill in)", "timeout", registryReadyTimeout.String(), "error", err.Error())
		}
		cancel()
	}

	// Registry unavailability must not prevent the agent from starting: a
	// crash-looping agent takes down the node's CNI ADD/DEL and xDS entirely,
	// turning a registrar blip into a node-wide outage (observed cascading
	// failure on talos-main, 2026-06-10). Serve the local-only snapshot now and
	// fill in registry-derived clusters/endpoints as soon as the registry
	// answers; the registry refresher keeps it current afterwards.
	if err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry); err != nil {
		s.log.ErrorContext(ctx, "registry unavailable for initial snapshot; starting with local-only config and retrying in background", "error", err)
		go s.retryInitialRegistryLoad(ctx)
	}

	return nil
}

// identityHoldLogInterval is how often the identity hold re-announces itself.
// Frequent enough that an operator watching the log sees it is a hold and not a
// hang, sparse enough that a long SPIRE outage does not fill the log — the
// authoritative per-attempt ladder (including the escalation to WARN) belongs
// to the identity source's own logger, not to this one.
const identityHoldLogInterval = 15 * time.Second

// holdForIdentity blocks until this agent holds a mesh identity, reporting
// whether it got one (false = ctx ended first). With no gate wired, or with the
// identity already in hand, it returns immediately.
//
// This is the difference between a restart during a SPIRE outage costing
// nothing and costing the node its data path (issue #740, finding 1). Envoy
// keeps serving the last configuration it was given whenever its management
// server is unreachable, which is exactly why the crash loop this replaced was
// survivable: a dying agent published nothing. A LIVING agent that opens the
// xDS socket and pushes what it can does the one thing the crash loop never
// did — it REPLACES a complete snapshot with a local-only one. On main-worker-03
// on 2026-09-07 that evicted every cross-node endpoint and failed ~95% of the
// node's mesh probes for the whole 6m41s outage (+5,083 errors), on a node whose
// Envoy had been serving fine a second earlier.
//
// So while identity is pending the agent programs everything else and simply
// does not open the socket. Envoy keeps its config, the node keeps working, and
// the readiness gate plus the node taint (past their dwell) stop NEW pods from
// landing on a node that cannot give them an identity — which is the honest
// signal, since a new pod is precisely what this state cannot serve.
//
// Mid-life is already correct and is deliberately left alone: once a snapshot
// has been published the cache keeps it, so a registrar that goes away later
// costs nothing — the refresher retries in the background and Envoy holds the
// last good config.
func (s *AgentXdsServer) holdForIdentity(ctx context.Context) bool {
	if s.identity == nil || s.identity.HasSVID() {
		return true
	}

	started := time.Now()
	s.log.InfoContext(ctx, "holding xDS until this agent has an SVID; Envoy keeps its current configuration", "elapsed", time.Duration(0).String())

	ticker := time.NewTicker(identityHoldLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.identity.Ready():
			s.log.InfoContext(ctx, "identity acquired; generating the initial snapshot", "held", time.Since(started).Round(time.Millisecond).String())
			return true
		case <-ticker.C:
			s.log.InfoContext(ctx, "holding xDS until this agent has an SVID; Envoy keeps its current configuration", "elapsed", time.Since(started).Round(time.Second).String())
		}
	}
}

// registryReadyTimeout bounds how long PreListen waits for the registry watch
// cache to hold a complete snapshot. Generous enough to cover a registrar
// restart finishing its first external-registry sync (~3-5s observed), small
// enough that a genuinely unavailable registrar cannot stall agent startup.
const registryReadyTimeout = 15 * time.Second

// retryInitialRegistryLoad retries the registry-derived snapshot load with
// capped exponential backoff until it succeeds or ctx ends.
func (s *AgentXdsServer) retryInitialRegistryLoad(ctx context.Context) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry); err != nil {
			s.log.DebugContext(ctx, "registry still unavailable; will retry", "backoff", backoff.String(), "error", err)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		s.log.InfoContext(ctx, "registry recovered; snapshot now includes registry-derived config")
		return
	}
}
