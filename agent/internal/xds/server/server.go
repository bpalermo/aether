// Package server implements the agent-specific Envoy xDS server.
// It builds xDS snapshots from local pod storage and the service registry,
// generating Envoy listeners, clusters, endpoints, and routes.
package server

import (
	"context"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/registry"
	"github.com/bpalermo/aether/xds"
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

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	cache *cache.SnapshotCache
}

// NewAgentXdsServer creates a new AgentXdsServer.
// It initializes an xDS server with a snapshot cache and registers itself as a callback
// to generate the initial Envoy snapshot before listening for client connections.
// The server listens on a Unix domain socket at the default xDS socket path.
func NewAgentXdsServer(ctx context.Context, clusterName string, nodeName string, registry registry.Registry, storage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache, log logr.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	aXdsServer := &AgentXdsServer{
		XdsServer:   xds.NewXdsServer(ctx, cfg, snapshotCache, nil, log),
		log:         log.WithName("agent-xds"),
		clusterName: clusterName,
		nodeName:    nodeName,
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

	if err := s.cache.LoadListenersFromStorage(ctx, s.storage); err != nil {
		s.log.Error(err, "failed to load listeners from storage")
		return err
	}

	if err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry); err != nil {
		s.log.Error(err, "failed to load clusters, endpoints and virtual hosts from registry")
		return err
	}

	return nil
}
