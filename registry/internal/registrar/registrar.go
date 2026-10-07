// Package registrar implements the Registry interface using a Registrar gRPC service.
// It caches endpoints locally from a server-streaming watch and delegates writes
// to the Registrar, which in turn persists them to the external registry backend.
package registrar

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/serviceref"
	"aethermesh.dev/common/snapshotversion"
	"aethermesh.dev/common/spire"
	"aethermesh.dev/common/telemetry"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
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
	// goawayNoErrorDetail is how grpc-go renders a server-initiated, graceful
	// HTTP/2 GOAWAY in the status message of the Unavailable error the stream
	// dies with. From google.golang.org/grpc/internal/transport/http2_client.go:
	//
	//	status.Newf(codes.Unavailable, "closing transport due to: %v, received prior goaway: %v", err, goAwayDebugMessage)
	//	goAwayDebugMessage = fmt.Sprintf("code: %s", f.ErrCode)          // no debug data
	//	goAwayDebugMessage = fmt.Sprintf("code: %s, debug data: %q", …)  // with debug data
	//
	// grpc-go exposes the GOAWAY code nowhere structurally — the status carries
	// no details, and internal/transport is not importable — so matching this
	// substring on the status message is the discriminator. grpc-go's own
	// test/goaway_test.go asserts against the same literal, and the unit test
	// in this package pins it against real grpc-go output rather than a
	// hand-written string.
	goawayNoErrorDetail = "received prior goaway: code: NO_ERROR"
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
	// Instance distinguishes this agent from another watching for the same
	// node at the same time — a surge roll's standby and the agent it replaces
	// (proposal 041). Optional: empty keys the watch by cluster and node only.
	Instance string
	// DialOptions are additional gRPC dial options (e.g., TLS credentials).
	// When empty, insecure credentials are used.
	DialOptions []grpc.DialOption
	// IdentityReady reports whether this client can complete an mTLS handshake
	// yet — i.e. whether SPIRE has issued this workload's SVID. Optional; nil
	// means "always ready" (SPIRE disabled, or a caller with no such notion).
	//
	// Since #740 the process no longer blocks on the first SVID, so the watch
	// stream can legitimately start before identity exists. Every such attempt
	// fails the handshake, and reporting those as stream FAILURES would bury the
	// one signal this loop's ERRORs exist for (a registrar that will not serve
	// the stream, #700) under a boot's worth of noise, and count watch_errors
	// for a condition that is not an error.
	IdentityReady func() bool
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
	// scope is the watch filter the cache admits endpoints for (nil = every
	// service): the filter most recently set, updated in the same critical
	// section that purges the services leaving it. An endpoint event for a
	// service outside it is not cached, so a stream still carrying an older,
	// wider filter cannot re-insert a service the purge just dropped.
	scope map[string]struct{}
	// held is the set of services whose cached endpoints are exactly what the
	// resume token names (nil = every service). The token is only ever
	// presented as covering held (#1239): a filter that stays within it
	// resumes with last_version, a filter that grows past it asks for the
	// missing services alone (partial_resume). Shrinks as the scope does, set to
	// the stream's filter at SNAPSHOT_COMPLETE, emptied when a resend clears
	// the cache or a stream ends inside a batch (the token is dropped, #1269).
	held map[string]struct{}
	// streamComplete is the set of services the current stream can still
	// deliver whole (nil = every service): its filter at the open, less every
	// service the filter has dropped since, whose entries were purged and whose
	// earlier events are gone even if it comes back. completeStart holds only
	// these (#1239 review, F1).
	streamComplete map[string]struct{}

	// wake cuts short the retry sleep between watch-stream attempts. Buffered
	// with capacity 1 and written non-blocking, so a signal raised while the
	// loop is not sleeping is still consumed by the next sleep. See
	// NotifyIdentityReady.
	wake chan struct{}

	// identityReadyAt is when this client was first told SPIRE had issued its
	// SVID (unix nanos; 0 = never). It bounds the window in which a failed
	// handshake is this connection catching up rather than the peer's fault —
	// see sinceIdentity and failStream.
	identityReadyAt atomic.Int64

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
	// distribution). Acquired before mu, never while holding it.
	filterMu       sync.Mutex
	filterServices []string
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
		held:        make(map[string]struct{}),
		wake:        make(chan struct{}, 1),
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
// the stream is cancelled so the loop reconnects re-asserting the new filter.
// The reconnect keeps the resume token for the services the cache still holds
// (#1239): a filter that shrank resumes without a resend, one that grew asks
// for the added services alone (see resumeFor). It satisfies the
// registry.WatchScoper capability.
func (r *RegistrarRegistry) SetServiceFilter(services []string) {
	r.filterMu.Lock()
	if stringSetsEqual(r.filterServices, services) {
		r.filterMu.Unlock()
		return
	}
	r.filterServices = slices.Clone(services)
	cancelStream := r.streamCancel

	// Purge cache entries for services outside the new filter: the registrar
	// sends no removal events for out-of-scope services, so without this the
	// entries go stale — and stale entries would satisfy ListEndpoints' cache
	// check, poisoning the cold path's RPC-fill with old endpoints. This
	// includes a switch from the full watch, whose cache holds every service.
	//
	// Still under filterMu: two concurrent calls must leave the cache scope
	// equal to the filter the loop asserts, never the older of the two.
	scope := serviceSet(services)
	r.mu.Lock()
	r.scope = scope
	r.held = intersect(r.held, scope)
	r.streamComplete = intersect(r.streamComplete, scope)
	r.purgeExceptLocked(scope)
	r.mu.Unlock()
	r.filterMu.Unlock()

	r.log.Debug("watch service filter updated; re-asserting on stream", "services", len(services))
	if cancelStream != nil {
		cancelStream()
	}
}

// purgeExceptLocked drops the cached endpoints of every service outside keep
// (nil = keep everything). Caller must hold mu for writing.
func (r *RegistrarRegistry) purgeExceptLocked(keep map[string]struct{}) {
	if keep == nil {
		return
	}
	for _, byName := range r.cache {
		for svc := range byName {
			if _, ok := keep[svc]; !ok {
				delete(byName, svc)
			}
		}
	}
}

// serviceSet converts a filter to a set, preserving nil (= every service).
func serviceSet(services []string) map[string]struct{} {
	if services == nil {
		return nil
	}
	set := make(map[string]struct{}, len(services))
	for _, svc := range services {
		set[svc] = struct{}{}
	}
	return set
}

// intersect returns a ∩ b as a new set, where nil is the set of every service.
func intersect(a, b map[string]struct{}) map[string]struct{} {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return maps.Clone(b)
	case b == nil:
		return maps.Clone(a)
	}
	out := make(map[string]struct{}, min(len(a), len(b)))
	for svc := range a {
		if _, ok := b[svc]; ok {
			out[svc] = struct{}{}
		}
	}
	return out
}

// subset reports whether a ⊆ b, where nil is the set of every service.
func subset(a, b map[string]struct{}) bool {
	if b == nil {
		return true
	}
	if a == nil {
		return false
	}
	for svc := range a {
		if _, ok := b[svc]; !ok {
			return false
		}
	}
	return true
}

// inScope reports whether set admits svc (nil admits every service).
func inScope(set map[string]struct{}, svc string) bool {
	if set == nil {
		return true
	}
	_, ok := set[svc]
	return ok
}

// streamOpen is how one watch stream was opened: its filter and its resume
// request, which together decide what the initial exchange means.
type streamOpen struct {
	// filter is the stream's watch filter (nil = full watch).
	filter map[string]struct{}
	// lastVersion is the resume token sent as last_version.
	lastVersion string
	// partial is sent instead of a token when the filter grew past what the
	// token covers (#1239).
	partial *registrarv1.PartialResume
	// noToken: last_version was empty, so the registrar resends in full unless
	// it honoured partial (SNAPSHOT_COMPLETE.extended). Every registrar
	// version resends on an empty last_version, which is what makes an empty
	// resend detectable (completeStart).
	noToken bool
}

// resumeFor decides how the stream about to assert filter (nil = full watch)
// resumes from token, the token the cache last earned (#1239).
//
// The token names the registrar's WHOLE contents at some version V, but the
// cache holds V only for the services in held: those of the filter the token
// was earned under, less every service the filter has dropped since (their
// entries are purged, and events for them are no longer admitted). It is
// presented accordingly:
//
//   - filter ⊆ held (a shrink, or no change): last_version = token. The
//     registrar answers current/renamed iff V's contents are its current
//     contents, and then the cache holds the current endpoints of every
//     service in filter. Safe against every registrar version, because the
//     claim is exactly the one a token has always made.
//   - filter ⊄ held (a growth): the added services were never delivered at V,
//     so last_version = token would be answered "current" with them missing.
//     The token goes in partial_resume instead, naming held ∩ filter; a
//     registrar that predates the field sees no token and resends in full.
//
// Either way the cache keeps only held ∩ filter: anything else is either out
// of the filter or not covered by the token (for example the partial
// endpoints of a service that left and re-entered the filter while one
// stream was live), and an extended or resent start rebuilds it.
func (r *RegistrarRegistry) resumeFor(filter map[string]struct{}, token string) streamOpen {
	open := streamOpen{filter: filter, noToken: true}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Everything this stream is about to deliver starts from here: a purge
	// before this point cannot have cost it an event.
	r.streamComplete = filter
	if token == "" {
		r.held = make(map[string]struct{})
		return open
	}
	keep := intersect(filter, r.held)
	r.held = keep
	r.purgeExceptLocked(keep)
	if subset(filter, keep) {
		open.lastVersion = token
		open.noToken = false
		return open
	}
	services := make([]string, 0, len(keep))
	for svc := range keep {
		services = append(services, svc)
	}
	slices.Sort(services)
	open.partial = &registrarv1.PartialResume{Version: token, Services: services}
	return open
}

// assertFilter publishes cancel as the canceller for the stream the watch loop
// is about to open and returns the filter that stream must assert. Both halves
// happen in ONE filterMu critical section, and that is the whole point: it
// makes the re-assert lossless.
//
// Read the filter in one critical section and publish the canceller in another
// and a SetServiceFilter landing in between is lost (#772, S8): it reads the
// PREVIOUS stream's streamCancel — already spent — so its cancel is a no-op,
// while this loop proceeds to open a stream carrying the filter it read before
// the update. Nothing cancels that stream, so the new dependency set is never
// asserted until the stream dies for some other reason. That is the shape of
// the #682 "demand-set shrink invisible until the next roll" stall.
//
// Holding the lock across both makes the two interleavings the only ones:
// SetServiceFilter runs entirely before (this stream carries the new filter)
// or entirely after (it sees this stream's canceller and cancels it).
func (r *RegistrarRegistry) assertFilter(cancel context.CancelFunc) []string {
	r.filterMu.Lock()
	defer r.filterMu.Unlock()
	r.streamCancel = cancel
	return slices.Clone(r.filterServices)
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

// registrarConnectParams is the registrar ClientConn's redial policy
// (issue #1137): 100 ms base, x1.6, 20% jitter, 500 ms cap. gRPC's default
// (1 s base, 120 s cap) is for remote servers. The registrar is an in-cluster
// Service, and how fast this connection redials is on the agent's critical
// path twice:
//
//   - At startup. The agent dials the registrar before SPIRE has issued its
//     SVID, so every attempt fails the handshake and the ClientConn backs off
//     on its own schedule, invisible to the watch loop. The xDS PreListen hold
//     waits for the registry snapshot after identity, with the node's proxy on
//     no ADS stream (#1123), so whatever backoff the ClientConn has reached when
//     the SVID lands is added to that gap. The own-SVID wait was 0.15–2 s on
//     talos-main and 1–5 s on kind. This ladder (0.1, 0.16, 0.26, 0.41, then
//     0.5 s) reaches its cap after ~0.9 s of failures, so the redial follows
//     identity by at most ~0.6 s (cap plus jitter), ~0.25 s on average. A 1 s
//     cap would double both.
//   - After an identity change that follows an outage. #740's finding 3 was
//     this ClientConn sitting at gRPC's 120 s default cap and answering every
//     RPC from its cached handshake failure for 2m11s after the SVID was back.
//     A 500 ms cap rules that out by construction.
//
// The cap also stays under initialBackoff, which bounds deferReconnect's
// wait-for-READY: one wait always spans the ClientConn's next redial, so the
// post-identity reconnect never falls through to another backoff round. And it
// is far inside spire.ReconnectWindow (5 s), whose classification assumes the
// redial lands within the window.
//
// The cost is paid only while the connection is failing: at most two dials a
// second per node. Against a registrar Service with no endpoints that is a
// refused connect; against a registrar replica with no SVID yet, a failed TLS
// handshake. MinConnectTimeout stays at gRPC's default (20 s): this policy
// changes how soon an attempt is made, not how long one may take.
//
// This replaces the ClientConn.ResetConnectBackoff call NotifyIdentityReady
// used to make, and that call must not come back. In grpc-go (1.83.2)
// ResetConnectBackoff copies the reference to the ClientConn's subchannel map
// under cc.mu, unlocks, and then iterates it, while the balancer creating a
// subchannel writes the same map under the lock (newAddrConnLocked). Announcing
// identity while the connection is being established, which is exactly when
// the agent announces it, is a data race, and can crash the process outright:
// a nil *addrConn read from the torn map
// (TestNotifyIdentityReady_DoesNotRaceSubchannelCreation failed 20 of 20 race
// runs with the call in place, 4 of them that way). The SPIRE broker client
// dropped the same call for the same reason (#1135, brokerConnectParams).
var registrarConnectParams = grpc.ConnectParams{
	Backoff: backoff.Config{
		BaseDelay:  100 * time.Millisecond,
		Multiplier: 1.6,
		Jitter:     0.2,
		MaxDelay:   500 * time.Millisecond,
	},
	MinConnectTimeout: 20 * time.Second,
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
	// The redial policy goes first so a caller that passes its own
	// WithConnectParams still wins (later options override earlier ones).
	opts := []grpc.DialOption{grpc.WithConnectParams(registrarConnectParams)}
	if len(r.config.DialOptions) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		opts = append(opts, r.config.DialOptions...)
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
//
// OWNERSHIP CONTRACT: the returned slice belongs to the caller — it is a copy,
// safe to append to, reorder, and range on any goroutine. The
// *registryv1.ServiceEndpoint elements are SHARED with the cache and with
// every other reader, so they must be treated as immutable; a caller that
// needs to change one clones it first (proto.Clone).
//
// The copy is not optional. The watch goroutine mutates a service's slice in
// place — upsertLocked overwrites eps[i], removeLocked left-shifts the tail
// over the hole — so a slice handed out by reference is a backing array being
// rewritten under the reader. That corrupts EDS content (an endpoint seen
// twice, another skipped), not merely the race detector (#772, S2).
func (r *RegistrarRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.mu.RLock()
	eps, ok := r.cache[protocol][service]
	if ok {
		eps = slices.Clone(eps)
	}
	r.mu.RUnlock()

	if ok {
		return eps, nil
	}

	// Fallback to RPC.
	return r.listEndpointsFromServer(ctx, service, protocol)
}

// ListAllEndpoints returns all endpoints from the local cache.
// Falls back to the Registrar RPC if the cache is empty.
//
// Same ownership contract as ListEndpoints: the map AND every slice in it are
// the caller's copies; the *registryv1.ServiceEndpoint elements are shared and
// immutable. Copying the map alone is not enough — its values would alias the
// cache's slices, which the watch goroutine rewrites in place (#772, S2).
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
			result[k] = slices.Clone(v)
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
// service filter and resumes from the token for the services it covers
// (resumeFor): newly in-scope services were never delivered to the old stream,
// so a grown filter asks for them explicitly rather than resuming past them.
//
// A stream the loop itself lost to a filter re-assert or to shutdown is not a
// failure: it is classified out of the error path (no ERROR, no backoff, no
// watch_errors count) so the only ERROR here is a registrar that would not
// serve the stream (issue #700).
func (r *RegistrarRegistry) watchLoop(ctx context.Context) {
	backoff := initialBackoff
	lastVersion := ""
	// The process's first stream is opened by Initialize, before the node's
	// dependency set exists, so it is routinely superseded by the startup
	// filter assert (issue #700). The marker keeps that expected handoff
	// distinguishable from a mid-life filter change in the log.
	firstStream := true

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Per-stream context so SetServiceFilter can end the stream and force
		// a reconnect that re-asserts the new filter.
		streamCtx, streamCancel := context.WithCancel(ctx)
		services := r.assertFilter(streamCancel)
		open := r.resumeFor(serviceSet(services), lastVersion)
		req := &registrarv1.WatchEndpointsRequest{
			ClusterName:   r.config.ClusterName,
			NodeName:      r.config.NodeName,
			Instance:      r.config.Instance,
			LastVersion:   open.lastVersion,
			PartialResume: open.partial,
		}
		if services != nil {
			req.Filter = &registrarv1.ServiceFilter{Services: services}
		}

		stream, err := r.client.WatchEndpoints(streamCtx, req)
		if err != nil {
			// Classify BEFORE cancelling: while the call is in flight the only
			// thing that cancels streamCtx is SetServiceFilter re-asserting a
			// changed filter (or the parent context ending) — never this loop.
			cancelled := streamCtx.Err() != nil
			streamCancel()
			keepWatching := r.recoverStreamOpen(ctx, err, cancelled, firstStream, &backoff)
			firstStream = false
			if !keepWatching {
				return
			}
			continue
		}
		firstStream = false

		// Reset backoff on successful connection.
		backoff = initialBackoff
		r.metrics.streamReconnected(ctx)
		r.log.DebugContext(ctx, "watch stream connected", "filtered", services != nil, "filterServices", len(services),
			"resume", open.lastVersion != "", "partialResume", open.partial != nil)
		r.signalReconnect()

		var failure error
		lastVersion, failure = r.consumeStream(ctx, stream, lastVersion, open)
		streamCancel()
		if failure != nil && !r.failStream(ctx, "watch stream disconnected, retrying", failure, &backoff) {
			return
		}
	}
}

// recoverStreamOpen applies the recovery for a watch stream that would not
// open, and reports whether the loop should keep watching (false = shut down).
// cancelled says whether the per-stream context was already cancelled when the
// call returned; firstStream whether this was the process's first stream.
//
// Opening a server-streaming RPC blocks in the gRPC picker until the
// connection is READY, so on a clean start that call is in flight across the
// whole dial + mTLS handshake — and the agent's startup filter assert lands
// inside that window (issue #700). Neither that handoff nor a shutdown is a
// stream failure, so neither takes the error path.
func (r *RegistrarRegistry) recoverStreamOpen(ctx context.Context, err error, cancelled, firstStream bool, backoff *time.Duration) bool {
	if ctx.Err() != nil {
		// Shutting down: the cancellation IS the shutdown.
		r.log.DebugContext(ctx, "watch stream cancelled by shutdown")
		return false
	}
	if cancelled && status.Code(err) == codes.Canceled {
		// Expected, not a failure: Initialize opens the unfiltered stream
		// before the node's dependency set exists, then the agent's xDS
		// PreListen asserts the demand-scoped filter as soon as the local pod
		// set is known (agent/internal/xds/server/server.go), cancelling the
		// in-flight dial. Reconnect immediately with the new filter — backing
		// off here would only delay the filtered snapshot.
		r.log.InfoContext(ctx, "watch stream superseded by a service-filter change before it connected; reconnecting",
			"startup", firstStream)
		return true
	}

	return r.failStream(ctx, "failed to start watch stream, retrying", err, backoff)
}

// failStream applies the failure path shared by both stream-loss shapes: count
// the failure, log it at ERROR, then sleep the current backoff with jitter and
// double it for the next attempt. It reports whether the loop should keep
// watching (false = the context ended while waiting, i.e. shutdown).
//
// Three startup transients are classified out of that path first, in
// spire.ClassifyHandshake's order — our own pending identity, the connection
// re-establishing itself in the seconds right after identity, then the peer's
// pending identity. The error text cannot order them: `x509svid: could not get
// X509 bundle` is raised by the local verifier whichever party was short, and
// the ClientConn hands back the failure it cached before identity for as long
// as it takes to redial. #718's drain classification never reaches here (an
// established stream ending on a GOAWAY returns no failure at all).
func (r *RegistrarRegistry) failStream(ctx context.Context, msg string, err error, backoff *time.Duration) bool {
	switch spire.ClassifyHandshake(err, !r.identityPending(), r.sinceIdentity()) {
	case spire.HandshakeOwnIdentityPending:
		return r.deferStream(ctx, err, *backoff)
	case spire.HandshakeReconnecting:
		return r.deferReconnect(ctx, err, backoff)
	case spire.HandshakePeerIdentityPending:
		return r.deferPeerStream(ctx, err, backoff)
	case spire.HandshakeFailure:
	}

	r.metrics.streamFailed(ctx)
	jitter := time.Duration(float64(*backoff) * jitterFraction * rand.Float64())
	wait := *backoff + jitter
	r.log.ErrorContext(ctx, msg, "error", err, "backoff", wait)
	if !r.waitBeforeRetry(ctx, wait) {
		return false
	}
	*backoff = min(*backoff*2, maxBackoff)
	return true
}

// identityPending reports whether this client is configured for mTLS but does
// not have its SVID yet, which makes a failed stream a deferral rather than a
// failure.
func (r *RegistrarRegistry) identityPending() bool {
	return r.config.IdentityReady != nil && !r.config.IdentityReady()
}

// sinceIdentity is how long ago this client was told its identity had arrived
// (NotifyIdentityReady), or a negative duration when it never was — SPIRE
// disabled, or a caller with no such notion. A negative value disables the
// reconnect window, which is what keeps those callers on their pre-#740
// classification exactly.
func (r *RegistrarRegistry) sinceIdentity() time.Duration {
	at := r.identityReadyAt.Load()
	if at == 0 {
		return -1
	}
	return time.Since(time.Unix(0, at))
}

// deferStream is the failure path's counterpart for a stream that could not be
// established because this workload has no identity yet (issue #740): announce
// it at INFO, wait out the CURRENT backoff with the usual jitter, and leave the
// backoff where it is. No ERROR, no watch_errors, and no escalation — the wait
// is on SPIRE, not on the registrar, and the moment the SVID lands the very next
// attempt handshakes. It reports whether the loop should keep watching.
func (r *RegistrarRegistry) deferStream(ctx context.Context, err error, backoff time.Duration) bool {
	jitter := time.Duration(float64(backoff) * jitterFraction * rand.Float64())
	wait := backoff + jitter
	r.log.InfoContext(ctx, "watch stream deferred until this agent has an SVID", "error", err, "backoff", wait)
	return r.waitBeforeRetry(ctx, wait)
}

// deferPeerStream is the other half of the same idea (issue #740, PR 4): OUR
// identity is ready and has been for a while, and the mTLS handshake still fails
// the way it fails when the REGISTRAR has no SVID. That is the server's startup
// — not a client fault, and not the "registrar will not serve this stream"
// condition #700 made these ERRORs for — so it is classified the way #718
// classified a drain GOAWAY: INFO, no watch_errors, bounded retry.
//
// "and has been for a while" is PR 5's correction: for the first seconds after
// identity the identical error is our own reconnect, not the peer — see
// deferReconnect and spire.ClassifyHandshake.
//
// Observed on the rev210 upgrade roll (2026-09-07 20:03:45Z): a registrar pod was
// in its Service's endpoints before it had an SVID (the readiness dwell #744
// removed), and the agent on main-worker-01 logged this as
// `failed to start watch stream, retrying` at ERROR for a replica that was
// serving seconds later.
func (r *RegistrarRegistry) deferPeerStream(ctx context.Context, err error, backoff *time.Duration) bool {
	return r.deferRetry(ctx, "registrar has no identity yet; retrying", err, backoff, "peer_identity_pending")
}

// deferReconnect is the third shape (issue #740, PR 5), and the one that used
// to be misread as the second: OUR identity has just arrived and the connection
// underneath the watch loop has not caught up with it. Every attempt made before
// the SVID landed failed the handshake, so the ClientConn holds a cached
// transport failure — with the pre-identity error text — until it redials.
//
// On the rev211 deploy roll (2026-09-07, main-worker-02) the wake fired (then a
// ResetConnectBackoff, removed by #1137) at 20:47:27.911Z, this loop logged
// `registrar has no identity yet; retrying` 1ms later, and the stream connected
// at 20:47:29.017Z. The registrar had had its identity for a minute. Blaming the
// far side for our own reconnect sends an operator to the wrong pod's logs.
//
// Same handling as deferPeerStream — INFO, no watch_errors, escalating backoff
// capped at maxBackoff — only the attribution differs.
//
// The wait itself ends early, unlike the peer's: the condition is local and
// observable. Whatever the backoff, the next attempt is made the moment the
// ClientConn reports READY (issue #1123). The wake's own retry routinely races
// the redial and loses (the ClientConn answers it from the cached pre-identity
// failure a few milliseconds before the new transport is up), and sleeping a
// full initialBackoff after that was a steady ~1.05s on every node of the
// 2026-10-02 talos agent rolls, spent with the node's proxy on no ADS stream.
func (r *RegistrarRegistry) deferReconnect(ctx context.Context, err error, backoff *time.Duration) bool {
	jitter := time.Duration(float64(*backoff) * jitterFraction * rand.Float64())
	wait := *backoff + jitter
	r.log.InfoContext(ctx, "registrar connection not yet re-established after identity; retrying", "reconnecting", true, "error", err, "backoff", wait)
	if !r.waitForConnReady(ctx, wait) {
		return false
	}
	*backoff = min(*backoff*2, maxBackoff)
	return true
}

// waitForConnReady sleeps up to wait, returning early once the ClientConn is
// READY (the retry will then succeed) or on a wake; false only when ctx ended
// (shutdown). An IDLE connection is asked to connect, since nothing else would
// move it while this loop is the only caller. With no connection (a client
// used before Initialize, or in tests) it is the plain sleep.
func (r *RegistrarRegistry) waitForConnReady(ctx context.Context, wait time.Duration) bool {
	if r.conn == nil {
		return r.waitBeforeRetry(ctx, wait)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	// A wake (a second identity notification) still cuts the wait short.
	go func() {
		select {
		case <-r.wake:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	for {
		state := r.conn.GetState()
		if state == connectivity.Ready {
			return true
		}
		if state == connectivity.Idle {
			r.conn.Connect()
		}
		if !r.conn.WaitForStateChange(waitCtx, state) {
			// The wait elapsed (or was woken): retry as before, unless this is
			// the shutdown.
			return ctx.Err() == nil
		}
	}
}

// deferRetry is the shared body of the two peer-side deferrals: announce at
// INFO, wait out the current backoff with the usual jitter, then double it
// (capped). No ERROR and no watch_errors — nothing here is a failure of either
// party.
//
// Unlike deferStream the backoff DOES escalate, because nothing wakes this loop
// for either condition: there is no local signal to fire, so the loop has to
// keep polling, and the doubling is what stops a genuinely dead registrar from
// being polled every second forever. The cap bounds the added reconnect latency.
func (r *RegistrarRegistry) deferRetry(ctx context.Context, msg string, err error, backoff *time.Duration, reason string) bool {
	jitter := time.Duration(float64(*backoff) * jitterFraction * rand.Float64())
	wait := *backoff + jitter
	r.log.InfoContext(ctx, msg, reason, true, "error", err, "backoff", wait)
	if !r.waitBeforeRetry(ctx, wait) {
		return false
	}
	*backoff = min(*backoff*2, maxBackoff)
	return true
}

// waitBeforeRetry sleeps between watch attempts, returning false if the context
// ended first (shutdown). A NotifyIdentityReady wake cuts the sleep short: the
// reason the loop was waiting has just gone away, so paying out the rest of the
// backoff is pure added downtime.
func (r *RegistrarRegistry) waitBeforeRetry(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.wake:
		return true
	case <-timer.C:
		return true
	}
}

// NotifyIdentityReady tells this client that SPIRE has now issued the SVID its
// mTLS handshake needs, so the next watch attempt should happen NOW.
//
// Two things stand between the SVID landing and the stream working:
//
//   - The gRPC ClientConn's OWN reconnect backoff, which the app-level loop
//     cannot see. Every attempt during the outage failed the handshake, so the
//     ClientConn answers new RPCs from its cached failure until its next
//     redial. On 2026-09-07 it had backed off to gRPC's default ~120s cap, and
//     the node's data path stayed down for 2m11s AFTER readiness said it had
//     recovered (#740, finding 3). That redial is now never more than ~0.6s
//     away (registrarConnectParams), and deferReconnect retries the moment the
//     ClientConn reports READY. This method deliberately does NOT call
//     ClientConn.ResetConnectBackoff: that races the balancer's subchannel
//     creation inside grpc-go (#1137; see registrarConnectParams).
//   - This loop's own sleep, which the wake cuts short.
//
// Safe to call at any time and from any goroutine, including before Initialize
// (the wake is buffered).
func (r *RegistrarRegistry) NotifyIdentityReady() {
	// Stamp the FIRST notification only: what the reconnect window measures is
	// the age of this connection's identity, and a later re-announcement must
	// not reopen a window that closed seconds after boot.
	r.identityReadyAt.CompareAndSwap(0, time.Now().UnixNano())
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// isServerDrainGoaway reports whether err is an established stream ending
// because the server drained itself: a graceful, server-initiated GOAWAY with
// NO_ERROR, which is exactly what a registrar Deployment roll produces
// (GracefulStop on SIGTERM). The agent reconnects to a surviving replica
// immediately, so this is a handoff, not a failure (issue #718).
func isServerDrainGoaway(err error) bool {
	if status.Code(err) != codes.Unavailable {
		return false
	}
	return strings.Contains(status.Convert(err).Message(), goawayNoErrorDetail)
}

// processStream reads events from a stream opened with last_version =
// lastVersion over the full watch, and updates the local cache. See
// consumeStream.
func (r *RegistrarRegistry) processStream(ctx context.Context, stream registrarv1.RegistrarService_WatchEndpointsClient, lastVersion string) (string, error) {
	return r.consumeStream(ctx, stream, lastVersion, streamOpen{lastVersion: lastVersion})
}

// consumeStream reads events from the stream and updates the local cache.
// lastVersion is the resume token the cache held when the stream was opened
// (whatever open actually presented), and open how the stream was opened.
// It returns the last version seen, for use as a resume token (empty when the
// stream ended inside a batch, see endStream), and the error
// the stream ended with when that end was a genuine failure (nil when it was
// an expected end: EOF, our own shutdown or filter re-assert, a forced resync,
// or a server drain — see handleStreamError).
func (r *RegistrarRegistry) consumeStream(ctx context.Context, stream registrarv1.RegistrarService_WatchEndpointsClient, lastVersion string, open streamOpen) (string, error) {
	snapshotCleared := false
	// Catalog replay: SERVICE_ADDED events before SNAPSHOT_COMPLETE rebuild
	// the service set, swapped in at the marker — but only when the server
	// actually resent state (the marker's version differs from our resume
	// token); a current client keeps its catalog.
	connectVersion := lastVersion
	catalogReplay := make(map[string]struct{})
	// inBatch: since the last versioned event, this stream has applied at least
	// one live event that carried no version, so the cache is past what
	// lastVersion names (#1269). See endStream.
	inBatch := false
	// For the abandoned-resend report only (abandonedResend).
	opened := time.Now()
	received := 0

	for {
		event, err := stream.Recv()
		if err != nil {
			token, reason, failure := r.handleStreamError(ctx, err, lastVersion)
			// catalogReplay is still set: the stream ended before its initial
			// exchange's marker. inBatch is false then by construction, so
			// endStream leaves the token alone.
			r.abandonedResend(ctx, token, reason, catalogReplay != nil, open, received, time.Since(opened))
			return r.endStream(ctx, token, inBatch), failure
		}
		received++

		if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE && catalogReplay == nil {
			// A version marker (#1241): the registrar's version moved with
			// nothing for this cache (a no-op store revision, or a change
			// outside the filter), and the cache holds it. Adopt it; there is
			// nothing to apply and nothing to re-derive. A registrar older than
			// #1241 never sends one.
			lastVersion = r.adoptVersion(ctx, event, lastVersion)
			inBatch = inBatch && event.GetVersion() == ""
			continue
		}
		// Only live events (after the initial exchange's marker) can leave the
		// cache past its token. Before the marker nothing is applied under the
		// presented token: a resend drops the token at its first FULL_SNAPSHOT,
		// an extension delivers only services outside held (resumeFor purges
		// them again if the stream is cut), and catalog events are buffered
		// until the marker swaps them in.
		if catalogReplay == nil {
			inBatch = event.GetVersion() == ""
		}

		// Clear cache before the first FULL_SNAPSHOT event to replace stale data.
		if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT && !snapshotCleared {
			r.clearForResend()
			snapshotCleared = true
			// The cache no longer holds what the old token names. Drop it until
			// this resend completes (SNAPSHOT_COMPLETE carries the new one): a
			// stream cut mid-resend must reconnect with no token, or a replica
			// whose contents match the OLD token -- a lagging peer, or contents
			// that reverted -- answers "current" onto an empty cache (#1203).
			lastVersion = ""
		}
		if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE {
			r.completeStart(open, event, snapshotCleared)
		}

		r.handleCatalogEvent(ctx, event, &catalogReplay, connectVersion)
		r.applyEvent(ctx, event)

		lastVersion = r.adoptVersion(ctx, event, lastVersion)
	}
}

// endStream returns the resume token a stream that ended with token leaves
// for the next one. inBatch says whether the stream ended after applying a
// live event that carried no version, i.e. inside a batch whose versioned last
// event never arrived (#1269).
//
// Such a cache holds the batch's prefix on top of what token names, so the
// token no longer names the cache: the cache is token's contents plus some, but
// not all, of the batch. Presenting it anyway is safe only while the registrar
// has moved on, because then its hash differs and it resends. When the
// registrar's contents return to the token's hash -- a change and its exact
// reversal (ABA), or a reconnect to a peer replica whose snapshot never got the
// change (a lost write-behind write) -- it answers current or renamed, or
// extended for a partial resume, and the prefix stays in the cache for good.
//
// The token is therefore dropped, and held with it (the cache holds no service
// at any version now), so the next stream is resent in full. That costs one
// resend per stream cut mid-batch, including a dependency-set change whose
// cancellation lands inside a batch. A registrar older than #1203 versions every
// event, so its streams never end in a batch and keep their token.
func (r *RegistrarRegistry) endStream(ctx context.Context, token string, inBatch bool) string {
	if !inBatch || token == "" {
		return token
	}
	r.mu.Lock()
	r.held = make(map[string]struct{})
	r.mu.Unlock()
	r.metrics.tokenDropped(ctx, tokenDropMidBatch)
	if ctx.Err() == nil { // a shutdown stays quiet (#712)
		r.log.InfoContext(ctx, "watch stream ended inside a batch; requesting a full snapshot on reconnect", "lastVersion", token)
	}
	return ""
}

// abandonedResend reports a stream that ended before the SNAPSHOT_COMPLETE of
// its initial exchange AND left no resume token (#1334): the registrar was
// resending this stream the whole filtered snapshot, that resend is lost, and
// the next stream is resent in full again. token is what the stream leaves for
// the next one, reason what ended it (handleStreamError), and beforeMarker
// whether it ended before that marker at all.
//
// The token is empty here in two ways. The stream was opened with none (the
// process's first stream, or one after a dropped token) and no marker ever
// supplied one. Or it presented a token the registrar no longer honoured, and
// the resend's first FULL_SNAPSHOT dropped it (#1203). Neither is a "token
// drop" in watch_token_drops' sense: that counts a non-empty token given up
// AFTER a completed start, and the two never count the same stream (a stream
// is inside a batch only after its marker).
//
// A stream that ends before its marker with its token intact is not reported:
// the registrar was answering current, renamed or extended, nothing was
// resent, and the next stream resumes from the same token.
//
// This changes nothing about the stream or the cache. It exists because the
// registrar's watch_starts{resume="resent"} counts both streams, and until
// this report nothing on the agent said why there were two (#1324).
func (r *RegistrarRegistry) abandonedResend(ctx context.Context, token, reason string, beforeMarker bool, open streamOpen, received int, openFor time.Duration) {
	if !beforeMarker || token != "" || reason == "" {
		// A kept token resumes; an empty reason is our own shutdown, which
		// stays quiet (#712) and has no next stream.
		return
	}
	r.metrics.resendAbandoned(ctx, reason)
	r.filterMu.Lock()
	nextFilter := r.filterServices
	r.filterMu.Unlock()
	r.log.InfoContext(ctx, "watch stream ended before its first SNAPSHOT_COMPLETE with no resume token; the snapshot it was being sent is abandoned and the next stream is resent in full",
		"reason", reason,
		"eventsReceived", received,
		"openFor", openFor.Round(time.Millisecond).String(),
		"tokenPresented", !open.noToken || open.partial != nil,
		"filtered", open.filter != nil, "filterServices", len(open.filter),
		"nextFiltered", nextFilter != nil, "nextFilterServices", len(nextFilter))
}

// adoptVersion returns the event's version as the new resume token, or token
// when the event carries none.
//
// The registrar versions only the points at which this cache holds everything
// the version names: SNAPSHOT_COMPLETE (the initial one, and since #1241 the
// version markers) and the last event of each batch it sends us (#1203).
// Adopting any non-empty version is therefore safe; an older registrar versions
// every event, which a stream cut can turn into a stale skip on reconnect
// (#1203).
func (r *RegistrarRegistry) adoptVersion(ctx context.Context, event *registrarv1.WatchEndpointsResponse, token string) string {
	v := event.GetVersion()
	if v == "" {
		return token
	}
	r.metrics.versionApplied(ctx, v)
	return v
}

// clearForResend empties the cache at the first FULL_SNAPSHOT event of a
// resend; nothing in it is held at any token any more.
func (r *RegistrarRegistry) clearForResend() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint)
	r.held = make(map[string]struct{})
}

// completeStart settles the cache at the SNAPSHOT_COMPLETE that ends a
// stream's initial exchange: from here the cache holds the marker's version for
// every service the stream could deliver whole (streamComplete) that is still
// in scope.
//
// Not the stream's whole filter: a service the filter dropped while this
// exchange was being read was purged, and a stream keeps delivering after the
// cancellation that drop caused, so if the service came back before the marker
// the cache holds only the events after the purge. It is not held, and the next
// stream asks for it again (#1239 review, F1).
//
// A full resend clears the cache at its first FULL_SNAPSHOT event, so a resend
// with nothing in the filter clears nothing and would leave stale endpoints
// standing under the new token: it is cleared here instead (emptyResend).
func (r *RegistrarRegistry) completeStart(open streamOpen, marker *registrarv1.WatchEndpointsResponse, snapshotCleared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !snapshotCleared && emptyResend(open, marker) {
		r.cache = make(map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint)
	}
	r.held = intersect(r.streamComplete, r.scope)
}

// emptyResend reports whether an initial exchange that carried no FULL_SNAPSHOT
// was nevertheless a full resend -- of a filter with no endpoints at all -- as
// told by its marker:
//
//   - extended: not a resend (the registrar honoured partial_resume);
//   - no token presented: always a resend, from every registrar version;
//   - the marker names the presented token: current;
//   - the marker's content hash equals the token's: renamed (the registrar
//     answers "renamed" only on equal hashes);
//   - anything else: the registrar resent, because the token named other
//     contents (#1239 review, P2). That includes a token or a marker without a
//     content hash, which only a registrar older than #1193 sends, and which
//     resumes only on an identical version.
//
// The comparison is snapshotversion.Compare, the one the registrar classifies
// the token with, so "renamed" here and there cannot disagree (#1272).
func emptyResend(open streamOpen, marker *registrarv1.WatchEndpointsResponse) bool {
	switch {
	case marker.GetExtended():
		return false
	case open.noToken:
		return true
	}
	return snapshotversion.Compare(open.lastVersion, marker.GetVersion()) == snapshotversion.Different
}

// handleStreamError classifies a stream.Recv error and returns the appropriate
// resume token, the reason the stream ended (one of the streamEnd constants;
// empty for our own shutdown), and the error to fail on — nil for every end
// that is not a failure, so the caller neither logs at ERROR, nor counts
// watch_errors, nor pays the reconnect backoff.
func (r *RegistrarRegistry) handleStreamError(ctx context.Context, err error, lastVersion string) (string, string, error) {
	switch {
	case status.Code(err) == codes.DataLoss:
		// DataLoss means the registrar force-resynced this watcher (its
		// event buffer overflowed). Resuming from lastVersion could skip
		// the missed events: the drop can open a hole inside a batch whose
		// versioned last event still arrived, so lastVersion may name the
		// current contents while events were dropped. Clear the resume token
		// so the reconnect receives a full snapshot.
		r.log.InfoContext(ctx, "registrar forced a resync; requesting full snapshot on reconnect")
		return "", streamEndForcedResync, nil

	case status.Code(err) == codes.Canceled && ctx.Err() == nil:
		// The stream was cancelled locally (SetServiceFilter
		// re-asserting a changed filter), not a registrar failure.
		r.log.DebugContext(ctx, "watch stream ended for filter re-assertion")
		return lastVersion, streamEndFilterChange, nil

	case ctx.Err() != nil:
		// Our own shutdown: the cancellation IS the shutdown (kept quiet by
		// #712).
		return lastVersion, "", nil

	case err == io.EOF:
		// A clean end of stream.
		return lastVersion, streamEndEOF, nil

	case isServerDrainGoaway(err):
		// The registrar is draining for a roll and sent a graceful GOAWAY;
		// the surviving replica is already there. Announce the handoff, but
		// reconnect straight away: no ERROR, no watch_errors, no backoff
		// (issue #718).
		r.log.InfoContext(ctx, "watch stream closed by server drain; reconnecting", "server_drain", true, "error", err)
		return lastVersion, streamEndServerDrain, nil
	}

	return lastVersion, streamEndError, err
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
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT,
		registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
		// A stream opened under an older, wider filter keeps delivering until
		// its cancellation lands; a service the new filter dropped was purged
		// and must not come back half-filled (see scope).
		if !inScope(r.scope, svcName) {
			return
		}
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
//
// It rewrites the slice IN PLACE (eps[i] = ep) rather than reallocating, which
// is why ListEndpoints/ListAllEndpoints hand out copies: an aliased slice would
// have its backing array rewritten under a reader ranging it (#772, S2).
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
//
// The removal left-shifts the tail over the hole IN PLACE — see upsertLocked
// for why readers must not be given the cache's own slice.
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
