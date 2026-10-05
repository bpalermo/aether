package server

import (
	"context"
	"fmt"
	"log/slog"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"aethermesh.dev/common/grpcserver"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/telemetry"
	"aethermesh.dev/registry"
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

	// Embed grpcserver.Server for gRPC lifecycle management.
	grpcserver.Server
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
	cfg := grpcserver.NewServerConfig()
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
		Server:      grpcserver.NewServer(cfg, log, grpcserver.WithGRPCServer(grpcSrv)),
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
	s.broadcaster.Publish(func() []*registrarv1.WatchEndpointsResponse {
		events := []*registrarv1.WatchEndpointsResponse{{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: req.GetServiceName(),
			Protocol:    req.GetProtocol(),
			Endpoint:    req.GetEndpoint(),
		}}
		version, transitions := s.snapshot.Apply(events)
		return stampVersion(append(events, transitions...), version)
	})
	s.metrics.snapshotState(ctx, s.snapshot.State())

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
			// UnregisterEndpointRequest carries no protocol, but a service is
			// registered under exactly one (HTTP or TCP). Supersede the pending
			// register on every synced protocol so a TCP endpoint's removal isn't
			// defeated by a still-pending TCP register keyed under a protocol the
			// HTTP-only key would miss. The external delete is protocol-agnostic.
			for _, protocol := range syncedProtocols {
				s.writeBehind.EnqueueUnregister(req.GetServiceName(), protocol, ip)
			}
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

	// The request names no protocol, and both the snapshot and the agents key
	// an endpoint by (service, protocol, ip): the removal is resolved against
	// the snapshot, one REMOVED event per protocol each IP is actually held
	// under (#1206). An IP the snapshot does not hold yields no event.
	s.broadcaster.Publish(func() []*registrarv1.WatchEndpointsResponse {
		events, version, transitions := s.snapshot.RemoveIPs(req.GetServiceName(), ips)
		return stampVersion(append(events, transitions...), version)
	})
	s.metrics.snapshotState(ctx, s.snapshot.State())

	return &registrarv1.UnregisterEndpointResponse{}, nil
}

// WatchEndpoints streams endpoint events to the agent. It subscribes to the
// broadcaster, then sends a full snapshot (unless the client's last_version
// names the current contents -- see Snapshot.WatchStart), then forwards the
// incremental events buffered since the subscription and every later one. A request filter scopes
// both the snapshot and the incremental events to the named services
// (demand-scoped distribution): the agent re-asserts its filter on every
// reconnect, and an unset filter preserves the full watch. A client whose
// filter grew sends partial_resume instead of last_version and, when its token
// names the current contents, receives only the added services' endpoints
// (#1239; see Snapshot.WatchStart).
func (s *RegistrarServer) WatchEndpoints(req *registrarv1.WatchEndpointsRequest, stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse]) error {
	watcherID := watcherIDOf(req)
	filterServices, filterSet := buildWatchFilter(req)
	token, have := resumeRequest(req)
	s.log.DebugContext(stream.Context(), "WatchEndpoints", "watcher", watcherID, "lastVersion", token,
		"partial", have != nil, "filtered", filterSet != nil, "filterServices", len(filterServices))

	// Never serve a snapshot before the first sync has populated it.
	if err := s.awaitSynced(stream.Context()); err != nil {
		return err
	}

	// Subscribe BEFORE reading the snapshot, and with publications excluded
	// (#1205): a batch broadcast while the initial exchange is on the wire
	// buffers on ch and follows the marker, instead of finding no subscription
	// and being lost on this stream until an unrelated event touches its
	// service. Excluding publications makes every batch either part of the
	// snapshot read here or delivered on ch, never both, so no batch whose
	// version predates the snapshot's can reach the client after the marker and
	// move its resume token back (see Broadcaster.SubscribeWith).
	//
	// The resume decision and the state it is made against are one critical
	// section: a sync landing between them must not make a current client
	// look stale, or a stale one current.
	var (
		events         []*registrarv1.WatchEndpointsResponse
		catalog        []string
		currentVersion string
		resume         Resume
		extended       bool
	)
	ch := s.broadcaster.SubscribeWith(watcherID, filterServices, func() string {
		events, catalog, currentVersion, resume, extended = s.snapshot.WatchStart(token, filterSet, have)
		return currentVersion
	})
	defer s.broadcaster.Unsubscribe(watcherID, ch)
	s.metrics.watchStarted(stream.Context(), resume, extended)
	s.log.DebugContext(stream.Context(), "WatchEndpoints resolved", "watcher", watcherID,
		"resume", resumeLabel(resume, extended), "version", currentVersion, "events", len(events))
	// A resend's FULL_SNAPSHOT events, or an extended client's missing
	// services as ENDPOINT_ADDED (#1239); nothing for a plain resume.
	if err := sendFilteredSnapshot(stream, events, filterSet); err != nil {
		return err
	}
	// Replay the full service catalog (every watcher, regardless of filter):
	// agents keep a local index of service names so the on-demand cold path
	// answers existence locally. Sent on a resend and on a rename (the client's
	// contents are current under an older name: the marker's new version makes
	// it swap catalogs, so it must receive the identical one first). Skipped
	// only when the client's token is the current version.
	if resume != ResumeCurrent {
		if err := sendServiceCatalog(stream, catalog); err != nil {
			return err
		}
	}

	// Mark the snapshot boundary so the client knows its cache is complete
	// (sent even when the snapshot was skipped: the client is current). This is
	// the ONLY event of the initial exchange that carries the version (#1203):
	// the client adopts a version as its resume token, which is safe only once
	// it holds everything the version names.
	if err := stream.Send(&registrarv1.WatchEndpointsResponse{
		Type:     registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE,
		Version:  currentVersion,
		Extended: extended,
	}); err != nil {
		return fmt.Errorf("failed to send snapshot-complete event: %w", err)
	}

	// Stream the incremental events buffered since the subscription, then
	// live ones (fan-out indexed by service), and the version-only markers the
	// syncer hands out (Broadcaster.MarkVersion, #1241).
	return streamEvents(stream, ch)
}

// stampVersion sets version on every event of a batch and returns it. The
// broadcaster keeps it only on each watcher's last event of the batch (#1203).
func stampVersion(events []*registrarv1.WatchEndpointsResponse, version string) []*registrarv1.WatchEndpointsResponse {
	for _, e := range events {
		e.Version = version
	}
	return events
}

// watcherIDOf keys a watch stream: one per cluster and node, and per agent
// instance when the agent names one. A reconnect from the same key replaces the
// previous stream; two agents on one node during a surge roll (proposal 041)
// must not, or each one's watch would close the other's with DataLoss for the
// whole overlap.
func watcherIDOf(req *registrarv1.WatchEndpointsRequest) string {
	if instance := req.GetInstance(); instance != "" {
		return fmt.Sprintf("%s/%s/%s", req.GetClusterName(), req.GetNodeName(), instance)
	}
	return fmt.Sprintf("%s/%s", req.GetClusterName(), req.GetNodeName())
}

// resumeRequest returns the request's resume token and, for a partial resume
// (#1239), the services the client holds at it (non-nil, possibly empty). A
// last_version wins: partial_resume is defined only alongside an empty one, so
// a client sending both is answered as an ordinary resume.
func resumeRequest(req *registrarv1.WatchEndpointsRequest) (string, map[string]struct{}) {
	if token := req.GetLastVersion(); token != "" {
		return token, nil
	}
	p := req.GetPartialResume()
	if p == nil || p.GetVersion() == "" {
		return "", nil
	}
	have := make(map[string]struct{}, len(p.GetServices()))
	for _, svc := range p.GetServices() {
		have[svc] = struct{}{}
	}
	return p.GetVersion(), have
}

// buildWatchFilter parses the request filter into a service list and lookup set.
// nil filterSet = full watch; non-nil (possibly empty) = scoped watch.
func buildWatchFilter(req *registrarv1.WatchEndpointsRequest) ([]string, map[string]struct{}) {
	f := req.GetFilter()
	if f == nil {
		return nil, nil
	}
	filterServices := f.GetServices()
	if filterServices == nil {
		filterServices = []string{}
	}
	filterSet := make(map[string]struct{}, len(filterServices))
	for _, svc := range filterServices {
		filterSet[svc] = struct{}{}
	}
	return filterServices, filterSet
}

// sendFilteredSnapshot sends snapshot events scoped to filterSet (nil = send all).
// WatchStart already applied the same filter; the check is kept as
// defense in depth so no out-of-scope endpoint can reach a scoped watcher.
func sendFilteredSnapshot(stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse], events []*registrarv1.WatchEndpointsResponse, filterSet map[string]struct{}) error {
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
	return nil
}

// sendServiceCatalog sends SERVICE_ADDED events for all names to the stream.
// Catalog events are sent to ALL watchers regardless of filter (see Broadcast).
// Like the FULL_SNAPSHOT events they carry no version (#1203).
func sendServiceCatalog(stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse], names []string) error {
	for _, name := range names {
		if err := stream.Send(&registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED,
			ServiceName: name,
		}); err != nil {
			return fmt.Errorf("failed to send service catalog event: %w", err)
		}
	}
	return nil
}

// streamEvents forwards incremental events from ch until the stream context ends
// or the channel is closed (overflow/reconnect).
func streamEvents(stream grpc.ServerStreamingServer[registrarv1.WatchEndpointsResponse], ch <-chan *registrarv1.WatchEndpointsResponse) error {
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case event, ok := <-ch:
			if !ok {
				// Channel closed: either the broadcaster force-resynced this
				// slow watcher after an overflow, or a reconnect replaced the
				// subscription. Either way the client must NOT resume from its
				// last seen version: an overflow drops events without regard to
				// batch boundaries, so a batch's versioned last event can reach
				// the client after an earlier event of the same batch was
				// dropped (#1203 versions only that last event, which covers a
				// stream cut, not a hole in the middle). DataLoss tells the
				// client to clear its resume token.
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
	s.log.DebugContext(ctx, "ListAllEndpoints", "protocol", req.GetProtocol().String())

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

// ListAllConfig returns the clusterset-wide config projections (proposal 026,
// multi-cluster config propagation). Agents pull this from the spoke registrar rather
// than reading the registry directly (the standing directive: agents don't talk to the
// store). When the backend has no cross-cluster config plane (kubernetes), the
// registry does not implement ConfigExporter and this returns an empty set — config
// stays cluster-local, which is the correct degenerate behaviour.
func (s *RegistrarServer) ListAllConfig(ctx context.Context, _ *registrarv1.ListAllConfigRequest) (*registrarv1.ListAllConfigResponse, error) {
	exporter, ok := s.registry.(registry.ConfigExporter)
	if !ok {
		return &registrarv1.ListAllConfigResponse{}, nil
	}
	projections, err := exporter.ListConfig(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "ListAllConfig failed", "error", err)
		return nil, status.Error(codes.Internal, "failed to list config projections")
	}
	s.log.DebugContext(ctx, "ListAllConfig", "count", len(projections))
	return &registrarv1.ListAllConfigResponse{Projections: projections}, nil
}
