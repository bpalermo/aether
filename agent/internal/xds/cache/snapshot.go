package cache

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

// snapshotVersionLabel identifies the unified snapshot in version strings.
const snapshotVersionLabel = "snapshot"

// generateSnapshot assembles a single, consistent xDS snapshot from every cached
// resource type — listeners, clusters, endpoints, the outbound route config and
// secrets — and sets it for the node.
//
// All cache mutations must funnel through here. go-control-plane's SetSnapshot
// replaces the entire node snapshot, so emitting a partial snapshot for one
// resource type (e.g. only listeners) drops every other type from Envoy — which
// makes listener, cluster and secret updates clobber one another. Resources are
// read from the in-memory maps (nothing is rebuilt), so a full snapshot is cheap.
func (c *SnapshotCache) generateSnapshot(ctx context.Context) error {
	v := generateSnapshotVersion(snapshotVersionLabel, c.version)

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

	resources := map[resourcev3.Type][]types.Resource{
		resourcev3.ListenerType: listeners,
		resourcev3.ClusterType:  clusters,
		resourcev3.EndpointType: endpoints,
		resourcev3.SecretType:   secrets,
	}
	if len(vhosts) > 0 {
		resources[resourcev3.RouteType] = []types.Resource{proxy.BuildOutboundRouteConfiguration(vhosts)}
	}

	c.log.V(1).Info("setting snapshot", "version", v,
		"listeners", len(listeners), "clusters", len(clusters),
		"endpoints", len(endpoints), "vhosts", len(vhosts), "secrets", len(secrets))

	snapshot, err := cachev3.NewSnapshot(v, resources)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if err := c.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	return nil
}
