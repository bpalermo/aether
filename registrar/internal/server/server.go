package server

import (
	"context"
	"sync"

	"github.com/bpalermo/aether/registrar/internal/server/cache"
	registryTypes "github.com/bpalermo/aether/registry/types"
	"github.com/bpalermo/aether/xds"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
)

type RegistrarServer struct {
	xds.Server

	cfg *RegistrarServerConfig

	log logr.Logger

	registry registryTypes.Registry

	currentSnapshot *cachev3.Snapshot
	snapshotMU      sync.RWMutex
}

func NewRegistrarServer(ctx context.Context, cfg *RegistrarServerConfig, reg registryTypes.Registry, log logr.Logger) (*RegistrarServer, error) {
	// we want ADS to be explicitly false because we won't be including listeners.
	// listeners are the responsibility of the agent on each node.
	fallbackCache := cache.NewFallbackSnapshotCache(false, nil, "*")

	// Set the default snapshot for all unknown nodes
	defaultSnapshot, _ := cachev3.NewSnapshot("1", map[resource.Type][]types.Resource{
		resource.ListenerType: make([]types.Resource, 0),
		resource.ClusterType:  make([]types.Resource, 0),
		resource.EndpointType: make([]types.Resource, 0),
		resource.RouteType:    make([]types.Resource, 0),
	})
	err := fallbackCache.SetSnapshot(ctx, "*", defaultSnapshot)
	if err != nil {
		return nil, err
	}

	l := log.WithName("server")

	return &RegistrarServer{
		Server:          xds.NewServer(ctx, cfg.SrvCfg, fallbackCache, nil, l),
		cfg:             cfg,
		log:             l,
		registry:        reg,
		currentSnapshot: nil,
		snapshotMU:      sync.RWMutex{},
	}, nil
}
