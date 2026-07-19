// Package registrar implements the Registry interface using a Registrar gRPC service.
// It caches endpoints locally from a server-streaming watch and delegates writes
// to the Registrar, which in turn persists them to the external registry backend.
package registrar

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/serviceref"
	"github.com/bpalermo/aether/common/telemetry"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	// initialBackoff is the starting backoff duration for reconnection.
	initialBackoff = 1 * time.Second
	// maxBackoff is the maximum backoff duration for reconnection.
	maxBackoff = 30 * time.Second
	// jitterFraction is the fraction of backoff to randomize (0.0–1.0).
	jitterFraction = 0.2
)

// Config holds configuration for connecting to the Registrar service.
type Config struct {
	// Address is the gRPC address of the Registrar service.
	Address string
	// ClusterName identifies this agent's cluster to the Registrar. Together with
	// NodeName it forms the unique watcher id; without it every agent collides on
	// the same id and the Registrar evicts their watch streams in a reconnect loop.
	ClusterName string
	// NodeName identifies this agent's node to the Registrar. See ClusterName.
	NodeName string
	// DialOptions are additional gRPC dial options (e.g., TLS credentials).
	// When empty, insecure credentials are used.
	DialOptions []grpc.DialOption
}

// RegistrarRegistry implements the Registry interface by communicating with a
// Registrar gRPC service. Reads are served from a local cache populated by a
// WatchEndpoints stream. Writes are delegated to the Registrar.
type RegistrarRegistry struct {
	log     *slog.Logger
	config  Config
	metrics *clientMetrics

	conn   *grpc.ClientConn
	client registrarv1.RegistrarServiceClient

	mu sync.RWMutex
	// cache is the watch-fed endpoint store, partitioned by protocol so a
	// ListAllEndpoints(protocol) returns only that protocol's services (HTTP
	// services ride the HCM path, TCP services the transparent-capture floor).
	// A service is registered under exactly one protocol, so the two partitions
	// never hold the same service name. The watch stream carries the protocol on
	// every endpoint event.
	cache map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
	// services is the full mesh service-name catalog (every watcher receives
	// catalog events regardless of filter): the ODCDS cold path answers
	// existence locally instead of stalling on nonexistent services. Replayed
	// on every reconnect; swapped atomically at SNAPSHOT_COMPLETE.
	services map[string]struct{}

	// notify coalesces endpoint-change signals for consumers (e.g. the agent
	// xDS cache). It is buffered with capacity 1 and written non-blocking, so a
	// burst of watch events collapses into a single pending signal.
	notify chan struct{}

	// reconnected signals each successful watch (re)connection (coalesced,
	// non-blocking). The agent re-asserts its local registrations on it: a
	// reconnect may mean a fresh/failed-over registrar replica whose
	// write-behind queue (and therefore snapshot) lost in-flight intents —
	// re-assertion makes that state loss self-healing at reconnect speed
	// instead of the 60s ghost sweep.
	reconnected chan struct{}

	// ready is closed when the first SNAPSHOT_COMPLETE event arrives — the
	// local cache then holds a complete world view. Consumers deriving config
	// from the cache (the agent's initial snapshot) wait on it so they never
	// publish from an empty/partial cache (rev-66 404 gap).
	ready     chan struct{}
	readyOnce sync.Once

	// filterMu guards the watch service filter. filterServices nil = full
	// watch; non-nil = scope the watch to these services (demand-scoped
	// distribution). filterGen increments on every filter change; the watch
	// loop compares it against the generation its stream was opened with and
	// clears the resume token when they differ, forcing a full (re-filtered)
	// snapshot — resuming by version would skip the newly in-scope services.
	filterMu       sync.Mutex
	filterServices []string
	filterGen      uint64
	// streamCancel ends the in-flight watch stream so the loop reconnects
	// with the current filter.
	streamCancel context.CancelFunc

	cancel context.CancelFunc
}

// NewRegistrarRegistry creates a new RegistrarRegistry.
func NewRegistrarRegistry(log *slog.Logger, cfg Config) *RegistrarRegistry {
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the client.
	metrics, err := newClientMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error("failed to create registrar client metrics; continuing without instrumentation", "error", err)
	}

	return &RegistrarRegistry{
		log:         commonlog.Named(log, "registrar-registry"),
		config:      cfg,
		metrics:     metrics,
		cache:       make(map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint),
		services:    make(map[string]struct{}),
		notify:      make(chan struct{}, 1),
		ready:       make(chan struct{}),
		reconnected: make(chan struct{}, 1),
	}
}

// Changes returns a channel that receives a signal whenever the cached set of
// endpoints changes (an endpoint is added, updated, or removed). Signals are
// coalesced: consumers should treat each receive as "something changed, re-read
// the registry" rather than a per-event notification. It satisfies the
// registry.ChangeNotifier capability.
func (r *RegistrarRegistry) Changes() <-chan struct{} {
	return r.notify
}

// Reconnects returns a channel receiving a (coalesced) signal after each
// successful watch stream (re)connection. It satisfies the
// registry.ReconnectNotifier capability.
func (r *RegistrarRegistry) Reconnects() <-chan struct{} {
	return r.reconnected
}

// signalReconnect performs a non-blocking, coalescing send on reconnected.
func (r *RegistrarRegistry) signalReconnect() {
	select {
	case r.reconnected <- struct{}{}:
	default:
	}
}

// HasService reports whether the named service currently has at least one
// endpoint anywhere in the mesh, answered from the local catalog. It
// satisfies the registry.ServiceCatalog capability.
func (r *RegistrarRegistry) HasService(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.services[name]
	return ok
}

// WaitReady blocks until the watch cache holds a complete snapshot (the first
// SNAPSHOT_COMPLETE event) or ctx ends. It satisfies the registry.ReadyWaiter
// capability; callers bound it with a context timeout and may proceed with
// degraded (RPC-fallback) reads on expiry.
func (r *RegistrarRegistry) WaitReady(ctx context.Context) error {
	select {
	case <-r.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetServiceFilter scopes the endpoint watch to the given services (the
// node's dependency set). nil restores the full watch; an empty non-nil set
// watches nothing. If the effective filter changed while a stream is active,
// the stream is cancelled so the loop reconnects re-asserting the new filter
// with a cleared resume token (the registrar must resend the snapshot — the
// new scope may include services the old stream never delivered). It
// satisfies the registry.WatchScoper capability.
func (r *RegistrarRegistry) SetServiceFilter(services []string) {
	r.filterMu.Lock()
	if stringSetsEqual(r.filterServices, services) {
		r.filterMu.Unlock()
		return
	}
	previous := r.filterServices
	r.filterServices = slices.Clone(services)
	r.filterGen++
	cancelStream := r.streamCancel
	r.filterMu.Unlock()

	// Purge cache entries for services leaving the filter: the registrar
	// sends no removal events for out-of-scope services, so without this the
	// entries go stale — and stale entries would satisfy ListEndpoints' cache
	// check, poisoning the cold path's RPC-fill with old endpoints.
	if previous != nil {
		keep := make(map[string]struct{}, len(services))
		for _, svc := range services {
			keep[svc] = struct{}{}
		}
		r.mu.Lock()
		for _, svc := range previous {
			if _, ok := keep[svc]; !ok {
				for _, byName := range r.cache {
					delete(byName, svc)
				}
			}
		}
		r.mu.Unlock()
	}

	r.log.Debug("watch service filter updated; re-asserting on stream", "services", len(services))
	if cancelStream != nil {
		cancelStream()
	}
}

// currentFilter returns the filter to assert on the next stream and its
// generation.
func (r *RegistrarRegistry) currentFilter() ([]string, uint64) {
	r.filterMu.Lock()
	defer r.filterMu.Unlock()
	return slices.Clone(r.filterServices), r.filterGen
}

// stringSetsEqual reports whether a and b contain the same members,
// treating nil and non-nil differently (nil = full watch).
func stringSetsEqual(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

// signalChange performs a non-blocking send on the notify channel, coalescing
// bursts of events into a single pending signal.
func (r *RegistrarRegistry) signalChange() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Initialize connects to the Registrar and starts the background watch stream.
func (r *RegistrarRegistry) Initialize(ctx context.Context) error {
	opts := r.config.DialOptions
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	// No-op until OTel providers are registered (--otel-enabled / --tracing-enabled).
	opts = append(opts, grpc.WithStatsHandler(telemetry.ClientStatsHandler()))
	conn, err := grpc.NewClient(r.config.Address, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to registrar at %s: %w", r.config.Address, err)
	}

	r.conn = conn
	r.client = registrarv1.NewRegistrarServiceClient(conn)

	watchCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.watchLoop(watchCtx)

	r.log.InfoContext(ctx, "initialized registrar registry", "address", r.config.Address)
	return nil
}

// Close shuts down the watch stream and closes the gRPC connection.
func (r *RegistrarRegistry) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// RegisterEndpoint delegates to the Registrar's RegisterEndpoint RPC.
func (r *RegistrarRegistry) RegisterEndpoint(ctx context.Context, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint) error {
	_, err := r.client.RegisterEndpoint(ctx, &registrarv1.RegisterEndpointRequest{
		ServiceName: serviceName,
		Protocol:    protocol,
		Endpoint:    endpoint,
	})
	return err
}

// UnregisterEndpoint delegates to the Registrar's UnregisterEndpoint RPC.
func (r *RegistrarRegistry) UnregisterEndpoint(ctx context.Context, serviceName string, ip string) error {
	_, err := r.client.UnregisterEndpoint(ctx, &registrarv1.UnregisterEndpointRequest{
		ServiceName: serviceName,
		Ips:         []string{ip},
	})
	return err
}

// UnregisterEndpoints delegates to the Registrar's UnregisterEndpoint RPC with multiple IPs.
func (r *RegistrarRegistry) UnregisterEndpoints(ctx context.Context, serviceName string, ips []string) error {
	_, err := r.client.UnregisterEndpoint(ctx, &registrarv1.UnregisterEndpointRequest{
		ServiceName: serviceName,
		Ips:         ips,
	})
	return err
}

// ListEndpoints returns endpoints for a service from the local cache.
// Falls back to the Registrar RPC if the cache is empty.
func (r *RegistrarRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.mu.RLock()
	eps, ok := r.cache[protocol][service]
	r.mu.RUnlock()

	if ok {
		return eps, nil
	}

	// Fallback to RPC.
	return r.listEndpointsFromServer(ctx, service, protocol)
}

// ListAllEndpoints returns all endpoints from the local cache.
// Falls back to the Registrar RPC if the cache is empty.
func (r *RegistrarRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	// Serve from the watch-fed cache once it holds a complete world view (first
	// SNAPSHOT_COMPLETE). Gating on readiness — not on a non-empty partition —
	// lets a protocol with no services (e.g. TCP when only HTTP exists) return
	// the truthful empty set instead of falling back to an RPC.
	select {
	case <-r.ready:
		r.mu.RLock()
		byName := r.cache[protocol]
		result := make(map[string][]*registryv1.ServiceEndpoint, len(byName))
		for k, v := range byName {
			result[k] = v
		}
		r.mu.RUnlock()
		return result, nil
	default:
	}

	// Cache not yet ready: fall back to RPC.
	return r.listAllEndpointsFromServer(ctx, protocol)
}

// ListAllEndpointsAuthoritative lists endpoints via the ListAllEndpoints RPC,
// bypassing the watch-fed cache. It satisfies the registry.AuthoritativeLister
// capability: the cache can be a stale superset of a fresh registrar's
// snapshot (an empty snapshot emits no FULL_SNAPSHOT events, so the cache is
// never cleared), and reconciliation diffing against it would silently no-op.
func (r *RegistrarRegistry) ListAllEndpointsAuthoritative(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	return r.listAllEndpointsFromServer(ctx, protocol)
}

// ListConfig fetches the clusterset-wide config projections via the registrar's
// ListAllConfig RPC (proposal 026). It satisfies registry.ConfigImporter: the agent
// imports cross-cluster GAMMA config through the registrar, never reading the store
// directly. An empty result (e.g. a backend with no cross-cluster config plane) is
// not an error.
func (r *RegistrarRegistry) ListConfig(ctx context.Context) ([]*registryv1.ServiceConfigProjection, error) {
	resp, err := r.client.ListAllConfig(ctx, &registrarv1.ListAllConfigRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list config projections from registrar: %w", err)
	}
	return resp.GetProjections(), nil
}

func (r *RegistrarRegistry) listEndpointsFromServer(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	resp, err := r.client.ListAllEndpoints(ctx, &registrarv1.ListAllEndpointsRequest{
		Protocol: protocol,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list endpoints from registrar: %w", err)
	}

	svcEps, ok := resp.GetServices()[service]
	if !ok {
		return nil, nil
	}
	return svcEps.GetEndpoints(), nil
}

func (r *RegistrarRegistry) listAllEndpointsFromServer(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	resp, err := r.client.ListAllEndpoints(ctx, &registrarv1.ListAllEndpointsRequest{
		Protocol: protocol,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list all endpoints from registrar: %w", err)
	}

	result := make(map[string][]*registryv1.ServiceEndpoint, len(resp.GetServices()))
	for svcName, svcEps := range resp.GetServices() {
		result[svcName] = svcEps.GetEndpoints()
	}
	return result, nil
}

// watchLoop maintains a persistent WatchEndpoints stream, reconnecting with
// exponential backoff on disconnect. Every (re)connect asserts the current
// service filter; when the filter changed since the stream the resume token
// was earned on, the token is cleared so the registrar resends the full
// (re-filtered) snapshot — newly in-scope services were never delivered to
// the old stream, so resuming by version would silently skip them.
func (r *RegistrarRegistry) watchLoop(ctx context.Context) {
	backoff := initialBackoff
	lastVersion := ""
	lastFilterGen := uint64(0)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		services, filterGen := r.currentFilter()
		if filterGen != lastFilterGen {
			lastVersion = ""
			lastFilterGen = filterGen
		}
		req := &registrarv1.WatchEndpointsRequest{
			ClusterName: r.config.ClusterName,
			NodeName:    r.config.NodeName,
			LastVersion: lastVersion,
		}
		if services != nil {
			req.Filter = &registrarv1.ServiceFilter{Services: services}
		}

		// Per-stream context so SetServiceFilter can end the stream and force
		// a reconnect that re-asserts the new filter.
		streamCtx, streamCancel := context.WithCancel(ctx)
		r.filterMu.Lock()
		r.streamCancel = streamCancel
		r.filterMu.Unlock()

		stream, err := r.client.WatchEndpoints(streamCtx, req)
		if err != nil {
			streamCancel()
			r.metrics.streamFailed(ctx)
			jitter := time.Duration(float64(backoff) * jitterFraction * rand.Float64())
			wait := backoff + jitter
			r.log.ErrorContext(ctx, "failed to start watch stream, retrying", "error", err, "backoff", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Reset backoff on successful connection.
		backoff = initialBackoff
		r.metrics.streamReconnected(ctx)
		r.log.DebugContext(ctx, "watch stream connected", "filtered", services != nil, "filterServices", len(services))
		r.signalReconnect()

		lastVersion = r.processStream(ctx, stream, lastVersion)
		streamCancel()
	}
}

// processStream reads events from the stream and updates the local cache.
// It returns the last version seen, for use as a resume token.
func (r *RegistrarRegistry) processStream(ctx context.Context, stream registrarv1.RegistrarService_WatchEndpointsClient, lastVersion string) string {
	snapshotCleared := false
	// Catalog replay: SERVICE_ADDED events before SNAPSHOT_COMPLETE rebuild
	// the service set, swapped in at the marker — but only when the server
	// actually resent state (the marker's version differs from our resume
	// token); a current client keeps its catalog.
	connectVersion := lastVersion
	catalogReplay := make(map[string]struct{})

	for {
		event, err := stream.Recv()
		if err != nil {
			return r.handleStreamError(ctx, err, lastVersion)
		}

		// Clear cache before the first FULL_SNAPSHOT event to replace stale data.
		if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT && !snapshotCleared {
			r.mu.Lock()
			r.cache = make(map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint)
			r.mu.Unlock()
			snapshotCleared = true
		}

		r.handleCatalogEvent(ctx, event, &catalogReplay, connectVersion)
		r.applyEvent(ctx, event)

		if event.GetVersion() != "" {
			lastVersion = event.GetVersion()
			r.metrics.versionApplied(ctx, lastVersion)
		}
	}
}

// handleStreamError classifies a stream.Recv error and returns the appropriate resume token.
func (r *RegistrarRegistry) handleStreamError(ctx context.Context, err error, lastVersion string) string {
	// DataLoss means the registrar force-resynced this watcher (its
	// event buffer overflowed). Resuming from lastVersion could skip
	// the missed events — batches share a version, so lastVersion may
	// match the current version while events were still dropped. Clear
	// the resume token so the reconnect receives a full snapshot.
	if status.Code(err) == codes.DataLoss {
		r.log.InfoContext(ctx, "registrar forced a resync; requesting full snapshot on reconnect")
		return ""
	}
	if status.Code(err) == codes.Canceled && ctx.Err() == nil {
		// The stream was cancelled locally (SetServiceFilter
		// re-asserting a changed filter), not a registrar failure.
		r.log.DebugContext(ctx, "watch stream ended for filter re-assertion")
		return lastVersion
	}
	if err != io.EOF && ctx.Err() == nil {
		r.log.ErrorContext(ctx, "watch stream disconnected", "error", err)
	}
	return lastVersion
}

// handleCatalogEvent processes SERVICE_ADDED, SERVICE_REMOVED, and SNAPSHOT_COMPLETE
// events to maintain the services catalog.
func (r *RegistrarRegistry) handleCatalogEvent(ctx context.Context, event *registrarv1.WatchEndpointsResponse, catalogReplay *map[string]struct{}, connectVersion string) {
	switch event.GetType() {
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED:
		if *catalogReplay != nil {
			// Pre-marker: catalog replay, accumulated and swapped at the
			// marker so a reconnect can't leave stale names behind.
			(*catalogReplay)[event.GetServiceName()] = struct{}{}
		} else {
			// Post-marker: incremental transition.
			r.mu.Lock()
			r.services[event.GetServiceName()] = struct{}{}
			r.mu.Unlock()
		}
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED:
		r.mu.Lock()
		delete(r.services, event.GetServiceName())
		r.mu.Unlock()
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE:
		if *catalogReplay != nil && event.GetVersion() != connectVersion {
			// The server resent state: swap the catalog wholesale
			// (possibly to empty — a fresh registrar with no services).
			r.mu.Lock()
			r.services = *catalogReplay
			r.mu.Unlock()
		}
		*catalogReplay = nil
		// The cache now holds a complete world view.
		r.readyOnce.Do(func() { close(r.ready) })
	}
}

// applyEvent updates the local cache based on an endpoint event.
func (r *RegistrarRegistry) applyEvent(ctx context.Context, event *registrarv1.WatchEndpointsResponse) {
	svcName := event.GetServiceName()
	protocol := event.GetProtocol()
	ep := event.GetEndpoint()

	// Ingress validation: a mutating event must carry a namespace-qualified
	// "<ns>/<sa>" key (proposal 020). A bare/malformed key means a backend keying
	// bug (e.g. the kubernetes backend before #427); drop it with a metric +
	// rate-aware warn rather than caching a key every downstream consumer would
	// silently skip. SNAPSHOT_COMPLETE is a marker with no service name.
	if event.GetType() != registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE {
		if _, ok := serviceref.ParseKey(svcName); !ok {
			r.metrics.malformedKey(ctx)
			r.log.WarnContext(ctx, "dropping endpoint event with a non-namespace-qualified service key (backend keying bug)", "service", svcName)
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch event.GetType() {
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT:
		// Upsert for full snapshot events.
		r.upsertLocked(protocol, svcName, ep)

	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
		r.upsertLocked(protocol, svcName, ep)

	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
		r.removeLocked(protocol, svcName, ep.GetIp())

	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE:
		// Marker only; no cache mutation. The change signal below still fires
		// so consumers re-derive from the now-complete cache.
	}

	// Wake consumers (e.g. the agent xDS cache) so they re-read the registry and
	// rebuild derived state. Coalesced and non-blocking.
	r.signalChange()
}

// upsertLocked adds or updates an endpoint in the protocol's partition of the
// cache. Caller must hold mu.
func (r *RegistrarRegistry) upsertLocked(protocol registryv1.Service_Protocol, svcName string, ep *registryv1.ServiceEndpoint) {
	byName := r.cache[protocol]
	if byName == nil {
		byName = make(map[string][]*registryv1.ServiceEndpoint)
		r.cache[protocol] = byName
	}
	eps := byName[svcName]
	for i, existing := range eps {
		if existing.GetIp() == ep.GetIp() {
			eps[i] = ep
			return
		}
	}
	byName[svcName] = append(eps, ep)
}

// removeLocked removes an endpoint by IP from the protocol's partition of the
// cache. Caller must hold mu.
func (r *RegistrarRegistry) removeLocked(protocol registryv1.Service_Protocol, svcName string, ip string) {
	byName := r.cache[protocol]
	eps := byName[svcName]
	for i, existing := range eps {
		if existing.GetIp() == ip {
			byName[svcName] = append(eps[:i], eps[i+1:]...)
			if len(byName[svcName]) == 0 {
				delete(byName, svcName)
			}
			return
		}
	}
}
