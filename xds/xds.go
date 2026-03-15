package xds

import (
	"context"
	"time"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// XdsServer is a gRPC server that implements Envoy's xDS discovery services.
// It embeds Server and registers discovery service handlers for LDS, CDS, EDS, RDS,
// and ADS with Envoy's go-control-plane server. The server uses a snapshot cache
// to manage versioned Envoy configurations.
//
// XdsServer is configured with appropriate keepalive parameters for long-lived client
// connections and supports up to 1000 concurrent gRPC streams.
type XdsServer struct {
	Server

	xSrv serverv3.Server

	cache cachev3.SnapshotCache
}

// NewXdsServer creates a new XdsServer with Envoy discovery services registered.
// It configures the gRPC server with keepalive parameters suitable for long-lived
// client connections, registers all Envoy discovery services (LDS, CDS, EDS, RDS, ADS),
// and returns an XdsServer ready to be started.
func NewXdsServer(ctx context.Context, cfg *ServerConfig, cache cachev3.SnapshotCache, callbacks serverv3.Callbacks, log logr.Logger) XdsServer {
	keepAliveTime := 30 * time.Second
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     15 * time.Minute, // Close idle connections after 15 minutes
			MaxConnectionAge:      1 * time.Hour,    // Max age of connection
			MaxConnectionAgeGrace: 10 * time.Second, // Allow 5 seconds for pending RPCs to complete
			Time:                  keepAliveTime,    // Ping client if no activity for 30 seconds
			Timeout:               5 * time.Second,  // Wait 10 seconds for ping ack before closing
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// Ensure we allow clients has enough time to send keep alive. If this is higher than the client's
			// keep alive setting, it will prematurely get a GOAWAY sent.
			MinTime:             keepAliveTime / 2, // Minimum time between client pings
			PermitWithoutStream: true,              // Allow pings even without active streams
		}),
		grpc.MaxConcurrentStreams(1000),
	)
	xdsSrv := serverv3.NewServer(ctx, cache, callbacks)

	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsSrv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsSrv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsSrv)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsSrv)

	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, xdsSrv)
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, xdsSrv)

	return XdsServer{
		Server: NewServer(cfg, log, WithGRPCServer(grpcServer)),
		xSrv:   xdsSrv,
		cache:  cache,
	}
}
