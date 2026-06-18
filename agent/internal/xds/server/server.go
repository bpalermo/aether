// Package server implements the agent-specific Envoy xDS server.
// It builds xDS snapshots from local pod storage and the service registry,
// generating Envoy listeners, clusters, endpoints, and routes.
package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
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
}

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

// PreListen generates the initial Envoy snapshot from local pod storage and the service registry.
// It creates listeners, clusters, endpoints, and routes, then sets the snapshot in the cache
// before the server starts accepting xDS client connections.
func (s *AgentXdsServer) PreListen(ctx context.Context) error {
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
