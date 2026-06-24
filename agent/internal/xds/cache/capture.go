package cache

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/capture"
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
	c.captureMu.RLock()
	tcpRoutes := c.tcpServiceRoutesSnapshot()
	tlsRoutes := c.tlsServiceRoutesSnapshot()
	tcpServices := make([]proxy.CaptureTCPService, len(c.captureTCPServices))
	for i, e := range c.captureTCPServices {
		tcpServices[i] = proxy.CaptureTCPService{
			// TCP clusters are separate from HTTP clusters: they share the same EDS
			// resource (same endpoint set) but use no ALPN on the transport socket so
			// the destination inbound demuxes to the TCP floor DEFAULT chain.
			ClusterName: proxy.TCPClusterName(e.serviceName, c.meshDomain),
			ClusterIP:   e.clusterIP,
			// L4 route rules (Phase 3b): override the passthrough floor chain when
			// a TCPRoute or TLSRoute is attached to this service.
			TCPRouteRules: tcpRoutes[e.serviceName],
			TLSRouteRules: tlsRoutes[e.serviceName],
		}
	}
	c.captureMu.RUnlock()

	l, err := proxy.GenerateCaptureListener(cniPod, constants.ProxyCapturePort, c.meshDomain, c.emitStatsPod, tcpServices)
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
// FQDN), or nil when mesh DNS is off. The CNI DNATs each pod's :53 directly to the
// resolver's host listener, so the cache only feeds it the mesh records — no Envoy
// DNS listeners or cluster.
func (c *SnapshotCache) SetMeshDNSServer(s *meshdns.Server) { c.meshDNS = s }

// SetMeshDNSRecords feeds the in-process resolver the mesh service -> IP table (from
// the mesh-Service reconciler). No-op when mesh DNS is off.
func (c *SnapshotCache) SetMeshDNSRecords(records map[string]string) {
	if c.meshDNS != nil {
		c.meshDNS.SetRecords(records)
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

// SetCaptureTCPServices implements capture.AuthoritySink: it replaces the list of
// non-HTTP mesh services that need per-ClusterIP TCP floor chains on the capture
// listener. A change rebuilds all per-pod capture listeners (they embed the TCP chains)
// and regenerates the xDS snapshot so Envoy picks up the new filter chains.
func (c *SnapshotCache) SetCaptureTCPServices(services []capture.CaptureTCPService) {
	entries := make([]captureTCPEntry, 0, len(services))
	for _, s := range services {
		if s.ServiceName != "" && s.ClusterIP != "" {
			entries = append(entries, captureTCPEntry{
				serviceName: s.ServiceName,
				clusterIP:   s.ClusterIP,
			})
		}
	}

	c.captureMu.Lock()
	changed := !equalTCPEntries(c.captureTCPServices, entries)
	c.captureTCPServices = entries
	c.captureMu.Unlock()

	if !changed {
		return
	}

	// Per-pod capture listeners embed TCP floor chains; regenerate all of them.
	if c.captureEnabled {
		c.listenerMu.Lock()
		for netns, entry := range c.listeners {
			if entry.cniPod == nil {
				continue
			}
			newCapture, err := c.generateCaptureListener(entry.cniPod)
			if err != nil {
				c.log.Error("failed to regenerate capture listener on TCP-services change",
					"netns", netns, "pod", entry.cniPod.GetName(), "error", err)
				continue
			}
			entry.capture = newCapture
			c.listeners[netns] = entry
		}
		c.listenerMu.Unlock()
	}

	// Signal dependency change to trigger cluster snapshot rebuild (TCP clusters
	// are also rebuilt from captureTCPServices) and a full snapshot push to Envoy.
	c.signalDependencyChange()
}

// captureTCPClusters returns the TCP floor clusters for non-HTTP services as a resource
// slice. These are separate EDS clusters (prefixed "tcp:") that share the same endpoints
// as the HTTP clusters but use NO ALPN on their transport socket so the destination
// inbound routes to the TCP floor DEFAULT chain. Called from generateSnapshot.
func (c *SnapshotCache) captureTCPClusters() []types.Resource {
	if !c.captureEnabled {
		return nil
	}

	c.captureMu.RLock()
	entries := make([]captureTCPEntry, len(c.captureTCPServices))
	copy(entries, c.captureTCPServices)
	c.captureMu.RUnlock()

	if len(entries) == 0 {
		return nil
	}

	c.localMu.RLock()
	netnsToID := make(map[string]string, len(c.localWorkloads))
	ids := make([]string, 0, len(c.localWorkloads))
	for netns, id := range c.localWorkloads {
		netnsToID[netns] = id
		ids = append(ids, id)
	}
	nodeSpiffeID := c.nodeSpiffeID
	trustDomain := c.trustDomain
	c.localMu.RUnlock()

	if nodeSpiffeID == "" {
		// Node SVID not yet available; skip TCP clusters until it is.
		return nil
	}

	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)

	// For TCP services, retrieve their SAN namespaces from the cluster map (the HTTP
	// cluster for the same service is guaranteed to exist if the service is in scope).
	c.clusterMu.RLock()
	resources := make([]types.Resource, 0, len(entries))
	for _, e := range entries {
		httpEntry, ok := c.clusters[e.serviceName]
		if !ok {
			// Service not yet in scope; skip until cluster map has it.
			continue
		}
		sanURIs := make([]string, 0, len(httpEntry.sanNamespaces))
		for _, ns := range httpEntry.sanNamespaces {
			sanURIs = append(sanURIs, fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, ns, httpEntry.service))
		}
		tcpName := proxy.TCPClusterName(e.serviceName, c.meshDomain)
		cl := proxy.NewTCPServiceCluster(tcpName, e.serviceName, e.serviceName, c.perDownstreamConnectionPool())
		// NO SNI for the TCP floor: the egress floor connection must NOT carry the
		// destination port as SNI, or the peer's inbound per-port HCM chain
		// (server_names:[port]) would win over the inbound TCP floor's default chain
		// (server_names > application_protocols > default) — the connection lands on
		// the HCM, which can't parse the raw TCP stream and 503s. An empty SNI lets it
		// fall through to the inbound default floor chain (tcp_proxy to the app).
		proxy.InjectUpstreamTCPMTLS(cl, netnsToID, ids, nodeSpiffeID, validationContextName, sanURIs, "")
		resources = append(resources, cl)
	}
	c.clusterMu.RUnlock()

	return resources
}

// edgeTCPClusters returns TCP floor clusters for services referenced by the edge's
// L4 routes (TCPRoute/TLSRoute parented to a Gateway). Called from generateSnapshot
// when in edge mode; parallel to captureTCPClusters but for the edge identity
// (EdgeUpstreamTCPTransportSocket fetches SDS from spire_agent, not ADS).
//
// The edge has one identity so no per-source matcher is needed: the cluster gets
// a single transport socket presenting the edge SVID with no ALPN and no SNI,
// so the destination inbound demuxes to the TCP floor DEFAULT chain.
func (c *SnapshotCache) edgeTCPClusters() []types.Resource {
	c.edgeMu.RLock()
	tcpRoutes := append([]proxy.EdgeL4TCPRoute(nil), c.edgeTCPRoutes...)
	tlsRoutes := append([]proxy.EdgeL4TLSRoute(nil), c.edgeTLSRoutes...)
	c.edgeMu.RUnlock()

	// Collect the distinct service names referenced by edge L4 routes.
	seen := make(map[string]struct{})
	var services []string
	for _, r := range tcpRoutes {
		for _, b := range r.Backends {
			if b.Service != "" && b.Cluster != "" {
				if _, ok := seen[b.Service]; !ok {
					seen[b.Service] = struct{}{}
					services = append(services, b.Service)
				}
			}
		}
	}
	for _, r := range tlsRoutes {
		for _, rule := range r.Rules {
			for _, b := range rule.Backends {
				if b.Service != "" && b.Cluster != "" {
					if _, ok := seen[b.Service]; !ok {
						seen[b.Service] = struct{}{}
						services = append(services, b.Service)
					}
				}
			}
		}
	}
	if len(services) == 0 {
		return nil
	}

	c.localMu.RLock()
	nodeSpiffeID := c.nodeSpiffeID
	trustDomain := c.trustDomain
	c.localMu.RUnlock()
	if nodeSpiffeID == "" {
		return nil
	}
	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)

	c.clusterMu.RLock()
	resources := make([]types.Resource, 0, len(services))
	for _, svc := range services {
		entry, ok := c.clusters[svc]
		if !ok {
			continue // not yet in scope; will appear when registry delivers endpoints
		}
		sanURIs := make([]string, 0, len(entry.sanNamespaces))
		for _, ns := range entry.sanNamespaces {
			sanURIs = append(sanURIs, fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, ns, entry.service))
		}
		tcpName := proxy.TCPClusterName(svc, c.meshDomain)
		cl := proxy.NewTCPServiceCluster(tcpName, svc, svc, false /* edge = single identity, no per-downstream pool */)
		// Edge variant: fetch SVID/bundle from spire_agent (not ADS), no ALPN, no SNI (TCP floor).
		cl.TransportSocket = proxy.EdgeUpstreamTCPTransportSocket(nodeSpiffeID, validationContextName, sanURIs)
		resources = append(resources, cl)
	}
	c.clusterMu.RUnlock()
	return resources
}

// captureVhosts builds the cap_http virtual hosts: each in-scope service that has a
// cluster.local authority routes (both the portless and :meshPort spellings) to its
// <svc>.<meshDomain> cluster. Scoped to the dependency set so cap_http only references
// clusters the scoped snapshot carries; unknown authorities hit the 404 default.
func (c *SnapshotCache) captureVhosts() []*routev3.VirtualHost {
	deps := c.DependencySet()
	// GAMMA (HTTPRoute/GRPCRoute) rules enrich the captured path too — capture is the
	// default client path (mesh-DNS), so the same L7 vocabulary the outbound listener
	// applies must apply here; no rules = passthrough to the service cluster.
	gammaRoutes := c.serviceRoutesSnapshot()

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
		vhosts = append(vhosts, proxy.BuildOutboundServiceVirtualHost(mesh, []string{
			fqdn, fmt.Sprintf("%s:%d", fqdn, constants.ProxyOutboundPort),
			mesh, fmt.Sprintf("%s:%d", mesh, constants.ProxyOutboundPort),
		}, gammaRoutes[svc]))
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

// equalTCPEntries reports whether two captureTCPEntry slices are identical by
// content (order-independent). Used to gate signalDependencyChange.
func equalTCPEntries(a, b []captureTCPEntry) bool {
	if len(a) != len(b) {
		return false
	}
	ma := make(map[string]string, len(a))
	for _, e := range a {
		ma[e.serviceName] = e.clusterIP
	}
	for _, e := range b {
		if ma[e.serviceName] != e.clusterIP {
			return false
		}
	}
	return true
}
