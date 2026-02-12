package xds

import (
	"context"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
)

type XdsServer struct {
	Server

	xSrv serverv3.Server

	cache cachev3.SnapshotCache
}

func NewXdsServer(ctx context.Context, cfg *ServerConfig, cache cachev3.SnapshotCache, callbacks serverv3.Callbacks, log logr.Logger) XdsServer {
	grpcServer := grpc.NewServer()
	xdsSrv := serverv3.NewServer(ctx, cache, callbacks)

	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsSrv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsSrv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsSrv)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsSrv)

	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, xdsSrv)

	return XdsServer{
		Server: NewServer(cfg, log, WithGRPCServer(grpcServer)),
		xSrv:   xdsSrv,
		cache:  cache,
	}
}
