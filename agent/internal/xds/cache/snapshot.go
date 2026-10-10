package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"aethermesh.dev/agent/internal/xds/cache/snapversion"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/common/telemetry"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// snapshotVersionLabel identifies the unified snapshot in version strings.
const snapshotVersionLabel = "snapshot"

// tracerName identifies this instrumentation scope in trace backends.
const tracerName = "aether/agent-xds-cache"

// snapshotWatchUnansweredMsg is logged when SetSnapshot installed the snapshot
// and then returned an error (generateSnapshot says when it can). The build
// returns ErrWatchNotAnswered.
const snapshotWatchUnansweredMsg = "snapshot installed, but an open watch was not answered from it"

// generateSnapshot assembles a single, consistent xDS snapshot from every cached
// resource type — listeners, clusters, endpoints, the outbound route config and
// secrets — and sets it for the node.
//
// All cache mutations must funnel through here. go-control-plane's SetSnapshot
// replaces the entire node snapshot, so emitting a partial snapshot for one
// resource type (e.g. only listeners) drops every other type from Envoy — which
// makes listener, cluster and secret updates clobber one another. Resources are
// read from the in-memory maps (most are not rebuilt); the expensive part of a
// build is versioning every resource for delta xDS, which happens before
// SetSnapshot and reuses the previous build's versions (#1105, versionmemo.go).
//
// snapshotMu serializes the whole version-generate + read + SetSnapshot sequence:
// concurrent callers would otherwise interleave so the snapshot carrying the
// older version (and older content) lands last, replacing newer config in Envoy
// until the next trigger. Serialization also guarantees the last snapshot set
// always reflects the final state of every map.
func (c *SnapshotCache) generateSnapshot(ctx context.Context) (retErr error) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()

	v := snapversion.Generate(snapshotVersionLabel, c.version)

	ctx, span := otel.Tracer(tracerName).Start(ctx, "agent.snapshot.generate",
		trace.WithAttributes(telemetry.AttrSnapshotVersion.String(v)))
	defer func() { telemetry.EndSpan(span, retErr) }()
	start := time.Now()
	defer func() {
		c.metrics.Generated(ctx, time.Since(start).Seconds(), int64(c.version.Load()), retErr)
	}()

	// Reconcile the per-pod inbound-readiness probes against the node identity
	// in force RIGHT NOW, before anything reads the listener map. It is a map
	// walk and a comparison in the steady state (nothing is rebuilt unless the
	// identity changed), and running it here rather than from a handful of
	// mutators is what makes "the gate is silently absent forever" unreachable —
	// see recomputeInboundReadyClusters.
	c.recomputeInboundReadyClusters()
	// Same discipline for the TCP floor's capture chains: they are gated on the
	// node identity (#877), which arrives asynchronously, so a readiness change
	// has to rebuild the listeners that were built without it.
	c.reconcileCaptureTCPChains()
	// And for the UDP capture listener, whose bindable backend depends on the
	// cluster cache the registry fills asynchronously (#873).
	c.reconcileUDPCaptureListeners()

	// Who owns each per-pod listener, and the netns → identity index, for the
	// identity-binding lines this build logs once the snapshot is set (#1621).
	// Read here, next to the listener set they are joined to, and not when the
	// lines are written: bindingView says what the gap between the two reads
	// leaves out.
	bindings := c.takeBindingView()
	listeners := c.Listeners()
	clusters, endpoints, vhosts, pins := c.clustersEndpointsVhostsAndPinsInto(c.entryClasses, c.mtlsEntries)
	c.entryClasses, c.mtlsEntries = pins.classes, pins.mtls

	// Per-pod application clusters live alongside listeners (not in the
	// registry-driven cluster map) so registry reloads never drop them. STATIC
	// clusters carry their endpoints inline, so they need no EDS resources.
	clusters = append(clusters, c.appClusters()...)

	// East/west waypoint ingress clusters (proposal 019 Phase 3b): STATIC clusters
	// of this node's local pods per hosted service, that the tunnel listener
	// forwards cross-cluster connections to. Empty unless --east-west-waypoint.
	clusters = append(clusters, c.ewIngressClusters()...)

	// TCP floor clusters (proposal 018, Phase 3a): one per non-HTTP service in the
	// capture TCP set. These are separate EDS clusters using ALPN "aether-tcp" that
	// share endpoints with the corresponding HTTP clusters. Only emitted when capture
	// is enabled and there are non-HTTP services.
	//
	// Each floor cluster subscribes to its OWN EDS name and its load assignment
	// comes back from the same call, so the two always ride this snapshot
	// together (aether#1013).
	tcpClusters, tcpCLAs := c.captureTCPClusters()
	clusters = append(clusters, tcpClusters...)
	endpoints = append(endpoints, tcpCLAs...)

	// Edge L4 TCP clusters (proposal 018, Phase 3b north-south): one per service
	// referenced by an edge TCPRoute or TLSRoute. Like capture TCP clusters but use
	// the edge SPIRE identity and fetch SDS from spire_agent directly.
	var edgeTCP []types.Resource
	if c.edge {
		var edgeTCPCLAs []types.Resource
		edgeTCP, edgeTCPCLAs = c.edgeTCPClusters()
		clusters = append(clusters, edgeTCP...)
		endpoints = append(endpoints, edgeTCPCLAs...)
		// Cleartext k8s-Service clusters: one per unique non-mesh HTTPRoute backend
		// (BackendNamespace, service, port). STRICT_DNS, no transport socket — the
		// backend is a plain k8s Service, not a mesh-registered endpoint.
		clusters = append(clusters, c.edgeK8sBackendClusters()...)
	}
	// Everything that carries an upstream transport socket is built. If the
	// node can publish TLS now, no entry of this snapshot is reported as "no TLS
	// published" (#1482): the read is taken after the last builder that gates
	// on the node identity, so an identity that arrived during this build is
	// seen here. See promoteTLSNotPublished.
	if c.tcpFloorIdentityReady() {
		pins.promoteTLSNotPublished()
	}
	// And the other way: a TCP floor entry whose floor cluster is not among
	// the ones just built has no TLS in this snapshot, so it is not reported
	// as TLS without a pin.
	pins.demoteUnpublishedFloors(tcpClusters, edgeTCP)
	// UDP floor clusters (proposal 018, Phase 3b): one per service with UDPRoute
	// backends. These are plain EDS clusters with no transport socket — UDP datagrams
	// are not protected by mesh mTLS (known limitation). Only emitted when capture is
	// enabled and there are UDPRoute rules.
	clusters = append(clusters, c.captureUDPClusters()...)

	// ORIGINAL_DST passthrough cluster (proposal 022, M2a spike): emitted only when
	// redirect-all capture is enabled. The capture listener's DefaultFilterChain
	// routes unrecognised (non-mesh) egress here in plain TCP.
	if pt := c.capturePassthroughCluster(); pt != nil {
		clusters = append(clusters, pt)
	}

	// The floor load assignments were appended after clustersEndpointsAndVhosts
	// sorted its own: re-sort so identical inputs produce byte-identical EDS
	// (the delta/determinism contract, see config/common.go).
	sortResourcesByName(endpoints)

	c.secretMu.RLock()
	secrets := make([]types.Resource, 0, len(c.secrets))
	for _, s := range c.secrets {
		secrets = append(secrets, s)
	}
	c.secretMu.RUnlock()
	// c.secrets is a map: sort so the SDS resource set is stable.
	sortResourcesByName(secrets)

	// The shared subset-headers extension config (ECDS): every outbound
	// HCM's header_to_metadata filter references this one resource, so
	// vocabulary changes apply in place with no listener drain.
	c.subsetMu.RLock()
	subsetExt := proxy.BuildSubsetHeadersExtension(c.subsetHeaderKeys)
	c.subsetMu.RUnlock()

	resources := map[resourcev3.Type][]types.Resource{
		resourcev3.ListenerType:        listeners,
		resourcev3.ClusterType:         clusters,
		resourcev3.EndpointType:        endpoints,
		resourcev3.SecretType:          secrets,
		resourcev3.ExtensionConfigType: {subsetExt},
	}
	if c.edge {
		if c.hasPerGatewayAddressing() {
			// Proposal 021 Phase 2: one route config per Gateway (edge_rt_<ns>_<gwname>),
			// serving only that Gateway's attached virtual hosts. Each listener's RDS
			// reference is isolated — no cross-Gateway route leakage.
			resources[resourcev3.RouteType] = c.edgeGatewayRouteConfigs()
		} else {
			// Phase 1 / fallback: single shared edge_http route config.
			// Always emitted (even empty) so the listener's RDS reference resolves;
			// the catch-all 404 vhost handles unmatched authorities. No ODCDS.
			resources[resourcev3.RouteType] = []types.Resource{proxy.BuildEdgeRouteConfiguration(c.virtualHostVhosts())}
		}
	} else {
		routes := make([]types.Resource, 0, 2)
		// ALWAYS emitted, zero vhosts included — exactly as the edge and capture
		// route configs are. It used to be conditional on len(vhosts) > 0, which
		// made an agent with nothing to publish (a local-only start, see
		// loadInitialRegistryConfig) omit out_http from the snapshot entirely.
		//
		// Omitting it is not "an empty route table"; under delta ADS it is NO
		// RESPONSE AT ALL. go-control-plane only writes a delta response when the
		// subscription has resources or removals (respondDelta), and a first-time
		// RDS subscriber to a name the snapshot has never carried produces
		// neither — so the watch just stays open and silent. Envoy's egress
		// listener then warms for the whole initial_fetch_timeout, activates with
		// an unresolved route table, and answers 404 NR route_not_found on
		// everything until a later snapshot finally carries out_http. Measured on
		// node B, 2026-09-19: LDS at 16:08:50.558Z, first 404 at
		// 16:09:05.214Z (14.7s ≈ the 15s timeout), first RDS 91ms after the last
		// 404 (issue #817).
		//
		// With the config always present the subscription resolves on the first
		// push, and BuildOutboundRouteConfiguration's on-demand catch-all makes
		// the zero-vhost table a working one: mesh-shaped authorities reach their
		// cluster through ODCDS rather than a dead 404.
		routes = append(routes, proxy.BuildOutboundRouteConfiguration(vhosts, c.meshDomain))
		// Transparent capture (proposal 018, Phase 3a): the cap_http table the per-pod
		// capture listeners reference over RDS. Always emitted when capture is on (it
		// carries the on-demand catch-all) so the listeners' RDS resolves even with no
		// in-scope authorities yet, and cold/off-node services recover via ODCDS.
		if c.captureEnabled {
			// captureKnownTargets pins every in-scope mesh authority to its cluster on
			// the redirect-all catch-all (no-op when redirect-all is off), so a captured
			// request to a known service never leaks to the ORIGINAL_DST passthrough
			// while its dedicated cap_http vhost is mid-rebuild across a GAMMA churn.
			routes = append(routes, proxy.BuildCaptureRouteConfiguration(
				c.captureVhosts(), c.meshDomain, c.captureRedirectAll, c.captureKnownTargets()...,
			))
		}
		// Unconditional: out_http is always in `routes`, so an agent with nothing
		// to publish still hands Envoy a RouteType set its RDS subscription can
		// resolve against.
		resources[resourcev3.RouteType] = routes
	}

	c.log.DebugContext(ctx, "setting snapshot", "version", v,
		"listeners", len(listeners), "clusters", len(clusters),
		"endpoints", len(endpoints), "vhosts", len(vhosts), "secrets", len(secrets))
	span.SetAttributes(
		attribute.Int("aether.snapshot.listeners", len(listeners)),
		attribute.Int("aether.snapshot.clusters", len(clusters)),
		attribute.Int("aether.snapshot.endpoints", len(endpoints)),
		attribute.Int("aether.snapshot.secrets", len(secrets)),
	)

	declared, observed := c.dependencyCounts()
	c.metrics.SnapshotShape(ctx, len(clusters), declared, observed)

	// NewSnapshot indexes every type by name and keeps one resource per name
	// without a word. Say it here if two resources share one (#1584).
	c.reportDuplicateResourceNames(ctx, resources)

	snapshot, err := cachev3.NewSnapshot(v, resources)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// The per-resource version map, computed HERE -- before SetSnapshot, so
	// outside go-control-plane's cache mutex -- and memoized across builds
	// (#1105). Left to go-control-plane it is built lazily by the first delta
	// watch to look at the snapshot, under that mutex, which is what stalled
	// the ADS stream for the length of every build. See versionmemo.go.
	if err := c.fillVersionMap(ctx, span, snapshot); err != nil {
		return fmt.Errorf("failed to version snapshot resources: %w", err)
	}

	// The pin class of every cluster entry of this snapshot (taken in the read
	// of the cluster map that collected its clusters, above), recorded under
	// the version its cluster is published at BEFORE SetSnapshot: SetSnapshot
	// is what lets the proxy see the snapshot, and the proxy's acknowledgement
	// of a cluster is looked up by that version (ClustersAccepted, #1425,
	// #1508). Recorded after, an ACK could arrive first and find nothing.
	//
	// A build moves the acknowledged gauge itself, without an ACK, in two
	// cases: it counts the very bytes the proxy holds differently, or it
	// publishes a version the proxy holds that had no class on record.
	c.publishAckedPins(ctx, pins, snapshot.GetVersionMap(resourcev3.ClusterType), v)

	// Everything SetSnapshot does runs under the cache mutex the ADS stream
	// needs; time it so a regression of the above is visible.
	//
	// It is not given the caller's context (#1620). SetSnapshot stores the
	// snapshot and then hands a response to every watch that was open, and for
	// each one it selects between the watch's channel and the end of the
	// context. With a context that has ALREADY ended both are ready, the choice
	// between them is random, and the first watch that loses it ends the set:
	// that watch and the ones after it are left open and unanswered, the proxy
	// is not sent what the snapshot changed until the next build, and the
	// caller is told the build failed. A caller's context
	// ends for reasons of its own (a CNI ADD whose RPC was abandoned), and what
	// the proxy is sent must not depend on them. So the watches are answered
	// under a context that keeps the caller's values and not its cancellation,
	// bounded by watchAnswerWait.
	setStart := time.Now()
	setCtx, cancelSet := context.WithTimeout(context.WithoutCancel(ctx), c.watchAnswerWait())
	setErr := c.SetSnapshot(setCtx, c.nodeName, snapshot)
	// The responses SetSnapshot built keep setCtx (go-control-plane's
	// Response.GetContext). The pinned server only passes it to the
	// state-of-the-world response callback, and no callback the agent
	// registers acts on that one.
	cancelSet()
	c.metrics.SnapshotSet(ctx, time.Since(setStart).Seconds())
	// An error here is not "the snapshot was not set" (#1549). The pinned
	// go-control-plane (v0.14.0, pkg/cache/v3/simple.go) stores the snapshot
	// as the first thing SetSnapshot does and can return an error only after
	// that, from answering the watches that were open: when the context ends
	// before a watch's channel takes its response. (Its other error, building
	// the version map, cannot happen for a snapshot whose map fillVersionMap
	// already built.) So this snapshot is the one every later request is
	// answered from, and the watches answered before the error were answered
	// from it. What follows is the report every build makes after SetSnapshot,
	// and it runs whatever SetSnapshot returned. Skipped, it was made up for
	// by the next build, under the next build's version.
	//
	// Whether a channel can refuse a response for that long is #1619. With
	// the pinned server's streams it cannot (defaultWatchAnswerTimeout says
	// why, and snapshot_watch_test.go holds it), so the error is not expected
	// from a proxy. It is logged here, under its own message, and returned as
	// ErrWatchNotAnswered: a watch that was open and not answered stays open,
	// and is answered by a later SetSnapshot unless its stream replaces or
	// cancels it first.
	if setErr != nil {
		c.log.WarnContext(ctx, snapshotWatchUnansweredMsg, "snapshot_version", v, "waited", c.watchAnswerWait().String(), "error", setErr)
	}

	// The three reports below are of this snapshot: each is made from what
	// the build read or built, none from the cache's maps as they are now
	// (#1621). A mutator can have changed those since; the build it triggers
	// reports the change, under its own version.
	published := bindings.publishedListeners(listeners)
	// Issue #638 discriminator: name the (source pod → outbound cluster → SDS
	// client-cert secret) bindings of the snapshot just installed, but only
	// the ones that changed — steady state is silent, a re-bind is loud.
	c.logIdentityBindings(ctx, v, bindings.sourceBindings(published), pins.mtls)
	// The inbound counterpart (#638, hypothesis inverted): ssl_fail_verify_san
	// is the CLIENT rejecting the SERVER's certificate, so the mis-bound
	// identity in a #638 event belongs to an inbound filter chain of the proxy
	// that TERMINATED the connection — which #686's client-side check cannot
	// see.
	c.logInboundIdentityBindings(ctx, v, bindings.inboundBindings(published, secrets))
	// The third identity fact a snapshot can get wrong silently (#832): a
	// cluster published with NO server-identity SAN pin. The two checks above
	// ask "is the identity we present the right one"; this one asks "are we
	// checking the identity we are handed at all".
	c.reportClusterPins(ctx, v, pins)

	if setErr != nil {
		return fmt.Errorf("snapshot %s is installed, but %w within %s: %w", v, ErrWatchNotAnswered, c.watchAnswerWait(), setErr)
	}
	return nil
}

// ErrWatchNotAnswered is what a snapshot build returns when the snapshot is
// INSTALLED, and is the one every later request is answered from, but handing
// its changes to a watch that was open did not finish in watchAnswerWait
// (generateSnapshot says when). The cache's state and its reports are those of
// a build that returned nil; what differs is that a proxy may not have been
// sent the change yet. Every mutator that returns a build's error returns this
// one wrapped, so a caller can tell it from a snapshot that was not built:
//
//	if err := c.AddPod(ctx, pod, td); err != nil && !errors.Is(err, cache.ErrWatchNotAnswered) { ... }
var ErrWatchNotAnswered = errors.New("an open watch was not answered from it")

// snapshotInstalled reports whether a build that returned err installed its
// snapshot: it returned nil, or ErrWatchNotAnswered. For the callers inside the
// package that only log a failed build; generateSnapshot has already logged the
// unanswered watch.
func snapshotInstalled(err error) bool {
	return err == nil || errors.Is(err, ErrWatchNotAnswered)
}

// defaultWatchAnswerTimeout bounds how long one SetSnapshot may wait to hand
// its responses to the watches that were open, with snapshotMu held and every
// other build of the node behind it.
//
// It is an escape, not a tuning: it is not expected to elapse (#1619). A
// response is handed over on a channel the stream owns, and the wait is for
// room in it. With the pinned go-control-plane (v0.14.0) there is always room,
// whatever the proxy does, a proxy that stopped reading its stream included:
//
//   - A delta stream (pkg/server/delta/v3) has one channel for all its types,
//     with room for twice the number of resource types the library knows (20).
//     A watch is answered once and is gone; the stream opens the next one for
//     that type only when it handles the proxy's next request for it, and
//     before it handles any request it empties the channel. So the channel
//     holds at most one response per type the stream had a watch open for,
//     plus the answer to the request being handled: seven for the six types
//     the agent serves, eleven for a stream that watched every type.
//   - A state-of-the-world stream (pkg/server/sotw/v3; the per-secret SDS
//     streams) makes a new channel with room for one response for every
//     request, and a watch is answered once. (That is the handler of a server
//     built without the ordered-ADS option, as the agent's is.)
//
// A stream that is blocked writing to a proxy, or a proxy that sends nothing
// more, therefore leaves the build with room for everything it has to hand
// over. If a later go-control-plane changes that, the tests fail, and this is
// what keeps one stream from holding every build of the node for longer.
const defaultWatchAnswerTimeout = 5 * time.Second

// watchAnswerWait is the bound in force. Caller holds snapshotMu.
func (c *SnapshotCache) watchAnswerWait() time.Duration {
	if c.watchAnswerTimeout > 0 {
		return c.watchAnswerTimeout
	}
	return defaultWatchAnswerTimeout
}

// fillVersionMap fills the snapshot's per-resource version map from the memo,
// records what it cost, and reports a memo violation (a published proto
// mutated in place). Caller holds snapshotMu.
func (c *SnapshotCache) fillVersionMap(ctx context.Context, span trace.Span, snapshot *cachev3.Snapshot) error {
	st, err := c.versions.fill(snapshot, time.Now())
	if err != nil {
		return err
	}
	c.metrics.SnapshotVersions(ctx, int64(st.hits), int64(st.hashed), int64(st.mismatchN))
	span.SetAttributes(
		attribute.Int("aether.snapshot.versions_memoized", st.hits),
		attribute.Int("aether.snapshot.versions_hashed", st.hashed),
		attribute.Bool("aether.snapshot.versions_audit", st.audit),
	)
	if st.mismatchN > 0 {
		// Corrected in this snapshot (the audit published the fresh version),
		// but every build between the mutation and this audit served the stale
		// version, so Envoy missed the change for that long. A builder broke
		// the never-mutate-a-published-proto rule; the names say which.
		c.log.ErrorContext(ctx, "xDS resources changed in place after being published; their delta versions were stale until this audit (issue #1105)",
			"count", st.mismatchN, "resources", st.mismatches)
	}
	return nil
}
