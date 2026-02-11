package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bpalermo/aether/registrar/internal/server/cache"
	registryTypes "github.com/bpalermo/aether/registry/types"
	"github.com/bpalermo/aether/xds"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"go.uber.org/atomic"
	"google.golang.org/grpc"
)

type RegistrarServer struct {
	xds *xds.Server

	cfg *RegistrarServerConfig

	log logr.Logger

	registry registryTypes.Registry

	liveness  *atomic.Bool
	readiness *atomic.Bool

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

	return &RegistrarServer{
		xds:             xds.NewServer(ctx, fallbackCache, nil),
		cfg:             cfg,
		log:             log.WithName("server"),
		registry:        reg,
		currentSnapshot: nil,
		snapshotMU:      sync.RWMutex{},
		liveness:        atomic.NewBool(false),
		readiness:       atomic.NewBool(false),
	}, nil
}

func (rs *RegistrarServer) Start(ctx context.Context) error {
	rs.log.V(1).Info("starting registrar server", "network", rs.cfg.Network, "address", rs.cfg.Address)

	listener, err := net.Listen(rs.cfg.Network, rs.cfg.Address)
	if err != nil {
		rs.log.Error(err, "failed to listen", "network", rs.cfg.Network, "address", rs.cfg.Address)
		return fmt.Errorf("failed to listen: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		rs.log.V(1).Info("starting gRPC server")
		if serveErr := rs.xds.GetGrpcServer().Serve(listener); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	rs.liveness.Store(true)
	// TODO: fix
	rs.readiness.Store(true)

	select {
	case <-ctx.Done():
		rs.log.V(1).Info("context cancelled, stopping registrar server")
		return rs.shutdown()
	case serveErr := <-errCh:
		return serveErr

	}
}

func (rs *RegistrarServer) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), rs.cfg.ShutdownTimeout)
	defer cancel()

	rs.readiness.Store(false)

	if rs.xds.GetGrpcServer() == nil {
		return nil
	}

	stopped := make(chan struct{})
	go func() {
		rs.xds.GetGrpcServer().GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		rs.log.V(1).Info("gRPC server graceful stop completed")
		return nil
	case <-ctx.Done():
		rs.log.V(1).Info("gRPC server forced stop due to timeout")
		rs.xds.GetGrpcServer().Stop()
		return ctx.Err()
	}
}

func generateSnapshotVersion() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
