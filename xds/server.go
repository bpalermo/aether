package xds

import (
	"context"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
)

type Server struct {
	gSrv *grpc.Server
	xSrv serverv3.Server

	cache cachev3.SnapshotCache
}

func NewServer(ctx context.Context, cache cachev3.SnapshotCache, callbacks serverv3.Callbacks) *Server {
	grpcServer := grpc.NewServer()
	xdsSrv := serverv3.NewServer(ctx, cache, callbacks)

	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsSrv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsSrv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsSrv)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsSrv)

	return &Server{grpcServer, xdsSrv, cache}
}

func (s *Server) GetGrpcServer() *grpc.Server {
	return s.gSrv
}
