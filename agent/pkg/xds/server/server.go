package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/xds/snapshot"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type envoyCluster struct {
	cluster   *clusterv3.Cluster
	endpoints map[string]*endpointv3.LbEndpoint
}

type XdsServer struct {
	log logr.Logger

	address string

	grpcServer *grpc.Server

	mgr manager.Manager

	initWg *sync.WaitGroup

	xdsSnapshot *snapshot.XdsSnapshot
}

type XdsServerOption func(*XdsServer)

func NewXdsServer(ctx context.Context, mgr manager.Manager, log logr.Logger, initWg *sync.WaitGroup, xdsSnapshot *snapshot.XdsSnapshot, opts ...XdsServerOption) *XdsServer {
	srv := &XdsServer{
		mgr:         mgr,
		log:         log.WithName("xds-server"),
		address:     constants.DefaultXdsSocketPath,
		initWg:      initWg,
		xdsSnapshot: xdsSnapshot,
	}

	for _, option := range opts {
		option(srv)
	}

	server := serverv3.NewServer(ctx, srv.xdsSnapshot.GetSnapshot(), nil)
	srv.grpcServer = grpc.NewServer()

	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(srv.grpcServer, server)
	endpointservice.RegisterEndpointDiscoveryServiceServer(srv.grpcServer, server)
	clusterservice.RegisterClusterDiscoveryServiceServer(srv.grpcServer, server)
	routeservice.RegisterRouteDiscoveryServiceServer(srv.grpcServer, server)
	listenerservice.RegisterListenerDiscoveryServiceServer(srv.grpcServer, server)

	return srv
}
func (s *XdsServer) Start(ctx context.Context) error {
	// Create listener
	listener, err := net.Listen("unix", s.address)
	if err != nil {
		return err
	}

	if err := os.Chmod(s.address, os.ModePerm); err != nil {
		// Close listener before returning error
		if err = listener.Close(); err != nil {
			s.log.Error(err, "failed to close listener")
		}
		return fmt.Errorf("failed to set socket file permissions: %v", err)
	}

	s.log.Info("XDS server uds", "address", s.address)

	// Monitor context cancellation
	go func() {
		<-ctx.Done()
		s.log.Info("context cancelled, initiating graceful shutdown")
		if err = listener.Close(); err != nil {
			s.log.Error(err, "failed to close listener")
		}
		s.grpcServer.GracefulStop()
	}()

	// Wait for registry and controller to be ready
	waitChan := make(chan struct{})
	go func() {
		s.initWg.Wait()
		close(waitChan)
	}()

	select {
	case <-waitChan:
		s.log.Info("All components initialized")
	case <-ctx.Done():
		return ctx.Err()
	}

	s.log.Info("starting XDS server")
	return s.grpcServer.Serve(listener)
}
