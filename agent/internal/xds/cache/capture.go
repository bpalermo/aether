package cache

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/meshdns"
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

// SetMeshDNSServer wires the agent's in-process DNS resolver (proposal 018, mesh-global
// FQDN). When set, the cache opens/closes a per-pod DNS socket in each pod's netns and
// feeds the server the mesh records. nil = mesh DNS off. This replaces the Envoy
// dns_filter, which broke c-ares resolvers (curl/Alpine) by mishandling forwarded
// queries; a real resolver (meshdns) speaks the full protocol.
func (c *SnapshotCache) SetMeshDNSServer(s *meshdns.Server) { c.meshDNS = s }

// SetMeshDNSRecords feeds the in-process resolver the mesh service -> IP table (from
// the mesh-Service reconciler). No-op when mesh DNS is off.
func (c *SnapshotCache) SetMeshDNSRecords(records map[string]string) {
	if c.meshDNS != nil {
		c.meshDNS.SetRecords(records)
	}
}

// addMeshDNS starts serving DNS in the pod's netns; removeMeshDNS stops it. Both are
// no-ops when mesh DNS is off.
func (c *SnapshotCache) addMeshDNS(netns string) {
	if c.meshDNS == nil || netns == "" {
		return
	}
	if err := c.meshDNS.AddNetns(netns); err != nil {
		c.log.Error("failed to serve mesh DNS in pod netns", "netns", netns, "error", err)
	}
}

func (c *SnapshotCache) removeMeshDNS(netns string) {
	if c.meshDNS != nil && netns != "" {
		c.meshDNS.RemoveNetns(netns)
	}
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
