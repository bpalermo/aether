package server

import (
	"context"
	"fmt"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	metrics     *Metrics

	// synced, when set (GateOnSync), gates snapshot-serving RPCs until the
	// syncer's first cycle completes: a freshly restarted registrar must not
	// serve an empty/partial world view — agents derive route configs from it
	// and 404 live traffic (rev-66 co-roll, 2026-06-11).
	synced <-chan struct{}

	// Embed xds.Server for gRPC lifecycle management.
	xds.Server
}

// GateOnSync makes snapshot-serving RPCs (WatchEndpoints, ListAllEndpoints)
// block until ch is closed (the syncer's first completed cycle). Nil leaves
// the RPCs ungated.
func (s *RegistrarServer) GateOnSync(ch <-chan struct{}) { s.synced = ch }

// awaitSynced blocks until the sync gate opens or ctx ends.
func (s *RegistrarServer) awaitSynced(ctx context.Context) error {
	if s.synced == nil {
		return nil
	}
	select {
	case <-s.synced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewRegistrarServer creates a RegistrarServer and registers it on the gRPC server.
// Additional gRPC server options (e.g., TLS credentials) can be passed via grpcOpts.
// metrics may be nil to disable instrumentation.
func NewRegistrarServer(
	reg registry.Registry,
	snapshot *Snapshot,
	broadcaster *Broadcaster,
	address string,
	log logr.Logger,
	metrics *Metrics,
	grpcOpts ...grpc.ServerOption,
) *RegistrarServer {
	cfg := xds.NewServerConfig()
	cfg.Network = "tcp"
	cfg.Address = address

	// No-op until OTel providers are registered (--otel-enabled / --tracing-enabled).
	grpcOpts = append(grpcOpts, grpc.StatsHandler(telemetry.ServerStatsHandler()))
	grpcSrv := grpc.NewServer(grpcOpts...)

	srv := &RegistrarServer{
		registry:    reg,
		snapshot:    snapshot,
		broadcaster: broadcaster,
		log:         log.WithName("registrar-server"),
		metrics:     metrics,
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
	s.metrics.versionAdvanced(ctx, version)
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
	s.metrics.versionAdvanced(ctx, version)
	s.broadcaster.Broadcast(events)

	return &registrarv1.UnregisterEndpointResponse{}, nil
}

// WatchEndpoints streams endpoint events to the agent. It first sends a full
// snapshot (unless the client's last_version matches the current version), then
// forwards incremental events from the broadcaster.
func (s *RegistrarServer) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	watcherID := fmt.Sprintf("%s/%s", req.GetClusterName(), req.GetNodeName())
	s.log.V(1).Info("WatchEndpoints", "watcher", watcherID, "lastVersion", req.GetLastVersion())

	// Never serve a snapshot before the first sync has populated it.
	if err := s.awaitSynced(stream.Context()); err != nil {
		return err
	}

	// Send current snapshot unless the client is already up-to-date.
	events, currentVersion := s.snapshot.FullSnapshotEvents()
	if req.GetLastVersion() != currentVersion {
		for _, event := range events {
			if err := stream.Send(event); err != nil {
				return fmt.Errorf("failed to send snapshot event: %w", err)
			}
		}
	}
	// Mark the snapshot boundary so the client knows its cache is complete
	// (sent even when the snapshot was skipped: the client is current).
	if err := stream.Send(&registrarv1.WatchEndpointsResponse{
		Type:    registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
		Version: currentVersion,
	}); err != nil {
		return fmt.Errorf("failed to send snapshot-complete event: %w", err)
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
				// Channel closed: either the broadcaster force-resynced this
				// slow watcher after an overflow, or a reconnect replaced the
				// subscription. Either way the client must NOT resume from its
				// last seen version — events within a batch share one version,
				// so a mid-batch overflow can leave the client's lastVersion
				// equal to the current version while it still missed events.
				// DataLoss tells the client to clear its resume token.
				return status.Error(codes.DataLoss, "watch stream overflowed; reconnect for a full snapshot")
			}
			if err := stream.Send(event); err != nil {
				return fmt.Errorf("failed to send event: %w", err)
			}
		}
	}
}

// ListAllEndpoints returns all endpoints from the local snapshot.
func (s *RegistrarServer) ListAllEndpoints(ctx context.Context, req *registrarv1.ListAllEndpointsRequest) (*registrarv1.ListAllEndpointsResponse, error) {
	s.log.V(1).Info("ListAllEndpoints", "protocol", req.GetProtocol())

	// Never serve a snapshot before the first sync has populated it (agents
	// fall back to this RPC at startup when their watch cache is empty).
	if err := s.awaitSynced(ctx); err != nil {
		return nil, err
	}

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
