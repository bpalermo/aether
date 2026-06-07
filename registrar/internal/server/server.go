package server

import (
	"context"
	"fmt"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
)

// RegistrarServer implements the RegistrarServiceServer gRPC interface.
// It delegates writes to an external registry and serves reads from a local
// snapshot. Change notifications are pushed to agents via the broadcaster.
type RegistrarServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	registry    registry.Registry
	snapshot    *Snapshot
	broadcaster *Broadcaster
	log         logr.Logger

	// Embed xds.Server for gRPC lifecycle management.
	xds.Server
}

// NewRegistrarServer creates a RegistrarServer and registers it on the gRPC server.
// Additional gRPC server options (e.g., TLS credentials) can be passed via grpcOpts.
func NewRegistrarServer(
	reg registry.Registry,
	snapshot *Snapshot,
	broadcaster *Broadcaster,
	address string,
	log logr.Logger,
	grpcOpts ...grpc.ServerOption,
) *RegistrarServer {
	cfg := xds.NewServerConfig()
	cfg.Network = "tcp"
	cfg.Address = address

	grpcSrv := grpc.NewServer(grpcOpts...)

	srv := &RegistrarServer{
		registry:    reg,
		snapshot:    snapshot,
		broadcaster: broadcaster,
		log:         log.WithName("registrar-server"),
		Server:      xds.NewServer(cfg, log, xds.WithGRPCServer(grpcSrv)),
	}

	registrarv1.RegisterRegistrarServiceServer(grpcSrv, srv)

	return srv
}

// RegisterEndpoint delegates to the external registry and broadcasts the change.
func (s *RegistrarServer) RegisterEndpoint(ctx context.Context, req *registrarv1.RegisterEndpointRequest) (*registrarv1.RegisterEndpointResponse, error) {
	s.log.V(1).Info("RegisterEndpoint", "service", req.GetServiceName(), "ip", req.GetEndpoint().GetIp())

	if err := s.registry.RegisterEndpoint(ctx, req.GetServiceName(), req.GetProtocol(), req.GetEndpoint()); err != nil {
		return nil, fmt.Errorf("failed to register endpoint: %w", err)
	}

	// Optimistic broadcast: notify watchers immediately.
	events := []*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: req.GetServiceName(),
		Protocol:    req.GetProtocol(),
		Endpoint:    req.GetEndpoint(),
	}}
	version := s.snapshot.Apply(events)
	for _, e := range events {
		e.Version = version
	}
	s.broadcaster.Broadcast(events)

	return &registrarv1.RegisterEndpointResponse{}, nil
}

// UnregisterEndpoint delegates to the external registry and broadcasts the change.
func (s *RegistrarServer) UnregisterEndpoint(ctx context.Context, req *registrarv1.UnregisterEndpointRequest) (*registrarv1.UnregisterEndpointResponse, error) {
	s.log.V(1).Info("UnregisterEndpoint", "service", req.GetServiceName(), "ips", req.GetIps())

	ips := req.GetIps()
	if len(ips) == 1 {
		if err := s.registry.UnregisterEndpoint(ctx, req.GetServiceName(), ips[0]); err != nil {
			return nil, fmt.Errorf("failed to unregister endpoint: %w", err)
		}
	} else if len(ips) > 1 {
		if err := s.registry.UnregisterEndpoints(ctx, req.GetServiceName(), ips); err != nil {
			return nil, fmt.Errorf("failed to unregister endpoints: %w", err)
		}
	}

	// Build removal events.
	events := make([]*registrarv1.WatchEndpointsResponse, 0, len(ips))
	for _, ip := range ips {
		events = append(events, &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED,
			ServiceName: req.GetServiceName(),
			Endpoint:    &registryv1.ServiceEndpoint{Ip: ip},
		})
	}
	version := s.snapshot.Apply(events)
	for _, e := range events {
		e.Version = version
	}
	s.broadcaster.Broadcast(events)

	return &registrarv1.UnregisterEndpointResponse{}, nil
}

// WatchEndpoints streams endpoint events to the agent. It first sends a full
// snapshot (unless the client's last_version matches the current version), then
// forwards incremental events from the broadcaster.
func (s *RegistrarServer) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	watcherID := fmt.Sprintf("%s/%s", req.GetClusterName(), req.GetNodeName())
	s.log.V(1).Info("WatchEndpoints", "watcher", watcherID, "lastVersion", req.GetLastVersion())

	// Send current snapshot unless the client is already up-to-date.
	events, currentVersion := s.snapshot.FullSnapshotEvents()
	if req.GetLastVersion() != currentVersion {
		for _, event := range events {
			if err := stream.Send(event); err != nil {
				return fmt.Errorf("failed to send snapshot event: %w", err)
			}
		}
	}

	// Subscribe and stream incremental events.
	ch := s.broadcaster.Subscribe(watcherID)
	defer s.broadcaster.Unsubscribe(watcherID, ch)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(event); err != nil {
				return fmt.Errorf("failed to send event: %w", err)
			}
		}
	}
}

// ListAllEndpoints returns all endpoints from the local snapshot.
func (s *RegistrarServer) ListAllEndpoints(_ context.Context, req *registrarv1.ListAllEndpointsRequest) (*registrarv1.ListAllEndpointsResponse, error) {
	s.log.V(1).Info("ListAllEndpoints", "protocol", req.GetProtocol())

	endpoints, version := s.snapshot.GetAllWithVersion(req.GetProtocol())

	services := make(map[string]*registrarv1.ServiceEndpoints, len(endpoints))
	for svcName, eps := range endpoints {
		services[svcName] = &registrarv1.ServiceEndpoints{Endpoints: eps}
	}

	return &registrarv1.ListAllEndpointsResponse{
		Services: services,
		Version:  version,
	}, nil
}
