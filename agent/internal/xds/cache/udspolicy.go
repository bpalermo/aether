package cache

import (
	"context"
	"maps"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/serviceref"
)

// SetUDSServicePolicies replaces the service-scoped UDS delivery map (proposal 034
// Phase 1b), keyed by "<ns>/<svc>". A pod of a listed service is delivered to the
// declared socket unless it carries its own uds-socket annotation (which wins).
//
// On change the affected pods' delivery clusters are rebuilt and pushed: the
// policy is authored independently of the workload, so nothing else would.
func (c *SnapshotCache) SetUDSServicePolicies(policies map[string]string) {
	c.depMu.Lock()
	changed := !maps.Equal(c.udsServicePolicies, policies)
	c.udsServicePolicies = policies
	// Not read by dependencySetLocked; bumped under the blanket "every depMu
	// write bumps" rule (over-invalidation is harmless).
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if !changed {
		return
	}
	// INFO on change: the in-vivo debugging signal for "the policy never applied".
	c.log.Info("uds service policies updated", "count", len(policies))
	c.regenerateAllAppDeliveryClusters()
}

// udsServicePolicyForPod returns the socket the service-scoped policy declares for
// the pod's own service, or "" when none. The pod's service key is the
// namespace-qualified ServiceAccount key every other Service-attached policy in the
// tree resolves against (020 Part 1).
func (c *SnapshotCache) udsServicePolicyForPod(cniPod *cniv1.CNIPod) string {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	if len(c.udsServicePolicies) == 0 {
		return ""
	}
	return c.udsServicePolicies[serviceref.New(cniPod.GetNamespace(), cniPod.GetServiceAccount()).Key()]
}

// regenerateAllAppDeliveryClusters rebuilds every cached pod's app + health
// clusters and pushes the result. Only the delivery address can have changed, so
// the pod's listeners (inbound/outbound/capture) are left untouched — a pod whose
// delivery is unaffected re-renders byte-identical clusters.
func (c *SnapshotCache) regenerateAllAppDeliveryClusters() {
	ctx := context.Background()
	c.listenerMu.Lock()
	for netns, entry := range c.listeners {
		if entry.cniPod == nil {
			continue
		}
		appClusters, healthCluster := proxy.NewAppDeliveryClusters(entry.cniPod, c.udsSocketPathForPod(ctx, entry.cniPod))
		entry.appClusters = clustersToResources(appClusters)
		entry.healthCluster = healthCluster
		c.listeners[netns] = entry
	}
	c.listenerMu.Unlock()
	// Push immediately: delivery clusters ride the listener snapshot, and nothing
	// in the node dependency set changed, so there is no refresher signal to wait on.
	if err := c.generateListenerSnapshot(ctx); err != nil {
		c.log.Error("failed to regenerate snapshot after uds policy change", "error", err)
	}
}
