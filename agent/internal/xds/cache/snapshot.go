package cache

import (
	"context"
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

// generateSnapshot assembles a single, consistent xDS snapshot from every cached
// resource type — listeners, clusters, endpoints, the outbound route config and
// secrets — and sets it for the node.
//
// All cache mutations must funnel through here. go-control-plane's SetSnapshot
// replaces the entire node snapshot, so emitting a partial snapshot for one
// resource type (e.g. only listeners) drops every other type from Envoy — which
// makes listener, cluster and secret updates clobber one another. Resources are
// read from the in-memory maps (nothing is rebuilt), so a full snapshot is cheap.
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

	listeners := c.Listeners()
	clusters, endpoints, vhosts := c.clustersEndpointsAndVhosts()

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
	clusters = append(clusters, c.captureTCPClusters()...)

	// Edge L4 TCP clusters (proposal 018, Phase 3b north-south): one per service
	// referenced by an edge TCPRoute or TLSRoute. Like capture TCP clusters but use
	// the edge SPIRE identity and fetch SDS from spire_agent directly.
	if c.edge {
		clusters = append(clusters, c.edgeTCPClusters()...)
		// Cleartext k8s-Service clusters: one per unique non-mesh HTTPRoute backend
		// (BackendNamespace, service, port). STRICT_DNS, no transport socket — the
		// backend is a plain k8s Service, not a mesh-registered endpoint.
		clusters = append(clusters, c.edgeK8sBackendClusters()...)
	}
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
		// main-worker-02, 2026-09-19: LDS at 16:08:50.558Z, first 404 at
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

	snapshot, err := cachev3.NewSnapshot(v, resources)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if err := c.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	// Issue #638 discriminator: name the (source pod → outbound cluster → SDS
	// client-cert secret) bindings this snapshot just handed Envoy, but only
	// the ones that changed — steady state is silent, a re-bind is loud. Runs
	// after SetSnapshot so a logged binding is one Envoy actually received.
	c.logIdentityBindings(ctx, v)
	// The inbound counterpart (#638, hypothesis inverted): ssl_fail_verify_san
	// is the CLIENT rejecting the SERVER's certificate, so the mis-bound
	// identity in a #638 event belongs to an inbound filter chain of the proxy
	// that TERMINATED the connection — which #686's client-side check cannot
	// see.
	c.logInboundIdentityBindings(ctx, v)
	// The third identity fact a snapshot can get wrong silently (#832): a
	// cluster published with NO server-identity SAN pin. The two checks above
	// ask "is the identity we present the right one"; this one asks "are we
	// checking the identity we are handed at all".
	c.reportUnpinnedClusters(ctx, v)

	return nil
}
