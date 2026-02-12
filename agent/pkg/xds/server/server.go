package server

import (
	"context"
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/storage"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/xds"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"go.uber.org/atomic"
)

type AgentXdsServer struct {
	xds.XdsServer

	nodeID string

	storage storage.Storage[*registryv1.RegistryPod]

	cache cachev3.SnapshotCache

	version *atomic.Uint64
}

func NewXdsServer(ctx context.Context, nodeID string, storage storage.Storage[*registryv1.RegistryPod], log logr.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

	aXdsServer := &AgentXdsServer{
		XdsServer: xds.NewXdsServer(ctx, cfg, cache, nil, log),
		nodeID:    nodeID,
		storage:   storage,
		cache:     cache,
		version:   atomic.NewUint64(0),
	}

	aXdsServer.AddCallback(aXdsServer)

	return aXdsServer, nil
}

func (s *AgentXdsServer) PreListen(ctx context.Context) error {
	s.Log().V(1).Info("generating initial snapshot")

	v := s.generateSnapshotVersion()
	snapshot, err := cachev3.NewSnapshot(v, map[resource.Type][]types.Resource{
		resource.ListenerType:    make([]types.Resource, 0),
		resource.EndpointType:    make([]types.Resource, 0),
		resource.ClusterType:     make([]types.Resource, 0),
		resource.RouteType:       make([]types.Resource, 0),
		resource.VirtualHostType: make([]types.Resource, 0),
	})
	if err != nil {
		return err
	}

	s.Log().V(1).Info("setting snapshot", "node", s.nodeID, "version", v)
	if err = s.cache.SetSnapshot(ctx, s.nodeID, snapshot); err != nil {
		return err
	}

	return nil
}

func (s *AgentXdsServer) generateSnapshotVersion() string {
	timestamp := time.Now().UnixMilli()
	version := s.version.Add(1)
	return fmt.Sprintf("%d.%d", timestamp, version)
}
