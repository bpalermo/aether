package xds

import (
	"context"
	"testing"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestNewXdsServer_ReturnsInitializedServer(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "returns XdsServer with embedded Server and non-nil cache",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

			got := NewXdsServer(context.Background(), cfg, cache, nil, logr.Discard())

			require.NotNil(t, got.cache)
			assert.Equal(t, cache, got.cache)
			// The embedded Server should have a gRPC server configured by NewXdsServer.
			assert.NotNil(t, got.gSrv)
			// The xDS server wrapper should be set.
			assert.NotNil(t, got.xSrv)
		})
	}
}

func TestNewXdsServer_WithCallbacks(t *testing.T) {
	tests := []struct {
		name      string
		callbacks serverv3.Callbacks
	}{
		{
			name:      "accepts nil callbacks",
			callbacks: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

			got := NewXdsServer(context.Background(), cfg, cache, tt.callbacks, logr.Discard())

			require.NotNil(t, got.xSrv)
		})
	}
}

func TestNewXdsServer_DiscoveryServicesRegistered(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "gRPC server has Envoy discovery services registered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

			got := NewXdsServer(context.Background(), cfg, cache, nil, logr.Discard())

			grpcSrv := got.gSrv
			require.NotNil(t, grpcSrv)

			// grpc.Server.GetServiceInfo returns a map of registered service names
			// to their info.  We verify that all five xDS services are present.
			serviceInfo := grpcSrv.GetServiceInfo()

			expectedServices := []string{
				"envoy.service.listener.v3.ListenerDiscoveryService",
				"envoy.service.cluster.v3.ClusterDiscoveryService",
				"envoy.service.endpoint.v3.EndpointDiscoveryService",
				"envoy.service.route.v3.RouteDiscoveryService",
				"envoy.service.discovery.v3.AggregatedDiscoveryService",
			}

			for _, svc := range expectedServices {
				_, ok := serviceInfo[svc]
				assert.Truef(t, ok, "expected service %q to be registered", svc)
			}

			// Exactly the five xDS services should be registered (no extras).
			assert.Len(t, serviceInfo, len(expectedServices))
		})
	}
}

func TestNewXdsServer_EmbeddedServerUsesProvidedConfig(t *testing.T) {
	tests := []struct {
		name    string
		network string
		address string
	}{
		{
			name:    "tcp config is passed through to embedded Server",
			network: "tcp",
			address: ":18000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			cfg.Network = tt.network
			cfg.Address = tt.address

			cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
			got := NewXdsServer(context.Background(), cfg, cache, nil, logr.Discard())

			assert.Equal(t, tt.network, got.cfg.Network)
			assert.Equal(t, tt.address, got.cfg.Address)
		})
	}
}

func TestNewXdsServer_GRPCServerIsDistinctFromBareGRPCServer(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "NewXdsServer creates its own gRPC server independent of grpc.NewServer()",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

			bare := grpc.NewServer()
			defer bare.Stop()

			got := NewXdsServer(context.Background(), cfg, cache, nil, logr.Discard())
			defer got.gSrv.Stop()

			// The gRPC server created by NewXdsServer must not be the bare one.
			assert.NotEqual(t, bare, got.gSrv)
		})
	}
}
