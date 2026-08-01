package cache

import (
	"sort"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/common/serviceref"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// localServicePodsLocked groups this node's local pods by their service FQDN and
// returns each service's sorted pod IPs. The caller must hold listenerMu (read).
// Used to build the east/west tunnel listener chains and the ew_ingress clusters
// (proposal 019 Phase 3b) — both are scoped to the services this node HOSTS.
func (c *SnapshotCache) localServicePodsLocked() map[string][]string {
	byFQDN := make(map[string][]string)
	for _, entry := range c.listeners {
		pod := entry.cniPod
		if pod == nil || pod.GetServiceAccount() == "" || pod.GetNamespace() == "" {
			continue
		}
		ips := pod.GetIps()
		if len(ips) == 0 {
			continue
		}
		fqdn := proxy.ServiceClusterName(serviceref.New(pod.GetNamespace(), pod.GetServiceAccount()).Key(), c.meshDomain)
		byFQDN[fqdn] = append(byFQDN[fqdn], ips[0])
	}
	for fqdn := range byFQDN {
		sort.Strings(byFQDN[fqdn])
	}
	return byFQDN
}

// waypointTunnelListenerLocked builds the node's host-netns east/west tunnel
// listener from the services hosted on this node (proposal 019 Phase 3b), or nil
// when the waypoint is disabled or the node hosts no mesh pods. Caller holds
// listenerMu (read).
func (c *SnapshotCache) waypointTunnelListenerLocked() types.Resource {
	if !c.waypointEnabled {
		return nil
	}
	byFQDN := c.localServicePodsLocked()
	fqdns := make([]string, 0, len(byFQDN))
	for fqdn := range byFQDN {
		fqdns = append(fqdns, fqdn)
	}
	sort.Strings(fqdns)

	chains := make([]*listenerv3.FilterChain, 0, len(fqdns))
	for _, fqdn := range fqdns {
		chains = append(chains, proxy.BuildWaypointTunnelChain(fqdn))
	}
	ln := proxy.BuildWaypointTunnelListener(c.waypointTunnelPort, chains)
	if ln == nil {
		return nil
	}
	return ln
}

// ewIngressClusters returns the STATIC clusters (one per hosted service) of this
// node's local pods at the mesh inbound port, that the tunnel forwards to
// (proposal 019 Phase 3b). Empty when the waypoint is disabled.
func (c *SnapshotCache) ewIngressClusters() []types.Resource {
	if !c.waypointEnabled {
		return nil
	}
	c.listenerMu.RLock()
	byFQDN := c.localServicePodsLocked()
	c.listenerMu.RUnlock()

	fqdns := make([]string, 0, len(byFQDN))
	for fqdn := range byFQDN {
		fqdns = append(fqdns, fqdn)
	}
	sort.Strings(fqdns)

	out := make([]types.Resource, 0, len(fqdns))
	for _, fqdn := range fqdns {
		out = append(out, proxy.BuildWaypointIngressCluster(fqdn, byFQDN[fqdn]))
	}
	return out
}
