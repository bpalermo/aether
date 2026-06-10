// Package server implements the agent-specific Envoy xDS server.
// It builds xDS snapshots from local pod storage and the service registry,
// generating Envoy listeners, clusters, endpoints, and routes.
package server

import (
	"context"
	"time"

	"github.com/bpalermo/aether/agent/constants"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
)

// AgentXdsServer is an xDS server that generates Envoy configuration from local pod storage
// and a service registry. It embeds xds.XdsServer and implements the ServerCallback interface
// to generate an initial snapshot before starting to accept connections.
//
// The server maintains versioned snapshots of Envoy resources (listeners, clusters, endpoints, routes)
// and serves them to local Envoy proxy instances via the xDS protocol.
type AgentXdsServer struct {
	xds.XdsServer

	log logr.Logger

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
func NewAgentXdsServer(ctx context.Context, clusterName string, nodeName string, trustDomain string, registry registry.Registry, storage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache, log logr.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	aXdsServer := &AgentXdsServer{
		XdsServer:   xds.NewXdsServer(ctx, cfg, snapshotCache, nil, log),
		log:         log.WithName("agent-xds"),
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
	s.log.V(1).Info("generating initial snapshot")

	if err := s.cache.LoadListenersFromStorage(ctx, s.storage, s.trustDomain); err != nil {
		s.log.Error(err, "failed to load listeners from storage")
		return err
	}

	// Registry unavailability must not prevent the agent from starting: a
	// crash-looping agent takes down the node's CNI ADD/DEL and xDS entirely,
	// turning a registrar blip into a node-wide outage (observed cascading
	// failure on talos-main, 2026-06-10). Serve the local-only snapshot now and
	// fill in registry-derived clusters/endpoints as soon as the registry
	// answers; the registry refresher keeps it current afterwards.
	if err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry); err != nil {
		s.log.Error(err, "registry unavailable for initial snapshot; starting with local-only config and retrying in background")
		go s.retryInitialRegistryLoad(ctx)
	}

	return nil
}

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
			s.log.V(1).Info("registry still unavailable; will retry", "backoff", backoff.String(), "error", err)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		s.log.Info("registry recovered; snapshot now includes registry-derived config")
		return
	}
}
