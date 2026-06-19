package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/common/telemetry"
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

	v := generateSnapshotVersion(snapshotVersionLabel, c.version)

	ctx, span := otel.Tracer(tracerName).Start(ctx, "agent.snapshot.generate",
		trace.WithAttributes(telemetry.AttrSnapshotVersion.String(v)))
	defer func() { telemetry.EndSpan(span, retErr) }()
	start := time.Now()
	defer func() {
		c.metrics.generated(ctx, time.Since(start).Seconds(), int64(c.version.Load()), retErr)
	}()

	listeners := c.Listeners()
	clusters, endpoints, vhosts := c.clustersEndpointsAndVhosts()

	// Per-pod application clusters live alongside listeners (not in the
	// registry-driven cluster map) so registry reloads never drop them. STATIC
	// clusters carry their endpoints inline, so they need no EDS resources.
	clusters = append(clusters, c.appClusters()...)

	c.secretMu.RLock()
	secrets := make([]types.Resource, 0, len(c.secrets))
	for _, s := range c.secrets {
		secrets = append(secrets, s)
	}
	c.secretMu.RUnlock()

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
		// The edge always serves a route config (even with zero exposed
		// services) so the listener's RDS reference resolves; the catch-all
		// 404 vhost handles unmatched authorities. No ODCDS catch-all.
		resources[resourcev3.RouteType] = []types.Resource{proxy.BuildEdgeRouteConfiguration(vhosts)}
	} else if len(vhosts) > 0 {
		resources[resourcev3.RouteType] = []types.Resource{proxy.BuildOutboundRouteConfiguration(vhosts, c.meshDomain)}
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

	c.depMu.RLock()
	declared := c.declaredCountLocked()
	observed := c.observedCountLocked()
	c.depMu.RUnlock()
	c.metrics.snapshotShape(ctx, len(clusters), declared, observed)

	snapshot, err := cachev3.NewSnapshot(v, resources)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if err := c.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	return nil
}
