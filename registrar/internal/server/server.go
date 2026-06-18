package server

import (
	"context"
	"fmt"
	"log/slog"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
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
	log         *slog.Logger
	metrics     *Metrics

	// writeBehind, when set (UseWriteBehind), makes RegisterEndpoint /
	// UnregisterEndpoint snapshot-first: apply + broadcast immediately, queue
	// the external-registry write. Nil = legacy write-through (tests).
	writeBehind *WriteBehindQueue

	// synced, when set (GateOnSync), gates snapshot-serving RPCs until the
	// syncer's first cycle completes: a freshly restarted registrar must not
	// serve an empty/partial world view — agents derive route configs from it
	// and 404 live traffic (rev-66 co-roll, 2026-06-11).
	synced <-chan struct{}

	// Embed xds.Server for gRPC lifecycle management.
	xds.Server
}

// UseWriteBehind enables snapshot-first registry mutations through q.
func (s *RegistrarServer) UseWriteBehind(q *WriteBehindQueue) { s.writeBehind = q }

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
	log *slog.Logger,
	metrics *Metrics,
	grpcOpts ...grpc.ServerOption,
) *RegistrarServer {
	cfg := xds.NewServerConfig()
	cfg.Network = "tcp"
	cfg.Address = address

	// The TracerProvider is always installed, so this records real RPC spans (whose
	// trace_id flows to logs); spans export only with --trace-export.
	grpcOpts = append(grpcOpts, grpc.StatsHandler(telemetry.ServerStatsHandler()))
	grpcSrv := grpc.NewServer(grpcOpts...)

	srv := &RegistrarServer{
		registry:    reg,
		snapshot:    snapshot,
		broadcaster: broadcaster,
		log:         commonlog.Named(log, "registrar-server"),
		metrics:     metrics,
		Server:      xds.NewServer(cfg, log, xds.WithGRPCServer(grpcSrv)),
	}

	registrarv1.RegisterRegistrarServiceServer(grpcSrv, srv)

	return srv
}

// RegisterEndpoint applies the change to the snapshot and broadcasts it
// immediately (discovery moves at watch latency), then writes the external
// registry — through the write-behind queue when enabled, so a failing
// external write can never make a serving pod invisible to the mesh (rev-68
// roll regression). Without a queue (tests/legacy) the write is synchronous
// and failures fail the RPC.
func (s *RegistrarServer) RegisterEndpoint(ctx context.Context, req *registrarv1.RegisterEndpointRequest) (*registrarv1.RegisterEndpointResponse, error) {
	s.log.DebugContext(ctx, "RegisterEndpoint", "service", req.GetServiceName(), "ip", req.GetEndpoint().GetIp())

	if s.writeBehind != nil {
		s.writeBehind.EnqueueRegister(req.GetServiceName(), req.GetProtocol(), req.GetEndpoint())
	} else if err := s.registry.RegisterEndpoint(ctx, req.GetServiceName(), req.GetProtocol(), req.GetEndpoint()); err != nil {
		return nil, fmt.Errorf("failed to register endpoint: %w", err)
	}

	// Snapshot-first broadcast: watchers see the endpoint immediately.
	events := []*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: req.GetServiceName(),
		Protocol:    req.GetProtocol(),
		Endpoint:    req.GetEndpoint(),
	}}
	version, transitions := s.snapshot.Apply(events)
	events = append(events, transitions...)
	for _, e := range events {
		e.Version = version
	}
	s.metrics.versionAdvanced(ctx, version)
	s.broadcaster.Broadcast(events)

	return &registrarv1.RegisterEndpointResponse{}, nil
}

// UnregisterEndpoint applies the removal to the snapshot and broadcasts it
// immediately, then writes the external registry (write-behind when enabled;
// see RegisterEndpoint).
func (s *RegistrarServer) UnregisterEndpoint(ctx context.Context, req *registrarv1.UnregisterEndpointRequest) (*registrarv1.UnregisterEndpointResponse, error) {
	s.log.DebugContext(ctx, "UnregisterEndpoint", "service", req.GetServiceName(), "ips", req.GetIps())

	ips := req.GetIps()
	if s.writeBehind != nil {
		for _, ip := range ips {
			// UnregisterEndpointRequest carries no protocol; the registrar is
			// HTTP-only end-to-end today (the sync loop lists PROTOCOL_HTTP),
			// and the key must match the register side's for supersede.
			s.writeBehind.EnqueueUnregister(req.GetServiceName(), registryv1.Service_PROTOCOL_HTTP, ip)
		}
	} else if len(ips) == 1 {
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
	version, transitions := s.snapshot.Apply(events)
	events = append(events, transitions...)
	for _, e := range events {
		e.Version = version
	}
	s.metrics.versionAdvanced(ctx, version)
	s.broadcaster.Broadcast(events)

	return &registrarv1.UnregisterEndpointResponse{}, nil
}

// WatchEndpoints streams endpoint events to the agent. It first sends a full
// snapshot (unless the client's last_version matches the current version), then
// forwards incremental events from the broadcaster. A request filter scopes
// both the snapshot and the incremental events to the named services
// (demand-scoped distribution): the agent re-asserts its filter on every
// reconnect, and an unset filter preserves the full watch.
func (s *RegistrarServer) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	watcherID := fmt.Sprintf("%s/%s", req.GetClusterName(), req.GetNodeName())

	// nil = full watch; non-nil (possibly empty) = scoped to these services.
	var filterServices []string
	var filterSet map[string]struct{}
	if f := req.GetFilter(); f != nil {
		filterServices = f.GetServices()
		if filterServices == nil {
			filterServices = []string{}
		}
		filterSet = make(map[string]struct{}, len(filterServices))
		for _, svc := range filterServices {
			filterSet[svc] = struct{}{}
		}
	}
	s.log.DebugContext(stream.Context(), "WatchEndpoints", "watcher", watcherID, "lastVersion", req.GetLastVersion(),
		"filtered", filterSet != nil, "filterServices", len(filterServices))

	// Never serve a snapshot before the first sync has populated it.
	if err := s.awaitSynced(stream.Context()); err != nil {
		return err
	}

	// Send current snapshot (scoped to the filter) unless the client is
	// already up-to-date.
	events, currentVersion := s.snapshot.FullSnapshotEvents()
	if req.GetLastVersion() != currentVersion {
		for _, event := range events {
			if filterSet != nil {
				if _, inScope := filterSet[event.GetServiceName()]; !inScope {
					continue
				}
			}
			if err := stream.Send(event); err != nil {
				return fmt.Errorf("failed to send snapshot event: %w", err)
			}
		}
	}
	// Replay the full service catalog (every watcher, regardless of filter):
	// agents keep a local index of service names so the on-demand cold path
	// answers existence locally. Skipped when the client is current (its
	// catalog is too: transitions ride the same versioned stream).
	if req.GetLastVersion() != currentVersion {
		for _, name := range s.snapshot.ServiceNames() {
			if err := stream.Send(&registrarv1.WatchEndpointsResponse{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED,
				ServiceName: name,
				Version:     currentVersion,
			}); err != nil {
				return fmt.Errorf("failed to send service catalog event: %w", err)
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

	// Subscribe and stream incremental events (fan-out indexed by service).
	ch := s.broadcaster.Subscribe(watcherID, filterServices)
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
	s.log.DebugContext(ctx, "ListAllEndpoints", "protocol", req.GetProtocol())

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
