package cache

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// generateCaptureListener builds a pod's transparent-capture listener, or returns
// nil when capture is disabled (so the listenerEntry carries no capture resource).
func (c *SnapshotCache) generateCaptureListener(cniPod *cniv1.CNIPod) (types.Resource, error) {
	if !c.captureEnabled {
		return nil, nil
	}
	l, err := proxy.GenerateCaptureListener(cniPod, constants.ProxyCapturePort, c.meshDomain, c.emitStatsPod)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// SetCaptureEnabled turns transparent capture (proposal 018, Phase 3a) on. Call once
// before the manager starts: it gates per-pod capture listener generation and the
// cap_http route table.
func (c *SnapshotCache) SetCaptureEnabled(v bool) { c.captureEnabled = v }

// SetMeshDNSEnabled turns the per-pod mesh-DNS listener on (proposal 018, mesh-global
// FQDN). SetMeshDNSUpstream sets the resolver(s) the dns_filter forwards non-mesh
// queries to (the cluster kube-dns). Both set once before the manager starts.
func (c *SnapshotCache) SetMeshDNSEnabled(v bool)       { c.meshDNSEnabled = v }
func (c *SnapshotCache) SetMeshDNSUpstream(rs []string) { c.meshDNSUpstream = rs }

// SetMeshDNSRecords replaces the mesh service -> ClusterIP map (fed by the mesh-Service
// reconciler). On change it signals a snapshot rebuild (cap_http) AND rebuilds the
// per-pod DNS listeners: the dns_filter's inline DnsTable is static once a listener is
// built, so a records update (incl. the first one, which arrives AFTER the listeners
// were generated at pod-add) needs a listener regen, or the table stays empty.
func (c *SnapshotCache) SetMeshDNSRecords(records map[string]string) {
	c.captureMu.Lock()
	changed := !equalStringMaps(c.meshDNSRecords, records)
	c.meshDNSRecords = records
	c.captureMu.Unlock()
	if !changed {
		return
	}
	c.signalDependencyChange()
	if c.meshDNSEnabled {
		if err := c.rebuildDNSListeners(context.Background()); err != nil {
			c.log.Error("failed to rebuild mesh-DNS listeners after records change", "error", err)
		}
	}
}

// rebuildDNSListeners regenerates every local pod's mesh-DNS listener from the current
// records and re-emits the listener snapshot. Listeners are generated outside the
// listener lock (generateDNSListener reads dep/capture state), then swapped in.
func (c *SnapshotCache) rebuildDNSListeners(ctx context.Context) error {
	c.listenerMu.RLock()
	pods := make([]*cniv1.CNIPod, 0, len(c.listeners))
	for _, e := range c.listeners {
		if e.cniPod != nil {
			pods = append(pods, e.cniPod)
		}
	}
	c.listenerMu.RUnlock()

	rebuilt := make(map[string]types.Resource, len(pods))
	for _, p := range pods {
		dnsL, err := c.generateDNSListener(p)
		if err != nil {
			return err
		}
		rebuilt[p.GetNetworkNamespace()] = dnsL
	}

	c.listenerMu.Lock()
	for netns, dnsL := range rebuilt {
		if e, ok := c.listeners[netns]; ok {
			e.dnsListener = dnsL
			c.listeners[netns] = e
		}
	}
	c.listenerMu.Unlock()

	return c.generateListenerSnapshot(ctx)
}

// generateDNSListener builds a pod's mesh-DNS listener, or nil when mesh DNS is off.
// The DnsTable answers each in-scope service's <svc>.<meshDomain> with its mesh-Service
// ClusterIP (dep-scoped: a pod resolves only the mesh names it depends on).
func (c *SnapshotCache) generateDNSListener(cniPod *cniv1.CNIPod) (types.Resource, error) {
	if !c.meshDNSEnabled {
		return nil, nil
	}
	l, err := proxy.GenerateDNSListener(cniPod, constants.ProxyDNSCapturePort, c.meshDNSVirtualDomains(), c.meshDNSUpstream)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// meshDNSVirtualDomains builds the DnsTable entries: <svc>.<meshDomain> -> the
// service's mesh-Service ClusterIP, for the WHOLE mesh catalog (not dep-scoped).
// Resolution is cheap and harmless; the actual reachability stays demand-scoped at
// the cluster/EDS/mTLS layer. Answering all names also avoids a bootstrap deadlock —
// a pod must resolve a name to dial it, but it isn't in the dependency set until first
// dialed.
func (c *SnapshotCache) meshDNSVirtualDomains() []proxy.DNSVirtualDomain {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	vds := make([]proxy.DNSVirtualDomain, 0, len(c.meshDNSRecords))
	for svc, clusterIP := range c.meshDNSRecords {
		vds = append(vds, proxy.DNSVirtualDomain{
			Domain:    proxy.ServiceClusterName(svc, c.meshDomain),
			Addresses: []string{clusterIP},
		})
	}
	return vds
}

// SetCaptureAuthorities replaces the mesh service -> cluster.local FQDN map (fed by
// the agent's capture reconciler from the generated mesh Services) and signals a
// snapshot rebuild on change so cap_http re-derives.
func (c *SnapshotCache) SetCaptureAuthorities(authorities map[string]string) {
	c.captureMu.Lock()
	changed := !equalStringMaps(c.captureAuthorities, authorities)
	c.captureAuthorities = authorities
	c.captureMu.Unlock()
	if changed {
		c.signalDependencyChange()
	}
}

// captureVhosts builds the cap_http virtual hosts: each in-scope service that has a
// cluster.local authority routes (both the portless and :meshPort spellings) to its
// <svc>.<meshDomain> cluster. Scoped to the dependency set so cap_http only references
// clusters the scoped snapshot carries; unknown authorities hit the 404 default.
func (c *SnapshotCache) captureVhosts() []*routev3.VirtualHost {
	deps := c.DependencySet()

	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	vhosts := make([]*routev3.VirtualHost, 0, len(c.captureAuthorities))
	for svc, fqdn := range c.captureAuthorities {
		if _, ok := deps[svc]; !ok {
			continue
		}
		mesh := proxy.ServiceClusterName(svc, c.meshDomain)
		// Route both the cluster.local authority and the mesh-global <svc>.<meshDomain>
		// authority (portless + :meshPort) to the service cluster, so a captured
		// request reaches it under either name (the mesh-DNS path uses the latter).
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(mesh, []string{
			fqdn, fmt.Sprintf("%s:%d", fqdn, constants.ProxyOutboundPort),
			mesh, fmt.Sprintf("%s:%d", mesh, constants.ProxyOutboundPort),
		}))
	}
	return vhosts
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
