// Package server implements the agent-specific Envoy xDS server.
// It builds xDS snapshots from local pod storage and the service registry,
// generating Envoy listeners, clusters, endpoints, and routes.
package server

import (
	"context"
	"log/slog"
	"time"

	"aethermesh.dev/agent/constants"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/xds"
	"aethermesh.dev/registry"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

// AgentXdsServer is an xDS server that generates Envoy configuration from local pod storage
// and a service registry. It embeds xds.XdsServer and implements the ServerCallback interface
// to generate an initial snapshot before starting to accept connections.
//
// The server maintains versioned snapshots of Envoy resources (listeners, clusters, endpoints, routes)
// and serves them to local Envoy proxy instances via the xDS protocol.
type AgentXdsServer struct {
	xds.XdsServer

	log *slog.Logger

	clusterName string
	nodeName    string
	trustDomain string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	cache *cache.SnapshotCache

	// identity gates the first snapshot on this agent holding a mesh identity.
	// Optional: nil means "no identity to wait for" (--spire-enabled=false, and
	// the edge, whose source is still created synchronously).
	identity IdentityGate

	// identityWatch is the same identity, consulted for LOG SEVERITY only. The
	// edge does not hold its first snapshot for identity (a 404-only table is what
	// it serves before its first reconcile anyway), but "the registry is
	// unreachable because this workload has no SVID yet" is still the designed
	// #740 wait and must not read as a fault (#766).
	identityWatch IdentityGate

	// readyTimeout bounds the initial snapshot's wait for a registry that can
	// serve endpoints. A field rather than the bare constant so tests can hold
	// the unreachable-registrar path to a fraction of a second; production
	// never changes it from registryReadyTimeout.
	readyTimeout time.Duration

	// retryBackoff is the background retry's first delay after a local-only
	// start; a field for the same reason readyTimeout is one.
	retryBackoff time.Duration

	// captureGate, when set, is closed once the mesh-Service reconciler has
	// projected the capture TCP service set (cache.CaptureProjected). The first
	// serve waits for it, at most captureTimeout (#1094).
	captureGate    <-chan struct{}
	captureTimeout time.Duration

	// clientCertTimeout bounds the first serve's wait for the local workloads'
	// client certificates (waitForClientCertificates, #1103). Zero means
	// clientCertificateTimeout; a field so tests can shorten it.
	clientCertTimeout time.Duration
}

// clientCertificateTimeout bounds the first serve's wait for the SPIRE bridge
// to deliver every local workload's certificate. The broker delivers them a few
// seconds after the agent's own SVID (~2 s on talos-main, ~3.5 s on kind); the
// bound exists for a pod whose SVID never comes, which would otherwise hold the
// node's xDS forever.
const clientCertificateTimeout = 5 * time.Second

// clientCertificatePoll is how often the wait re-reads the cache.
const clientCertificatePoll = 50 * time.Millisecond

// captureProjectionTimeout bounds the first serve's wait for the mesh-Service
// projection. The reconciler projects within a second or two of its informer
// syncing (~2 s after start on talos); the bound exists only for a kube API
// that cannot be listed, where holding Envoy's previous configuration a little
// longer is the safe failure and holding it forever is not.
const captureProjectionTimeout = 10 * time.Second

// SetCaptureGate makes the first serve wait (bounded) until the capture
// listener's TCP chain set is known: ready is closed by the first mesh-Service
// projection (cache.SnapshotCache.CaptureProjected).
//
// Without it the socket opens on whatever had arrived by the time the registry
// answered. On a restart that replaces Envoy's still-correct capture listener
// with one carrying none of the per-VIP/per-port TCP chains, and raw TCP to a
// mesh Service falls to the passthrough until the reconciler catches up. A
// setter for the same reason SetIdentityGate is one: the edge serves no capture
// listener and builds the same server without it.
func (s *AgentXdsServer) SetCaptureGate(ready <-chan struct{}) {
	s.captureGate = ready
	if s.captureTimeout == 0 {
		s.captureTimeout = captureProjectionTimeout
	}
}

// waitForCaptureProjection blocks until the capture gate opens, the bound
// passes, or ctx ends. A timeout is logged and the serve proceeds: the
// projection still rebuilds the listeners whenever it lands.
func (s *AgentXdsServer) waitForCaptureProjection(ctx context.Context) {
	if s.captureGate == nil {
		return
	}
	started := time.Now()
	timer := time.NewTimer(s.captureTimeout)
	defer timer.Stop()
	select {
	case <-s.captureGate:
		s.log.DebugContext(ctx, "mesh-Service projection received; capture TCP chains are complete for the first serve",
			"waited", time.Since(started).Round(time.Millisecond).String())
	case <-timer.C:
		s.log.WarnContext(ctx, "mesh-Service projection not received in time; serving the capture listener without its TCP chains until it arrives",
			"timeout", s.captureTimeout.String(), "issue", "aether#1094")
	case <-ctx.Done():
	}
}

// IdentityGate is the mesh identity as the xDS server needs to see it: is it
// here yet, and a channel to wait on until it is. commonspire.WaitingSource
// implements it.
type IdentityGate interface {
	// HasSVID reports whether this workload's identity (SVID and trust bundle)
	// is complete.
	HasSVID() bool
	// Ready returns a channel closed when that identity first arrives. It is
	// closed, not sent on, so any number of waiters may select on it.
	Ready() <-chan struct{}
}

// SetIdentityGate makes the initial snapshot wait for this agent's mesh
// identity. A setter rather than another positional argument to
// NewAgentXdsServer, for the same reason CNIServer.SetChainState is one: the
// gate is an interlock on WHEN the server may first serve, not something it
// needs in order to exist, and the edge builds the same server without one.
//
// Pass a nil gate (or never call this) to keep the pre-#740 behaviour.
func (s *AgentXdsServer) SetIdentityGate(gate IdentityGate) { s.identity = gate }

// SetIdentityWatch lets the server tell "unreachable because this workload has no
// SVID yet" from a real registry fault when it logs, WITHOUT holding the first
// snapshot the way SetIdentityGate does. For the edge.
func (s *AgentXdsServer) SetIdentityWatch(gate IdentityGate) { s.identityWatch = gate }

// identityPending reports whether this workload is still waiting for its first
// SVID, by whichever view of the identity was wired.
func (s *AgentXdsServer) identityPending() bool {
	for _, gate := range []IdentityGate{s.identity, s.identityWatch} {
		if gate != nil && !gate.HasSVID() {
			return true
		}
	}
	return false
}

// NewAgentXdsServer creates a new AgentXdsServer.
// It initializes an xDS server with a snapshot cache and registers itself as a callback
// to generate the initial Envoy snapshot before listening for client connections.
// The server listens on a Unix domain socket at the default xDS socket path.
// callbacks (optional, may be nil) observe the discovery streams — the agent
// passes the ACK tracker's callbacks so pod lifecycle can await Envoy ACKs.
func NewAgentXdsServer(ctx context.Context, clusterName string, nodeName string, trustDomain string, registry registry.Registry, storage storage.Storage[*cniv1.CNIPod], snapshotCache *cache.SnapshotCache, callbacks serverv3.Callbacks, log *slog.Logger) (*AgentXdsServer, error) {
	cfg := xds.NewServerConfig(
		xds.WithUDS(constants.DefaultXdsSocketPath),
	)

	// Watch the discovery streams for on-demand CDS subscriptions (ODCDS cold
	// path) alongside the caller's callbacks (the ACK tracker).
	observer := newOnDemandObserver(snapshotCache, registry, log)
	combined := combinedCallbacks{observer.Callbacks()}
	if callbacks != nil {
		combined = append(combined, callbacks)
	}

	// Store the registry in the cache so the edge reconciler can query mesh
	// service existence via HasRegistryService (registry-aware backend check).
	snapshotCache.SetRegistry(registry)

	aXdsServer := &AgentXdsServer{
		XdsServer:    xds.NewXdsServer(ctx, cfg, snapshotCache, combined, log),
		log:          commonlog.Named(log, "agent-xds"),
		clusterName:  clusterName,
		nodeName:     nodeName,
		trustDomain:  trustDomain,
		registry:     registry,
		storage:      storage,
		cache:        snapshotCache,
		readyTimeout: registryReadyTimeout,
		retryBackoff: time.Second,
	}

	aXdsServer.AddCallback(aXdsServer)

	return aXdsServer, nil
}

// NeedLeaderElection returns false so the xDS server runs on EVERY replica, not
// just the leader. Each edge/agent pod serves xDS to its own co-located Envoy
// over a node-local UDS; leader-gating it would leave all non-leader proxies
// without a control plane and break data-plane HA. (The embedded xds.Server
// already declares this; AgentXdsServer states it explicitly so the
// per-pod-runnable contract is visible at this type.) On the node agent (leader
// election off) this method is a no-op.
func (s *AgentXdsServer) NeedLeaderElection() bool { return false }

// PreListen generates the initial Envoy snapshot from local pod storage and the service registry.
// It creates listeners, clusters, endpoints, and routes, then sets the snapshot in the cache
// before the server starts accepting xDS client connections.
//
// It first HOLDS until this agent has a mesh identity (see holdForIdentity):
// the whole point of the local-only fallback below is to survive a registrar
// blip, and without an SVID there is no such thing as a registrar blip — every
// handshake fails, so the fallback would fire on every restart and publish a
// snapshot with no cross-node endpoints at all.
func (s *AgentXdsServer) PreListen(ctx context.Context) error {
	if !s.holdForIdentity(ctx) {
		// Shutting down before identity arrived. Return nil rather than an error:
		// the manager is already stopping and a SPIRE outage must never be the
		// reason a shutdown is reported as a failure.
		return nil
	}

	s.log.DebugContext(ctx, "generating initial snapshot")

	if err := s.cache.LoadListenersFromStorage(ctx, s.storage, s.trustDomain); err != nil {
		s.log.ErrorContext(ctx, "failed to load listeners from storage", "error", err)
		return err
	}

	// The dependency set is now known (local pods loaded): scope the registry
	// watch to it before waiting on the watch cache, so the snapshot the
	// registrar streams is the filtered one (demand-scoped distribution).
	AssertWatchFilter(s.cache, s.registry)

	// Before the registry load, not after: the load ends in a full snapshot,
	// so waiting first makes that snapshot -- the one the socket opens on --
	// carry the projected TCP chains. Its rebuild only signals; it does not
	// itself publish (#1094).
	s.waitForCaptureProjection(ctx)
	if ctx.Err() != nil {
		// Shutting down during the hold: nil, as for the identity hold.
		return nil
	}

	s.loadInitialRegistryConfig(ctx)

	s.waitForClientCertificates(ctx)

	return nil
}

// waitForClientCertificates holds the first serve (bounded) until the snapshot
// carries every local workload's client certificate (issue #1103).
//
// A restarted agent holds its own SVID long before the SPIRE bridge has
// re-delivered the pods' certificates, and until it has, the #1049 gate keeps
// those identities' `quic:` twins and selection arms out of the snapshot. A
// proxy that reconnects to that snapshot is told to REMOVE every twin it holds
// (delta xDS: a held resource missing from the snapshot is in
// removed_resources), and the requests its routes still select a twin for
// fail until the certificate's snapshot re-adds it: on kind 1-9.6 s of
// cluster_not_found, up to 56 x 503 in one restart. Envoy's default 30 s
// reconnect backoff hid this most of the time by reconnecting late; the 1 s
// cap #1103 adds to the proxy bootstrap makes the proxy reconnect within a
// second of the socket opening, so the socket must open on the complete
// snapshot. With every certificate present the reconnecting proxy finds its
// twins unchanged and keeps them.
//
// Only with an identity gate (SPIRE on, the node agent): with no certificates
// at all the #1049 gate is off and nothing is held back. A timeout serves what
// there is, as before, and says how many identities are still missing.
func (s *AgentXdsServer) waitForClientCertificates(ctx context.Context) {
	if s.identity == nil || ctx.Err() != nil {
		return
	}
	awaiting := s.cache.AwaitingClientCertificates()
	if awaiting == 0 {
		return
	}
	timeout := s.clientCertTimeout
	if timeout <= 0 {
		timeout = clientCertificateTimeout
	}
	started := time.Now()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(clientCertificatePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			s.log.WarnContext(ctx, "local workloads' client certificates not delivered in time; serving without their east-west QUIC twins until they arrive",
				"timeout", timeout.String(), "awaiting_client_cert", s.cache.AwaitingClientCertificates(), "issue", "aether#1103")
			return
		case <-tick.C:
			if s.cache.AwaitingClientCertificates() == 0 {
				s.log.InfoContext(ctx, "local workloads' client certificates delivered; serving the complete snapshot",
					"awaiting_at_start", awaiting, "waited", time.Since(started).Round(time.Millisecond).String())
				return
			}
		}
	}
}

// loadInitialRegistryConfig derives the registry half of the initial snapshot,
// waiting (bounded) until the registry can actually answer with endpoints
// before it does — and falling back to local-only config when it genuinely
// cannot.
//
// "Can answer with endpoints" is the load-bearing part, and it is what changed
// in issue #740's PR 5. The wait this replaced was
// registry.ReadyWaiter.WaitReady alone, which is satisfied by the one-shot latch
// the registrar client closes at its first SNAPSHOT_COMPLETE
// (registry/internal/registrar: readyOnce) — "a complete snapshot arrived at
// some point", not "the registry will serve this read now". The read that
// follows takes a different path (the RPC fallback when the cache is not
// serving), and on the rev211 deploy roll (2026-09-07 20:47:28Z on
// main-worker-02) it failed while the registrar client's ClientConn was still
// recovering from the handshakes it had failed before this agent acquired its
// SVID a fraction of a second earlier. The registrar was healthy and both
// replicas had been Ready for 43 seconds. The snapshot published from that
// failure had no cross-node endpoints, and the node's prober logged 316
// http_error over the ~30s until the next refresh repaired it.
//
// So the wait now ends on a SUCCESSFUL load, not on a latch: retry inside the
// same bounded budget the ready-wait already had. A registrar that is genuinely
// unreachable still costs exactly what it cost before — the budget, then the
// local-only fallback and the background retry — because a crash-looping or
// stalled agent takes down the node's CNI ADD/DEL and xDS entirely, turning a
// registrar blip into a node-wide outage (talos-main, 2026-06-10).
//
// Registries with no watch (the synchronous backends) have nothing to wait for
// and keep the single-attempt behaviour exactly.
func (s *AgentXdsServer) loadInitialRegistryConfig(ctx context.Context) {
	started := time.Now()

	rw, watchBacked := s.registry.(registry.ReadyWaiter)
	if !watchBacked {
		if err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry); err != nil {
			s.startLocalOnly(ctx, err)
		}
		return
	}

	deadline := started.Add(s.readyTimeout)

	// First the watch cache: a fresh agent that builds its snapshot from an
	// empty/partial cache opens the xDS socket serving a route config with
	// missing vhosts, and the reconnecting Envoy 404s live traffic until the
	// next refresh (rev-66 agent roll, 2026-06-11).
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	err := rw.WaitReady(waitCtx)
	cancel()
	if err != nil {
		s.log.InfoContext(ctx, "registry watch cache not complete in time; proceeding (RPC fallback / background retry will fill in)", "timeout", s.readyTimeout.String(), "error", err.Error())
	}

	if loadErr := s.loadClustersUntil(ctx, deadline); loadErr != nil {
		s.startLocalOnly(ctx, loadErr)
		return
	}

	s.log.InfoContext(ctx, "registry connected; generating the initial snapshot",
		"waited", time.Since(started).Round(time.Millisecond).String())
}

// loadClustersUntil builds the registry-derived config, retrying until it
// succeeds or the deadline passes. It always makes at least one attempt, so an
// already-expired budget behaves exactly as the single attempt it replaces.
func (s *AgentXdsServer) loadClustersUntil(ctx context.Context, deadline time.Time) error {
	backoff := initialRegistryLoadBackoff
	for {
		err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Add(backoff).Before(deadline) {
			return err
		}
		s.log.DebugContext(ctx, "registry not serving endpoints yet; holding the initial snapshot",
			"backoff", backoff.String(), "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxInitialRegistryLoadBackoff)
	}
}

// startLocalOnly publishes what the node knows on its own and keeps trying for
// the rest of the process's life. Deliberately loud: a node running on
// local-only config has no cross-node endpoints at all.
//
// The one exception is a workload that has no SVID yet: it cannot complete a
// handshake with the registrar, so the load was always going to fail, the wait
// is the designed #740 one (owned and escalated by the identity source's own
// logger), and the background retry repairs it the moment the SVID lands. That
// is WARN — an ERROR there tripped every "zero ERROR lines" gate on a healthy
// edge roll (#766). The retry escalates if identity arrives and it still fails.
func (s *AgentXdsServer) startLocalOnly(ctx context.Context, err error) {
	waitingForIdentity := s.identityPending()
	if waitingForIdentity {
		s.log.WarnContext(ctx, "registry unreachable for the initial snapshot while this workload waits for its first SVID; starting with local-only config, the background retry takes over", "error", err)
	} else {
		s.log.ErrorContext(ctx, "registry unavailable for initial snapshot; starting with local-only config and retrying in background", "error", err)
	}
	go s.retryInitialRegistryLoad(ctx, waitingForIdentity)
}

// identityHoldLogInterval is how often the identity hold re-announces itself.
// Frequent enough that an operator watching the log sees it is a hold and not a
// hang, sparse enough that a long SPIRE outage does not fill the log — the
// authoritative per-attempt ladder (including the escalation to WARN) belongs
// to the identity source's own logger, not to this one.
const identityHoldLogInterval = 15 * time.Second

// holdForIdentity blocks until this agent holds a mesh identity, reporting
// whether it got one (false = ctx ended first). With no gate wired, or with the
// identity already in hand, it returns immediately.
//
// This is the difference between a restart during a SPIRE outage costing
// nothing and costing the node its data path (issue #740, finding 1). Envoy
// keeps serving the last configuration it was given whenever its management
// server is unreachable, which is exactly why the crash loop this replaced was
// survivable: a dying agent published nothing. A LIVING agent that opens the
// xDS socket and pushes what it can does the one thing the crash loop never
// did — it REPLACES a complete snapshot with a local-only one. On main-worker-03
// on 2026-09-07 that evicted every cross-node endpoint and failed ~95% of the
// node's mesh probes for the whole 6m41s outage (+5,083 errors), on a node whose
// Envoy had been serving fine a second earlier.
//
// So while identity is pending the agent programs everything else and simply
// does not open the socket. Envoy keeps its config, the node keeps working, and
// the readiness gate plus the node taint (past their dwell) stop NEW pods from
// landing on a node that cannot give them an identity — which is the honest
// signal, since a new pod is precisely what this state cannot serve.
//
// Mid-life is already correct and is deliberately left alone: once a snapshot
// has been published the cache keeps it, so a registrar that goes away later
// costs nothing — the refresher retries in the background and Envoy holds the
// last good config.
func (s *AgentXdsServer) holdForIdentity(ctx context.Context) bool {
	if s.identity == nil || s.identity.HasSVID() {
		return true
	}

	started := time.Now()
	s.log.InfoContext(ctx, "holding xDS until this agent has an SVID; Envoy keeps its current configuration", "elapsed", time.Duration(0).String())

	ticker := time.NewTicker(identityHoldLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.identity.Ready():
			s.log.InfoContext(ctx, "identity acquired; generating the initial snapshot", "held", time.Since(started).Round(time.Millisecond).String())
			return true
		case <-ticker.C:
			s.log.InfoContext(ctx, "holding xDS until this agent has an SVID; Envoy keeps its current configuration", "elapsed", time.Since(started).Round(time.Second).String())
		}
	}
}

// registryReadyTimeout bounds how long PreListen waits for the registry to be
// able to serve this node's endpoints — the watch cache holding a complete
// snapshot AND a load succeeding against it. Generous enough to cover a
// registrar restart finishing its first external-registry sync (~3-5s observed)
// and a client connection re-establishing itself after identity (~1.1s on the
// rev211 roll), small enough that a genuinely unavailable registrar cannot stall
// agent startup.
const registryReadyTimeout = 15 * time.Second

// The retry cadence inside that budget. Short at first — the condition it exists
// for clears in about a second — then backing off so a longer outage spends the
// budget on a handful of attempts rather than a busy loop.
const (
	initialRegistryLoadBackoff    = 250 * time.Millisecond
	maxInitialRegistryLoadBackoff = 2 * time.Second
)

// retriesAfterIdentityBeforeError is how many failed loads the background retry
// tolerates AFTER the SVID has arrived before a start that was excused as
// "waiting for identity" stops being excused.
const retriesAfterIdentityBeforeError = 3

// stillExcused decides whether a failed background load is still covered by "this
// workload has no SVID yet". It stays covered while identity is pending, and for
// retriesAfterIdentityBeforeError failures after it arrives; then it logs the one
// ERROR the local-only start was spared and reports false.
func (s *AgentXdsServer) stillExcused(ctx context.Context, err error, failuresWithIdentity *int) bool {
	if s.identityPending() {
		return true
	}
	*failuresWithIdentity++
	if *failuresWithIdentity < retriesAfterIdentityBeforeError {
		return true
	}
	s.log.ErrorContext(ctx, "registry still unavailable although this workload now has its SVID; serving local-only config and retrying in background", "error", err)
	return false
}

// retryInitialRegistryLoad retries the registry-derived snapshot load with
// capped exponential backoff until it succeeds or ctx ends. excused means the
// local-only start was logged at WARN because identity was pending; once it is
// not, a registry that still cannot be loaded is a fault after all.
func (s *AgentXdsServer) retryInitialRegistryLoad(ctx context.Context, excused bool) {
	const maxBackoff = 30 * time.Second
	backoff := s.retryBackoff
	failuresWithIdentity := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := s.cache.LoadClustersFromRegistry(ctx, s.clusterName, s.nodeName, s.registry); err != nil {
			s.log.DebugContext(ctx, "registry still unavailable; will retry", "backoff", backoff.String(), "error", err)
			if excused {
				excused = s.stillExcused(ctx, err, &failuresWithIdentity)
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		s.log.InfoContext(ctx, "registry recovered; snapshot now includes registry-derived config")
		return
	}
}
