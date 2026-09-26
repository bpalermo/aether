package cache

import (
	"context"
	"errors"
	"io/fs"
	"os"

	agentconstants "aethermesh.dev/agent/constants"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/serviceref"
	"aethermesh.dev/common/udspath"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// netnsExists reports whether a pod's network-namespace path is still present.
// Overridable in tests (which use synthetic netns paths). A pod whose netns is
// gone is excluded from listener generation — see LoadListenersFromStorage (at
// startup) and staleNetns (on EVERY snapshot generation).
var netnsExists = func(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// staleNetns reports whether a per-pod listener entry must be left out of the
// snapshot because the pod's network namespace is gone.
//
// Checking at startup only is not enough (#717). The window between a pod's
// netns being unpinned and the ghost sweep pruning its storage entry is up to
// 60s, and CNI DEL now completes without the agent when the agent is absent
// (#796), so the window opens routinely. What happens inside it is not benign:
//
//   - A hot-restart successor asked to create a listener in a netns whose path
//     is gone NACKs the WHOLE SotW LDS response and comes up with ZERO
//     listeners — Envoy opens the netns before it asks the parent for the
//     socket (the netns jump wraps the parent-socket handoff), so a listener
//     that rode the previous handoff fine is silently dropped and the port dies
//     when the parent finishes draining. Measured on the pinned snapshot.
//   - An in-place LDS modify of such a listener is ACCEPTED (the socket factory
//     is cloned, the netns never reopened), so the dangling path hides until
//     the next remove+re-add or hot restart.
//
// A stat per pod per regeneration is cheap; losing every listener on the node
// at the next proxy roll is not. The ghost sweep still owns the actual prune.
func (c *SnapshotCache) staleNetns(netns string) bool {
	return netns != "" && !netnsExists(netns)
}

// warnStaleNetnsOnce logs a skipped pod at WARN the first time it is skipped.
// A stale entry survives every regeneration until the ghost sweep prunes it, so
// an unconditional log would repeat the same line on each one.
func (c *SnapshotCache) warnStaleNetnsOnce(netns string, entry listenerEntry) {
	c.staleNetnsMu.Lock()
	defer c.staleNetnsMu.Unlock()
	if _, seen := c.staleNetnsWarned[netns]; seen {
		return
	}
	c.staleNetnsWarned[netns] = struct{}{}
	c.log.Warn("skipping pod with missing network namespace in snapshot generation (stale storage; the ghost sweep will prune it)",
		"pod", entry.cniPod.GetName(), "namespace", entry.cniPod.GetNamespace(), "netns", netns)
}

// forgetStaleNetnsWarning drops a pod's warn-once record, so an ID that is
// reused later can warn again.
func (c *SnapshotCache) forgetStaleNetnsWarning(netns string) {
	c.staleNetnsMu.Lock()
	defer c.staleNetnsMu.Unlock()
	delete(c.staleNetnsWarned, netns)
}

// AddPod generates the inbound and outbound listeners and per-pod clusters for the
// given pod and adds them to the cache keyed by the pod's network namespace, then
// regenerates the listener snapshot. Returns an error if listener generation or
// snapshot generation fails.
// applyWaypointInboundServerNames makes the pod's SNI-matched (secondary-port)
// inbound chains ALSO match the structured waypoint SNI
// <port>.<svc>.<ns>.<meshDomain>, so a cross-cluster connection forwarded
// (un-rewritten) by a node tunnel demuxes to the right served port. The primary
// port needs no change — it is caught by the no-SNI h2 chain. No-op when the
// waypoint is disabled, so inbound config is byte-identical for everyone else.
// (proposal 019 Phase 3, dest side.)
func (c *SnapshotCache) applyWaypointInboundServerNames(inbound *listenerv3.Listener, cniPod *cniv1.CNIPod) {
	if !c.waypointEnabled || inbound == nil {
		return
	}
	fqdn := proxy.ServiceClusterName(serviceref.New(cniPod.GetNamespace(), cniPod.GetServiceAccount()).Key(), c.meshDomain)
	for _, fc := range inbound.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if m == nil || len(m.GetServerNames()) == 0 {
			continue
		}
		structured := make([]string, 0, len(m.GetServerNames()))
		for _, sn := range m.GetServerNames() {
			structured = append(structured, sn+"."+fqdn)
		}
		m.ServerNames = append(m.GetServerNames(), structured...)
	}
}

// udsSocketRequestForPod returns the "<volume>/<file>" socket the pod is to be
// delivered to, and where it was declared (for logs). The pod annotation is the
// most specific declaration and wins; an EndpointPolicy attached to the pod's
// service (proposal 034 Phase 1b) is the service-level default. Empty socket =
// TCP loopback delivery.
func (c *SnapshotCache) udsSocketRequestForPod(cniPod *cniv1.CNIPod) (socket, source string) {
	if annotation := cniPod.GetAnnotations()[aetherannotations.AnnotationEndpointUDSSocket]; annotation != "" {
		return annotation, "annotation"
	}
	if policy := c.udsServicePolicyForPod(cniPod); policy != "" {
		return policy, "endpointpolicy"
	}
	return "", ""
}

// udsSocketPathForPod resolves the pod's requested socket to its host path under
// kubelet's pod-volumes directory (proposal 034). It returns "" — TCP loopback
// delivery — when no socket is requested, when UDS delivery is disabled
// (--kubelet-pods-dir empty), when the stored record carries no pod UID (written
// before the UID was persisted), or when the request fails validation.
//
// Every failure falls back rather than rejecting the pod: falling back is
// safe-degraded, not a blackhole. A UDS pod has nothing listening on its TCP
// port, so the delegated-liveness probe fails, the endpoint stays unpromoted,
// and no traffic is sent to an address that cannot serve it.
func (c *SnapshotCache) udsSocketPathForPod(ctx context.Context, cniPod *cniv1.CNIPod) string {
	socket, source := c.udsSocketRequestForPod(cniPod)
	if socket == "" {
		return ""
	}
	if c.kubeletPodsDir == "" {
		c.log.ErrorContext(ctx, "pod requests UDS delivery but it is disabled (--kubelet-pods-dir is empty); falling back to TCP loopback", "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "socket", socket, "source", source)
		return ""
	}
	if cniPod.GetUid() == "" {
		c.log.ErrorContext(ctx, "pod requests UDS delivery but its stored record has no pod UID; falling back to TCP loopback", "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "socket", socket, "source", source)
		return ""
	}
	path, err := udspath.Resolve(c.kubeletPodsDir, cniPod.GetUid(), socket)
	if err != nil {
		c.log.ErrorContext(ctx, "failed to resolve the pod's UDS socket path; falling back to TCP loopback", "error", err, "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "socket", socket, "source", source)
		return ""
	}
	return path
}

// inboundReadyIdentity is the node-wide input the per-pod inbound-readiness
// probe clusters are rendered from: the node's own SVID (the client certificate
// those probes present) and the trust domain (the validation-context secret
// name and the pod SPIFFE IDs pinned as the expected server identity).
type inboundReadyIdentity struct {
	nodeSpiffeID string
	trustDomain  string
}

// inboundReadyIdentitySnapshot copies the node identity currently in force.
// The trust domain is read from its atomic holder (the single source of truth);
// only the node SVID needs localMu.
func (c *SnapshotCache) inboundReadyIdentitySnapshot() inboundReadyIdentity {
	c.localMu.RLock()
	nodeSpiffeID := c.nodeSpiffeID
	c.localMu.RUnlock()
	return inboundReadyIdentity{nodeSpiffeID: nodeSpiffeID, trustDomain: c.currentTrustDomain()}
}

// inboundGateMissing names the precondition that keeps a pod's inbound-readiness
// probe from being programmed, or "" when the probe is programmed. It is the
// text the "gate absent" INFO line and the ungated-pods gauge are explained by.
func (c *SnapshotCache) inboundGateMissing(cniPod *cniv1.CNIPod, id inboundReadyIdentity) string {
	if reason := c.inboundGateNodeMissing(id); reason != "" {
		return reason
	}
	switch {
	case cniPod == nil:
		return "no CNI pod recorded for this listener entry"
	case cniPod.GetNetworkNamespace() == "":
		return "pod has no network namespace to bind the probe into"
	default:
		return ""
	}
}

// inboundGateNodeMissing names the NODE-WIDE precondition the gate is waiting
// on, independent of any one pod, or "" when the node is ready to gate.
func (c *SnapshotCache) inboundGateNodeMissing(id inboundReadyIdentity) string {
	switch {
	case c.edge:
		return "edge mode: no per-pod listeners"
	case !c.spireEnabled:
		return "SPIRE disabled: the inbound listener is cleartext, there is no handshake to prove"
	case id.trustDomain == "":
		return "no trust domain yet"
	case id.nodeSpiffeID == "":
		return "no node SVID yet (nothing to present as the probe's client certificate)"
	default:
		return ""
	}
}

// inboundReadyClusterFor builds a pod's inbound-readiness probe cluster, or
// returns nil when the probe would be meaningless or unbuildable (issue #815);
// inboundGateMissing enumerates and names those cases.
//
// A nil result is "this pod is UNGATED", not "this pod is unhealthy": the pod's
// gateway then carries no /healthz/inboundready_<pod> path at all and the
// liveness loop falls back to the app probe alone — the pre-#815 behaviour —
// while counting the pod as ungated so the absence is queryable.
func (c *SnapshotCache) inboundReadyClusterFor(cniPod *cniv1.CNIPod, id inboundReadyIdentity) types.Resource {
	if c.inboundGateMissing(cniPod, id) != "" {
		return nil
	}
	return proxy.NewInboundReadyProbeCluster(
		proxy.InboundReadyClusterName(cniPod),
		cniPod.GetNetworkNamespace(),
		id.nodeSpiffeID,
		proxy.ValidationContextName(id.trustDomain),
		proxy.SpiffeIDFromPod(cniPod, id.trustDomain),
	)
}

// inboundReadyClusterName returns the name of the entry's inbound-readiness
// probe cluster, or "" when the entry carries none. It reads the NAME OFF THE
// EMITTED PROTO rather than re-deriving it from the pod, so the health gateway
// can never require a cluster the CDS snapshot does not carry (which the
// health_check filter treats as unhealthy — a gate that never opens).
func inboundReadyClusterName(entry listenerEntry) string {
	cl, ok := entry.inboundReadyCluster.(*clusterv3.Cluster)
	if !ok || cl == nil {
		return ""
	}
	return cl.GetName()
}

// recomputeInboundReadyClusters brings every entry's inbound-readiness probe up
// to date with the current node identity, and reports how many pods ended up
// gated and ungated.
//
// It runs on EVERY snapshot generation, not on a handful of hand-picked
// triggers. The original #819 shape recomputed only from a bulk listener load
// and from SetNodeIdentity — which the SPIRE bridge calls exactly once, on the
// first node SVID it ever serves (`if firstServe`). Any ordering in which both
// of those fire while one precondition is still missing left the gate absent
// for the entire life of the agent, with nothing to re-run it and no signal:
// main-worker-05 on 2026-09-19 emitted no inboundready_* cluster at all despite
// a served node SVID. Convergence must not depend on catching a specific event.
//
// The per-entry work is skipped when the entry was already rendered from this
// identity, so the steady state is a map walk and a comparison — no proto is
// rebuilt and, by construction, no snapshot bytes change
// (TestSnapshotRegenerationIsANoOpPush).
func (c *SnapshotCache) recomputeInboundReadyClusters() (gated, ungated int) {
	id := c.inboundReadyIdentitySnapshot()

	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()
	for netns, entry := range c.listeners {
		if !entry.inboundReadyRendered || entry.inboundReadyFrom != id {
			entry.inboundReadyCluster = c.inboundReadyClusterFor(entry.cniPod, id)
			entry.inboundReadyFrom = id
			entry.inboundReadyRendered = true
			c.listeners[netns] = entry
		}
		if entry.inboundReadyCluster != nil {
			gated++
		} else {
			ungated++
		}
	}
	c.reportInboundGateState(id, ungated, gated)
	return gated, ungated
}

// reportInboundGateState logs the node's inbound-readiness gate state, ONCE on
// the first evaluation and again on every change, naming the missing
// precondition when the gate is not active. "The gate is silently absent" was
// the second half of the 2026-09-19 failure; one grep-able line per agent
// answers it without a metrics backend.
func (c *SnapshotCache) reportInboundGateState(id inboundReadyIdentity, ungated, gated int) {
	active := gated > 0 && ungated == 0
	reason := ""
	if !active {
		reason = c.inboundGateNodeMissing(id)
		if reason == "" {
			reason = "no local pods yet, or a pod with no network namespace"
		}
	}
	state := inboundGateState{active: active, reason: reason, gated: gated, ungated: ungated}

	c.inboundGateMu.Lock()
	unchanged := c.inboundGateReported && c.lastInboundGate == state
	c.lastInboundGate = state
	c.inboundGateReported = true
	c.inboundGateMu.Unlock()
	if unchanged {
		return
	}
	if active {
		c.log.Info("inbound-readiness promotion gate ACTIVE: HEALTHY requires an mTLS handshake with the pod's own inbound listener",
			"gated_pods", gated)
		return
	}
	c.log.Info("inbound-readiness promotion gate NOT active for every local pod; those pods keep the application probe alone",
		"gated_pods", gated, "ungated_pods", ungated, "missing", reason)
}

func (c *SnapshotCache) AddPod(ctx context.Context, cniPod *cniv1.CNIPod, trustDomain string) error {
	netns := cniPod.GetNetworkNamespace()
	c.log.DebugContext(ctx, "adding listeners for pod", "pod", cniPod.GetName(), "namespace", cniPod.GetNamespace(), "netns", netns)

	// Record the trust domain BEFORE anything derived from it is built or
	// published. The cache must never hold a listener whose identity came from a
	// trust domain it does not itself know: a concurrent regeneration reading the
	// cache's copy would then rebuild that listener from "" (#815/#819).
	c.setTrustDomain(trustDomain)

	// One node-global extension union for both the pod's HTTP listeners and its
	// capture listener (see extensionHTTPFilters).
	extensionFilters := c.podExtensionHTTPFilters(cniPod, c.extensionHTTPFilters())

	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(cniPod, trustDomain, c.meshDomain, c.emitStatsPod, !c.spireEnabled, extensionFilters, c.inboundFilterForPod(cniPod), c.udsSocketPathForPod(ctx, cniPod))
	if err != nil {
		return err
	}
	c.applyWaypointInboundServerNames(inbound, cniPod)
	inboundQUIC, err := c.generateInboundQUICListener(cniPod, trustDomain, extensionFilters)
	if err != nil {
		return err
	}
	capture, err := c.generateCaptureListener(cniPod, trustDomain, extensionFilters)
	if err != nil {
		return err
	}
	udpCapture, err := c.generateUDPCaptureListener(cniPod)
	if err != nil {
		return err
	}

	// The node identity the pod's inbound-readiness probe presents. The trust
	// domain is the one just recorded, so this can only be short of the node
	// SVID — and the per-snapshot recompute fills the probe in the moment that
	// lands, with no dependence on a one-shot trigger.
	readyIdentity := c.inboundReadyIdentitySnapshot()

	c.listenerMu.Lock()
	if c.listeners == nil {
		c.listeners = make(map[string]listenerEntry)
	}
	c.listeners[netns] = listenerEntry{
		inbound:              inbound,
		inboundQUIC:          inboundQUIC,
		outbound:             outbound,
		capture:              capture,
		udpCapture:           udpCapture,
		cniPod:               cniPod,
		appClusters:          clustersToResources(appClusters),
		healthCluster:        healthCluster,
		inboundReadyCluster:  c.inboundReadyClusterFor(cniPod, readyIdentity),
		inboundReadyFrom:     readyIdentity,
		inboundReadyRendered: true,
	}
	c.listenerMu.Unlock()

	c.setLocalWorkload(netns, proxy.SpiffeIDFromPod(cniPod, trustDomain))

	// Contribute the pod's own service and declared upstreams to the node
	// dependency set; a change signals the refresher to rebuild the scoped
	// cluster snapshot (the pod's upstream clusters must be distributed
	// before its first request — the declared warm path).
	c.setPodDependencies(netns, cniPod)

	return c.generateListenerSnapshot(ctx)
}

// RemovePod removes the inbound and outbound listeners for the pod with the
// given container network namespace, then regenerates the listener snapshot.
// If the pod does not exist in the cache, it returns nil without error.
// Returns an error if snapshot generation fails.
func (c *SnapshotCache) RemovePod(ctx context.Context, netns string) error {
	c.listenerMu.Lock()
	_, exists := c.listeners[netns]
	if exists {
		delete(c.listeners, netns)
	}
	c.listenerMu.Unlock()

	if !exists {
		return nil
	}

	c.forgetStaleNetnsWarning(netns)
	c.removeLocalWorkload(netns)

	// Shrink the node dependency set; clusters only this pod depended on are
	// dropped on the next scoped reload (after the retention grace).
	c.removePodDependencies(netns)

	return c.generateListenerSnapshot(ctx)
}

// Listeners returns all cached inbound and outbound listener resources plus
// the health gateway listener (per-pod health_check filters over the
// health_<pod> clusters, probed by the liveness loop) as a flat slice.
// Thread-safe.
func (c *SnapshotCache) Listeners() []types.Resource {
	// Edge mode: public-facing listener(s) (no per-pod inbound/outbound
	// listeners, no health gateway — the edge runs no local workloads). The edge
	// serves:
	//   - HTTP listener (plain or TLS-terminating) from HTTPRoutes
	//   - TCP listener(s) for each Gateway TCP port from TCPRoutes
	//   - TLS passthrough listener(s) for each Gateway TLS port from TLSRoutes
	if c.edge {
		return c.edgeListeners()
	}
	return c.meshListeners()
}

// edgeListeners returns the edge-mode listener set (readiness + HTTP/HTTPS/TCP/TLS).
func (c *SnapshotCache) edgeListeners() []types.Resource {
	var resources []types.Resource

	// Always emit the dedicated readiness listener so the kubelet probe has a
	// stable target independent of which public listeners are bound. Under
	// Phase 2 the public listeners move to per-Gateway internal ports and
	// nothing binds the HTTPS port — probing it there fails and wedges the roll.
	readinessPort := c.edgeReadinessPort
	if readinessPort == 0 {
		readinessPort = proxy.DefaultEdgeReadinessPort
	}
	resources = append(resources, proxy.BuildEdgeReadinessListener(readinessPort))

	if c.hasPerGatewayAddressing() {
		// Proposal 021 Phase 2: per-Gateway listeners, each with a unique name
		// (edge_gw_<ns>_<gwname>_<internalPort>). No shared edge_http/edge_https
		// listeners — every Gateway gets its own listener bound on its allocated
		// internal port. L4 (TCP/TLS passthrough) listeners are still shared.
		resources = append(resources, c.edgeGatewayListeners()...)
	} else {
		resources = c.appendEdgePhase1Listeners(resources)
	}
	resources = append(resources, c.edgeTCPListeners()...)
	return resources
}

// appendEdgePhase1Listeners appends the Phase 1 shared edge_http / edge_https /
// edge_redirect listeners to resources and returns the updated slice.
func (c *SnapshotCache) appendEdgePhase1Listeners(resources []types.Resource) []types.Resource {
	c.edgeHTTPRedirectMu.RLock()
	httpRedirect := c.edgeHTTPRedirect
	c.edgeHTTPRedirectMu.RUnlock()

	if c.edgeTLSEnabled {
		// TLS listener on httpsPort, under a name DISTINCT from the :80 listener so
		// the two never collide in the snapshot/LDS (a shared name dropped :443).
		resources = append(
			resources,
			proxy.BuildEdgeListener(proxy.EdgeHTTPSListenerName, c.edgeHTTPSPort, c.edgeTLSSecretNames()),
		)
	}
	if httpRedirect {
		// At least one Gateway opted into HTTP→HTTPS redirect: emit the redirect
		// listener on the plain HTTP port (replaces a routing listener for that port).
		resources = append(resources, proxy.BuildEdgeRedirectListener(c.edgeHTTPPort))
	} else {
		// Default: the HTTP-port listener serves its attached HTTPRoutes directly.
		resources = append(resources, proxy.BuildEdgeListener(proxy.EdgeListenerName, c.edgeHTTPPort, nil))
	}
	return resources
}

// meshListeners returns the mesh-mode listener set (per-pod inbound/outbound +
// health gateway + optional east-west waypoint tunnel).
func (c *SnapshotCache) meshListeners() []types.Resource {
	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	resources := make([]types.Resource, 0, 2*len(c.listeners)+1)
	probes := make([]proxy.HealthGatewayProbe, 0, len(c.listeners))
	var stale int64
	for netns, entry := range c.listeners {
		// A pod whose netns is gone must not reach an LDS response at all: the
		// successor of the next hot restart would NACK the whole thing and come
		// up with no listeners (see staleNetns).
		if c.staleNetns(netns) {
			stale++
			c.warnStaleNetnsOnce(netns, entry)
			continue
		}
		// appendListener filters out any nil / typed-nil / malformed listener
		// (empty Name AND no Address) so a single bad per-pod resource cannot make
		// Envoy NACK the entire LDS push ("address is necessary"), which would drop
		// every good listener in the same delta and wedge a pod added in the window.
		resources = appendListener(resources, entry.inbound)
		resources = appendListener(resources, entry.inboundQUIC)
		resources = appendListener(resources, entry.outbound)
		resources = appendListener(resources, entry.capture)
		resources = appendListener(resources, entry.udpCapture)
		if hc, ok := entry.healthCluster.(*clusterv3.Cluster); ok && hc != nil {
			// The pod's gateway path answers 200 only when the app probe AND —
			// when it is programmed — the inbound-readiness probe both pass
			// (issue #815). A pod without the second cluster keeps exactly
			// today's single-cluster gate, which is what makes SPIRE-off and
			// pre-node-SVID snapshots byte-identical.
			probes = append(probes, proxy.NewHealthGatewayProbe(hc.GetName(), inboundReadyClusterName(entry)))
		}
	}
	// Counted once per generation, from the listener pass only: appClusters()
	// applies the same filter but must not double-count the same pod. The
	// context is Background because the read path carries none — a counter
	// needs no trace linkage, and the WARN above names the pod.
	c.metrics.StaleNetnsSkipped(context.Background(), stale)

	// c.listeners is keyed by netns, so the per-pod listeners come out in a
	// random order. (probeClusters is sorted inside BuildHealthGatewayListener.)
	sortResourcesByName(resources)
	resources = append(resources, proxy.BuildHealthGatewayListener(agentconstants.DefaultProxyHealthSocketPath, probes))
	// East/west waypoint (proposal 019): one host-netns tunnel listener that
	// SNI-forwards cross-cluster mTLS to the services this node hosts. nil unless
	// --east-west-waypoint is set and the node hosts mesh pods.
	if tunnel := c.waypointTunnelListenerLocked(); tunnel != nil {
		resources = append(resources, tunnel)
	}
	return resources
}

// appendListener appends r to resources only when it is a usable Listener.
//
// It rejects three failure modes that would otherwise poison the LDS push:
//   - a nil interface (entry never populated);
//   - a *typed*-nil *listener.v3.Listener wrapped in a non-nil types.Resource
//     (the historical bug: generateUDPCaptureListener returned the nil pointer
//     directly when there were no UDPRoute backends, so the `!= nil` guard passed);
//   - a malformed listener with no Name AND no Address. go-control-plane assigns a
//     random UUID name to any nameless Listener, and Envoy then rejects it with
//     "error adding listener named '<UUID>': address is necessary" — NACK'ing the
//     whole delta and dropping the good listeners alongside it.
//
// A listener that has a Name but somehow lacks an Address is still kept here (it is
// not the typed-nil pathology); the generators always set both, so this only ever
// drops genuinely empty resources.
func appendListener(resources []types.Resource, r types.Resource) []types.Resource {
	if r == nil {
		return resources
	}
	l, ok := r.(*listenerv3.Listener)
	if !ok {
		// Not a Listener (should not happen for listener entries); keep it rather
		// than silently dropping an unexpected-but-valid resource.
		return append(resources, r)
	}
	if l == nil {
		// Typed-nil pointer wrapped in a non-nil interface.
		return resources
	}
	if l.GetName() == "" && l.GetAddress() == nil {
		// Nameless + addressless: the exact shape Envoy rejects with
		// "address is necessary". Drop it so the rest of the push survives.
		return resources
	}
	return append(resources, l)
}

// appClusters returns the per-pod application clusters (one per managed pod)
// as a resource slice. These STATIC clusters forward decrypted inbound traffic
// to each pod's own application on loopback. They are kept alongside listeners
// rather than in the registry-driven cluster map so registry reloads never drop
// them. Thread-safe.
func (c *SnapshotCache) appClusters() []types.Resource {
	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	resources := make([]types.Resource, 0, 2*len(c.listeners))
	for netns, entry := range c.listeners {
		// Same stale-netns filter as meshListeners (which does the counting and
		// the logging for both): a per-pod app/health cluster carries the pod's
		// NetworkNamespaceFilepath, so it is stale config too, and leaving it in
		// would keep the health checkers dialling a netns that will never exist
		// again.
		if c.staleNetns(netns) {
			continue
		}
		resources = append(resources, entry.appClusters...)
		if entry.healthCluster != nil {
			resources = append(resources, entry.healthCluster)
		}
		if entry.inboundReadyCluster != nil {
			resources = append(resources, entry.inboundReadyCluster)
		}
	}
	// c.listeners is a map: sort so the per-pod cluster set is stable.
	sortResourcesByName(resources)
	return resources
}

// clustersToResources converts a slice of concrete app clusters to the
// resource slice stored in a listenerEntry.
func clustersToResources(clusters []*clusterv3.Cluster) []types.Resource {
	resources := make([]types.Resource, 0, len(clusters))
	for _, c := range clusters {
		resources = append(resources, c)
	}
	return resources
}

// LoadListenersFromStorage retrieves all pods from the given storage backend,
// generates the inbound and outbound Envoy listeners and per-pod clusters for each
// pod, and populates the cache keyed by container network namespace. After populating
// the cache, it generates and sets a new listener snapshot.
//
// The trustDomain is the SPIFFE trust domain used for SDS secret naming.
//
// If listener generation fails for any pod, it logs the error and continues
// processing other pods. Returns an error with all accumulated errors if at
// least one pod failed, or if snapshot generation fails.
func (c *SnapshotCache) LoadListenersFromStorage(ctx context.Context, store storage.Storage[*cniv1.CNIPod], trustDomain string) error {
	c.log.DebugContext(ctx, "generating listeners")

	// Record the trust domain FIRST — before a single listener enters the map.
	//
	// This is the structural half of the #815/#819 fix. The old code recorded it
	// AFTER publishing the whole listener map, which left a window (710 ms on
	// main-worker-03, 2026-09-19) in which the cache held 15 per-pod listeners
	// and an empty trust domain. Any regeneration that started in that window —
	// the gamma reconciler's first SetServiceChainFilters did, at 17:31:22.024Z —
	// read "" and rewrote every inbound chain as `spiffe:///ns/…`.
	c.setTrustDomain(trustDomain)

	pods, err := store.GetAll(ctx)
	if err != nil {
		return err
	}
	c.log.DebugContext(ctx, "found pods in local storage", "count", len(pods))

	var errs []error
	local := make(map[string]string, len(pods))

	// Node-global union, built once for the whole loop (see extensionHTTPFilters).
	shared := c.extensionHTTPFilters()

	c.listenerMu.Lock()
	for _, pod := range pods {
		netns := pod.GetNetworkNamespace()

		// Skip a pod whose network namespace no longer exists: a missed CNI DEL
		// (or one the agent was absent for, #796) left the storage entry behind,
		// and its per-pod cluster would point Envoy at a dead netns via
		// NetworkNamespaceFilepath. That used to fault the proxy outright (talos
		// worker-01, 2026-06-19); on the pinned snapshot it is only stale config
		// whose dials fail cleanly (envoyproxy/envoy#45975 for the pool dial,
		// #46503 for the active health checkers). Skipping is still right — the
		// config can never become correct again, and programming it only buys
		// pointless health-check noise against a host that will never come back.
		// The ghost sweep prunes the stale entry; this keeps the bad config out
		// of the snapshot at startup, before that runs.
		if netns != "" && !netnsExists(netns) {
			c.log.WarnContext(ctx, "skipping pod with missing network namespace (stale storage; CNI DEL likely missed)", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)
			continue
		}
		c.log.DebugContext(ctx, "generating listeners for pod", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "netns", netns)

		extensionFilters := c.podExtensionHTTPFilters(pod, shared)
		inbound, outbound, appClusters, healthCluster, listenerErr := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, c.meshDomain, c.emitStatsPod, !c.spireEnabled, extensionFilters, c.inboundFilterForPod(pod), c.udsSocketPathForPod(ctx, pod))
		if listenerErr != nil {
			c.log.ErrorContext(ctx, "failed to generate listeners for pod", "error", listenerErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, listenerErr)
			continue
		}
		c.applyWaypointInboundServerNames(inbound, pod)
		inboundQUIC, quicErr := c.generateInboundQUICListener(pod, trustDomain, extensionFilters)
		if quicErr != nil {
			c.log.ErrorContext(ctx, "failed to generate inbound QUIC listener for pod", "error", quicErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, quicErr)
			continue
		}
		capture, captureErr := c.generateCaptureListener(pod, trustDomain, extensionFilters)
		if captureErr != nil {
			c.log.ErrorContext(ctx, "failed to generate capture listener for pod", "error", captureErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, captureErr)
			continue
		}
		udpCapture, udpCaptureErr := c.generateUDPCaptureListener(pod)
		if udpCaptureErr != nil {
			c.log.ErrorContext(ctx, "failed to generate UDP capture listener for pod", "error", udpCaptureErr, "pod", pod.GetName(), "namespace", pod.GetNamespace())
			errs = append(errs, udpCaptureErr)
			continue
		}
		c.listeners[netns] = listenerEntry{
			inbound:       inbound,
			inboundQUIC:   inboundQUIC,
			outbound:      outbound,
			capture:       capture,
			udpCapture:    udpCapture,
			cniPod:        pod,
			appClusters:   clustersToResources(appClusters),
			healthCluster: healthCluster,
			// inboundReadyCluster is filled by recomputeInboundReadyClusters
			// below, once the trust domain and node identity are recorded.
		}
		local[netns] = proxy.SpiffeIDFromPod(pod, trustDomain)
		// Contribute to the node dependency set so the scoped registry load
		// that follows (PreListen) carries these pods' upstreams.
		c.setPodDependencies(netns, pod)
	}
	c.listenerMu.Unlock()

	// Merge (never replace) into localWorkloads: this load runs concurrently with
	// the CNI server, and a wholesale replacement would wipe the netns→SPIFFE-ID
	// mapping of a pod whose AddPod landed between the storage GetAll above and
	// this write — silently downgrading that pod's outbound mTLS to the node
	// certificate (the matcher's no-match path). At startup the map is empty, so
	// merge and replace are otherwise equivalent.
	c.localMu.Lock()
	for netns, id := range local {
		c.localWorkloads[netns] = id
	}
	c.localMu.Unlock()

	// The merged workload map (and trust domain) feed every cached
	// mTLS-injected cluster; rebuild them before the snapshot below reads the
	// cache (issue #537). The per-pod inbound-readiness probes are refreshed by
	// generateSnapshot itself, so they need no trigger here.
	c.recomputeMTLSClusters()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	listeners := c.Listeners()
	c.log.DebugContext(ctx, "generated listeners", "count", len(listeners))

	return c.generateListenerSnapshot(ctx)
}

// generateListenerSnapshot regenerates the node snapshot after a listener change.
// It delegates to generateSnapshot, which emits a complete snapshot of all
// resource types so listener updates do not clobber clusters, routes or secrets.
func (c *SnapshotCache) generateListenerSnapshot(ctx context.Context) error {
	return c.generateSnapshot(ctx)
}

// generateInboundQUICListener builds the pod's HTTP/3 inbound listener
// (proposal 038 Phase 4), or returns an untyped nil when there is none
// (SPIRE off). The untyped nil matters: a typed-nil *Listener inside a
// non-nil types.Resource would defeat appendListener's guard and reach LDS as
// an empty listener, which Envoy NACKs whole ("address is necessary").
func (c *SnapshotCache) generateInboundQUICListener(cniPod *cniv1.CNIPod, trustDomain string, extensionFilters []*http_connection_managerv3.HttpFilter) (types.Resource, error) {
	l, err := proxy.NewInboundQUICListener(cniPod, trustDomain, c.emitStatsPod, !c.spireEnabled, proxy.WithoutSourceMetadata(extensionFilters), c.inboundFilterForPod(cniPod))
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	return l, nil
}
