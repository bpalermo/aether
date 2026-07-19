package cache

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bpalermo/aether/common/serviceref"

	"github.com/bpalermo/aether/agent/internal/capture"
	"github.com/bpalermo/aether/agent/internal/meshdns"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	meshconst "github.com/bpalermo/aether/common/constants/mesh"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// generateUDPCaptureListener builds a pod's per-pod UDP capture listener, or
// returns nil when capture is disabled or no UDPRoute backends are in scope.
// The listener is generated from the current udpServiceRoutes snapshot.
func (c *SnapshotCache) generateUDPCaptureListener(cniPod *cniv1.CNIPod) (types.Resource, error) {
	if !c.captureEnabled {
		return nil, nil
	}
	udpRoutes := c.udpServiceRoutesSnapshot()
	l, err := proxy.GenerateUDPCaptureListener(
		cniPod.GetName(),
		cniPod.GetNetworkNamespace(),
		meshconst.ProxyCapturePort,
		udpRoutes,
	)
	if err != nil {
		return nil, err
	}
	if l == nil {
		// No UDPRoute backends in scope: GenerateUDPCaptureListener returns a nil
		// *Listener. Return an untyped nil interface — NOT the typed-nil pointer.
		// Returning the *listenerv3.Listener directly would wrap a nil pointer in a
		// non-nil types.Resource interface, defeating the `!= nil` guard in
		// Listeners(); the empty-Name/nil-Address Listener then lands in the LDS
		// snapshot and Envoy NACKs the whole push with "address is necessary".
		return nil, nil
	}
	return l, nil
}

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

	// Escape-hatch extension filters (proposal 025): the HCM must carry every
	// allow-listed filter any in-scope route references, default-disabled, so the
	// route's typed_per_filter_config can re-enable it (per-route config only
	// overrides a chain filter, never adds one). Union over all GammaRoutes; disabled
	// entries are inert, so the (broader-than-per-pod) union is harmless.
	extensionFilters := c.podExtensionHTTPFilters(cniPod)

	l, err := proxy.GenerateCaptureListener(cniPod, meshconst.ProxyCapturePort, c.meshDomain, c.emitStatsPod, tcpServices, c.captureRedirectAll, extensionFilters)
	if err != nil {
		return nil, err
	}
	if l == nil {
		// Defensive: never wrap a nil *Listener in a non-nil types.Resource (see the
		// note in generateUDPCaptureListener). GenerateCaptureListener does not
		// currently return (nil, nil), but guarding here keeps the typed-nil out of
		// the snapshot if that ever changes.
		return nil, nil
	}
	return l, nil
}

// SetCaptureEnabled turns transparent capture (proposal 018, Phase 3a) on. Call once
// before the manager starts: it gates per-pod capture listener generation and the
// cap_http route table.
func (c *SnapshotCache) SetCaptureEnabled(v bool) { c.captureEnabled = v }

// SetCaptureRedirectAll enables the redirect-all + ORIGINAL_DST passthrough mode
// (proposal 022, M2a spike). Must be called after SetCaptureEnabled(true): redirect-all
// only makes sense when the capture listener is being generated. Call once before the
// manager starts; read without locking.
func (c *SnapshotCache) SetCaptureRedirectAll(v bool) { c.captureRedirectAll = v }

// capturePassthroughCluster returns the ORIGINAL_DST passthrough cluster when
// redirect-all capture is enabled, or nil otherwise. The cluster is emitted into
// the CDS snapshot so Envoy can resolve the "passthrough_original_dst" reference
// from the capture listener's DefaultFilterChain.
func (c *SnapshotCache) capturePassthroughCluster() types.Resource {
	if !c.captureEnabled || !c.captureRedirectAll {
		return nil
	}
	return proxy.NewPassthroughOriginalDstCluster()
}

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
	if changed {
		// Invalidate the routeDomains memo: the SA-backed fqdns feed the
		// per-route-target domain lists (content-equal replacement leaves the
		// memo exact, so no bump then).
		c.captureAuthGen++
	}
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

	// Mirror the service names into dependency state (depMu): every TCP mesh
	// service joins the node dependency set so its endpoints + tcp: cluster are
	// always delivered alongside its (unconditional) capture floor chain — a
	// chain without its cluster kills every connection silently, and tcp_proxy
	// has no ODCDS cold path to recover.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.serviceName)
	}
	c.depMu.Lock()
	c.captureTCPDeps = names
	c.bumpDepGenLocked()
	c.depMu.Unlock()

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
		// The service's expected server SPIFFE IDs are precomputed on the entry
		// (refreshEntryMTLSLocked, issue #537) from its endpoints' namespaces
		// and the BARE service name for the sa/ segment — the same pinning the
		// HTTP cluster path uses.
		sanURIs := httpEntry.sanURIs
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
	services := c.collectEdgeTCPServices()
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
		// SAN pinning precomputed on the entry (refreshEntryMTLSLocked, issue
		// #537); see the node-proxy TCP variant above.
		sanURIs := entry.sanURIs
		tcpName := proxy.TCPClusterName(svc, c.meshDomain)
		cl := proxy.NewTCPServiceCluster(tcpName, svc, svc, false /* edge = single identity, no per-downstream pool */)
		// Edge variant: fetch SVID/bundle from spire_agent (not ADS), no ALPN, no SNI (TCP floor).
		cl.TransportSocket = proxy.EdgeUpstreamTCPTransportSocket(nodeSpiffeID, validationContextName, sanURIs)
		resources = append(resources, cl)
	}
	c.clusterMu.RUnlock()

	return resources
}

// collectEdgeTCPServices returns the distinct service names (with a Cluster name
// set) referenced by the edge's TCP and TLS L4 routes.
func (c *SnapshotCache) collectEdgeTCPServices() []string {
	c.edgeMu.RLock()
	tcpRoutes := append([]proxy.EdgeL4TCPRoute(nil), c.edgeTCPRoutes...)
	tlsRoutes := append([]proxy.EdgeL4TLSRoute(nil), c.edgeTLSRoutes...)
	c.edgeMu.RUnlock()

	seen := make(map[string]struct{})
	var services []string
	addSvc := func(svc, cluster string) {
		if svc == "" || cluster == "" {
			return
		}
		if _, ok := seen[svc]; !ok {
			seen[svc] = struct{}{}
			services = append(services, svc)
		}
	}
	for _, r := range tcpRoutes {
		for _, b := range r.Backends {
			addSvc(b.Service, b.Cluster)
		}
	}
	for _, r := range tlsRoutes {
		for _, rule := range r.Rules {
			for _, b := range rule.Backends {
				addSvc(b.Service, b.Cluster)
			}
		}
	}
	return services
}

// captureUDPClusters returns the UDP floor clusters for services with UDPRoute
// backends as a resource slice. Each in-scope service that has at least one UDP
// backend emits a "udp:<svc>.<domain>" EDS cluster — a plain EDS cluster with
// no transport socket, since UDP traffic is not covered by mesh mTLS.
//
// SECURITY NOTE: these clusters forward datagrams in plaintext. This is a known
// limitation of the UDP floor (proposal 018 Phase 3b).
func (c *SnapshotCache) captureUDPClusters() []types.Resource {
	if !c.captureEnabled {
		return nil
	}

	udpRoutes := c.udpServiceRoutesSnapshot()
	if len(udpRoutes) == 0 {
		return nil
	}

	// Collect unique service names from the UDP route backends.
	services := make(map[string]struct{}, len(udpRoutes))
	for _, backends := range udpRoutes {
		for _, b := range backends {
			if b.Service != "" {
				services[b.Service] = struct{}{}
			}
		}
	}

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	resources := make([]types.Resource, 0, len(services))
	for svc := range services {
		entry, ok := c.clusters[svc]
		if !ok || entry.loadAssignment == nil {
			// Backend service not in scope yet; skip until its cluster/EDS exists.
			continue
		}
		// entry.sni carries the backend's registered application port. The UDP floor
		// has no inbound mTLS hop, so udp_proxy must reach that app port directly (not
		// the mesh inbound :18008 the shared bare-name EDS carries) — build an inline
		// app-port UDP load assignment from the service's endpoints.
		port, err := strconv.Atoi(entry.sni)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		udpName := proxy.UDPClusterName(svc, c.meshDomain)
		la := proxy.UDPLoadAssignment(entry.loadAssignment, udpName, uint32(port))
		cl := proxy.NewUDPServiceCluster(udpName, svc, la)
		resources = append(resources, cl)
	}
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
	// applies must apply here; no rules = passthrough to the service cluster. The
	// per-route-target domain lists are read from the same-generation memo (issue
	// #540); they fold in the real Service port(s) (proposal 023 M2) and the
	// bare-name uniqueness guard.
	gammaRoutes, routeDomains := c.serviceRoutesAndDomainsSnapshot()
	// Service-wide always-on extension filters (025 M4 CHAIN scope), vhost-enabled.
	chainFilters := c.serviceChainFiltersSnapshot()

	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	vhosts := make([]*routev3.VirtualHost, 0, len(c.captureAuthorities)+len(gammaRoutes))
	vhosts = c.appendSABackedCaptureVhosts(vhosts, deps, gammaRoutes, routeDomains, chainFilters)
	vhosts = c.appendRouteOnlyCaptureVhosts(vhosts, deps, gammaRoutes, routeDomains, chainFilters)
	return vhosts
}

// appendSABackedCaptureVhosts appends cap_http virtual hosts for services that have
// SA-backed mesh authorities (captureAuthorities). Caller must hold captureMu.
func (c *SnapshotCache) appendSABackedCaptureVhosts(vhosts []*routev3.VirtualHost, deps map[string]struct{}, gammaRoutes map[string][]proxy.GammaRoute, routeDomains map[string][]string, chainFilters map[string]proxy.ExtensionFilter) []*routev3.VirtualHost {
	for svc, fqdn := range c.captureAuthorities {
		if _, ok := deps[svc]; !ok {
			continue
		}
		mesh := proxy.ServiceClusterName(svc, c.meshDomain)
		rules := gammaRoutes[svc]
		domains := c.captureVhostDomains(svc, fqdn, mesh, rules, routeDomains)
		vh := proxy.BuildOutboundServiceVirtualHost(mesh, domains, rules)
		applyChainFilter(vh, chainFilters, svc)
		vhosts = append(vhosts, vh)
	}
	return vhosts
}

// captureVhostDomains returns the host-match domain list for an SA-backed service's
// cap_http virtual host. When the service carries GAMMA rules the full short-name +
// real-port domain set is used; otherwise only the cluster.local + mesh spellings.
func (c *SnapshotCache) captureVhostDomains(svc, fqdn, mesh string, rules []proxy.GammaRoute, routeDomains map[string][]string) []string {
	if len(rules) > 0 {
		domains := routeDomains[svc]
		if len(domains) == 0 {
			// Defensive: a route target with rules always has a memo entry
			// unless its "<ns>/<svc>" key is unparseable AND it raced past a
			// captureAuthorities swap. ParseKey failing means the short-name
			// spellings (and bareNameCount/ports) are unused, so this inline
			// render matches what the memo would have built.
			return routeTargetDomains(svc, fqdn, mesh, nil, nil)
		}
		return domains
	}
	// Route both the cluster.local authority and the mesh-global
	// <svc>.<meshDomain> authority (portless + :meshPort) to the service
	// cluster, so a captured request reaches it under either name (the
	// mesh-DNS path uses the latter).
	return []string{
		fqdn, fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyOutboundPort),
		mesh, fmt.Sprintf("%s:%d", mesh, meshconst.ProxyOutboundPort),
	}
}

// appendRouteOnlyCaptureVhosts appends cap_http virtual hosts for GAMMA route
// targets that have no SA-backed mesh Service of their own (the versioned-fanout
// shape). Caller must hold captureMu.
func (c *SnapshotCache) appendRouteOnlyCaptureVhosts(vhosts []*routev3.VirtualHost, deps map[string]struct{}, gammaRoutes map[string][]proxy.GammaRoute, routeDomains map[string][]string, chainFilters map[string]proxy.ExtensionFilter) []*routev3.VirtualHost {
	// Service-based routing (proposal 023): a GAMMA route TARGET with no SA-backed
	// mesh Service of its own (the versioned-fanout shape — an "echo" target routed
	// to echo-v1/echo-v2) still needs a cap_http vhost. Its cluster.local authority
	// is derived from the namespace-qualified key, and the GAMMA rules route to the
	// backendRef (SA-backed) clusters. Skip targets already handled as SA-backed
	// authorities above (a Service that is both a target and its own backend).
	for svc := range gammaRoutes {
		if _, ok := c.captureAuthorities[svc]; ok {
			continue
		}
		if _, ok := deps[svc]; !ok {
			continue
		}
		domains := routeDomains[svc]
		if len(domains) == 0 {
			// No memo entry: the "<ns>/<svc>" key is unparseable (no cluster.local
			// authority can be derived) — skipped before the memo existed too.
			continue
		}
		mesh := proxy.ServiceClusterName(svc, c.meshDomain)
		vh := proxy.BuildOutboundServiceVirtualHost(mesh, domains, gammaRoutes[svc])
		applyChainFilter(vh, chainFilters, svc)
		vhosts = append(vhosts, vh)
	}
	return vhosts
}

// captureKnownTargets builds the redirect-all catch-all's known-target safety net
// (proposal 022 hardening): one route-target per in-scope mesh authority, pinning
// all of that authority's non-mesh cluster.local dial spellings (on any port) to
// its mesh cluster. The catch-all consults these BEFORE the ORIGINAL_DST
// passthrough, so a captured request to a service the mesh knows about can never
// leak to kube-proxy — including the window where the service's dedicated cap_http
// vhost is mid-rebuild across a GAMMA HTTPRoute add/delete (serviceRoutes churns,
// but captureAuthorities — fed by the generated mesh Services, independent of
// HTTPRoute lifecycle — does not).
//
// Returns nil unless redirect-all capture is on (without a passthrough cluster
// there is nothing to shadow, and the catch-all keeps its hard 404).
//
// Gating: the safety net is keyed on the STABLE known-mesh-service signals —
// every captureAuthorities entry (a generated mesh Service exists ⇒ the service
// is real and meshed; this map is fed by the mesh-Service reconciler and is
// untouched by HTTPRoute churn) plus every current GAMMA route target. It is
// deliberately NOT gated on the demand-scoped dependency set: a route target's
// dependency-set membership comes from its (churning) GAMMA rule, so gating on
// deps would re-introduce the very race this fixes — the entry would vanish the
// instant the route is deleted. Routing a known service's captured traffic to its
// mesh cluster fails CLOSED into the mesh (a brief 503 if the demand-scoped cluster
// is momentarily absent — which the conformance harness cleanly retries) rather
// than OPEN to kube-proxy (a silent 200 that drops the GAMMA feature and resets the
// consecutive-success counter). The regex is built once per service over its
// short-name spellings with an optional ":port" suffix, so it matches a dial on the
// real Service port without the control plane tracking it.
func (c *SnapshotCache) captureKnownTargets() []proxy.KnownTargetRoute {
	if !c.captureEnabled || !c.captureRedirectAll {
		return nil
	}
	gammaRoutes := c.serviceRoutesSnapshot()

	c.captureMu.RLock()
	defer c.captureMu.RUnlock()

	seen := make(map[string]struct{}, len(c.captureAuthorities)+len(gammaRoutes))
	targets := make([]proxy.KnownTargetRoute, 0, len(c.captureAuthorities)+len(gammaRoutes))
	add := func(svc, fqdn string) {
		if _, ok := seen[svc]; ok {
			return
		}
		ref, ok := serviceref.ParseKey(svc)
		if !ok {
			return
		}
		seen[svc] = struct{}{}
		targets = append(targets, proxy.KnownTargetRoute{
			AuthorityRegex: knownTargetAuthorityRegex(ref.Name, ref.Namespace, fqdn),
			Cluster:        proxy.ServiceClusterName(svc, c.meshDomain),
		})
	}
	// Every SA-backed mesh authority (covers plain services AND the MeshFrontend
	// shape where a Service is its own GAMMA backend). Stable across HTTPRoute churn.
	for svc, fqdn := range c.captureAuthorities {
		add(svc, fqdn)
	}
	// Route-only GAMMA targets (no SA-backed mesh Service of their own — the
	// versioned-fanout shape): derive the cluster.local authority from the
	// namespace-qualified key. Present only while the route is, but the dedicated
	// vhost is too, so there is no extra window to cover here.
	for svc := range gammaRoutes {
		if _, ok := c.captureAuthorities[svc]; ok {
			continue
		}
		ref, ok := serviceref.ParseKey(svc)
		if !ok {
			continue
		}
		add(svc, ref.ClusterLocalFQDN())
	}
	return targets
}

// knownTargetAuthorityRegex builds the RE2 :authority pattern covering all of a
// service's non-mesh dial spellings with an optional ":port" suffix: the bare
// same-namespace name, <name>.<ns>, <name>.<ns>.svc, and the cluster.local FQDN.
// The mesh spelling (<svc>.<meshDomain>) is intentionally omitted — the catch-all
// already routes mesh-shaped authorities via the ODCDS regex. The bare name is
// always included here (unlike routeTargetDomains' bareNameCount guard) because a
// safe_regex header match cannot collide the way duplicate vhost domains NACK; a
// bare name shared across namespaces simply means both services' regexes match it,
// and either resolves to a real in-scope cluster (never the passthrough), which is
// strictly better than the kube-proxy leak. Port-agnostic so any real Service port
// matches.
func knownTargetAuthorityRegex(name, namespace, fqdn string) string {
	spellings := []string{
		name,
		name + "." + namespace,
		name + "." + namespace + ".svc",
		fqdn,
	}
	quoted := make([]string, 0, len(spellings))
	seen := make(map[string]struct{}, len(spellings))
	for _, s := range spellings {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		quoted = append(quoted, regexp.QuoteMeta(s))
	}
	return "^(" + strings.Join(quoted, "|") + ")(:[0-9]+)?$"
}

// serviceRoutesAndDomainsSnapshot returns the effective stripped GAMMA rules
// together with the per-route-target cap_http domain lists built from that SAME
// route generation, memoized on the two input generations (issue #540): depGen
// (routes + routeTargetPorts + the authz flag) and captureAuthGen
// (captureAuthorities, the SA-backed fqdn source). Previously
// routeTargetDomains rebuilt every list via fmt.Sprintf in nested loops (base
// names × ports) on EVERY snapshot; now the snapshot path only reads the memo.
// Both returned maps are shared across calls until the next rebuild and must be
// treated as read-only (the depSet convention).
func (c *SnapshotCache) serviceRoutesAndDomainsSnapshot() (map[string][]proxy.GammaRoute, map[string][]string) {
	// Lock order: captureMu and depMu are taken strictly sequentially, never
	// nested (matching SetCaptureTCPServices).
	c.captureMu.RLock()
	authGen := c.captureAuthGen
	c.captureMu.RUnlock()

	c.depMu.Lock()
	routes := c.effectiveStrippedRoutesLocked()
	if c.routeDomainsValid && c.routeDomainsDepGen == c.depGen && c.routeDomainsAuthGen == authGen {
		domains := c.routeDomains
		c.depMu.Unlock()
		return routes, domains
	}
	depGen := c.depGen
	// routeTargetPorts is replaced wholesale by SetRouteTargetPorts, never
	// mutated in place, so the reference stays safe to read outside depMu.
	ports := c.routeTargetPorts
	c.depMu.Unlock()

	// Same replace-not-mutate convention for captureAuthorities. Re-read the
	// generation WITH the map so the memo tag matches the inputs actually used;
	// if either input advances mid-build, the tag mismatch makes the next
	// reader rebuild.
	c.captureMu.RLock()
	authGen = c.captureAuthGen
	authorities := c.captureAuthorities
	c.captureMu.RUnlock()

	built := c.buildRouteTargetDomains(routes, authorities, ports)

	c.depMu.Lock()
	c.routeDomains = built
	c.routeDomainsDepGen = depGen
	c.routeDomainsAuthGen = authGen
	c.routeDomainsValid = true
	c.depMu.Unlock()
	return routes, built
}

// buildRouteTargetDomains renders the cap_http domain list for every route
// target in routes (the memo rebuild). The fqdn is the target's mesh authority
// when SA-backed (captureAuthorities), else derived from its "<ns>/<svc>" key;
// a target with an unparseable key and no authority entry gets no entry
// (captureVhosts skips those, matching the previous inline behavior).
//
// bareNameCount equivalence: the previous inline computation counted bare names
// only over route targets present in the dependency set — but every effective
// route target is unconditionally unioned into the dependency set
// (dependencySetLocked), so counting over all route keys is identical.
func (c *SnapshotCache) buildRouteTargetDomains(routes map[string][]proxy.GammaRoute, authorities map[string]string, ports map[string][]uint32) map[string][]string {
	if len(routes) == 0 {
		return nil
	}
	bareNameCount := make(map[string]int, len(routes))
	for svc := range routes {
		if ref, ok := serviceref.ParseKey(svc); ok {
			bareNameCount[ref.Name]++
		}
	}
	out := make(map[string][]string, len(routes))
	for svc := range routes {
		fqdn, ok := authorities[svc]
		if !ok {
			ref, parsed := serviceref.ParseKey(svc)
			if !parsed {
				continue
			}
			fqdn = ref.ClusterLocalFQDN()
		}
		mesh := proxy.ServiceClusterName(svc, c.meshDomain)
		out[svc] = routeTargetDomains(svc, fqdn, mesh, bareNameCount, ports[svc])
	}
	return out
}

// routeTargetDomains builds the cap_http host-match domains for a GAMMA route
// target keyed "<ns>/<name>". A captured request carries whatever authority the
// client used to dial the target. Kubernetes search-domain resolution lets a
// client reach a Service by any of its short names — the bare name (same
// namespace), the <name>.<namespace> form, and the <name>.<namespace>.svc form —
// in addition to the full cluster.local FQDN and aether's mesh name. The captured
// request's :authority is exactly the (un-resolved) name the client typed, so
// cap_http must host-match every spelling or a same-namespace dial misses the
// route-target vhost and falls through to the kube-proxy passthrough (round
// robin), bypassing the GAMMA rules. Both portless and the mesh :18081 spellings
// are emitted, plus each real Service port (proposal 023 M2) so a client dialing
// the captured ClusterIP:port (echo:80 / echo:8080) host-matches too. The bare
// same-namespace name is emitted only when a single namespace owns it
// (bareNameCount==1), since a duplicate bare domain makes Envoy NACK the route
// config. Called from the routeDomains memo rebuild (issue #540), not per
// snapshot.
func routeTargetDomains(svc, fqdn, mesh string, bareNameCount map[string]int, ports []uint32) []string {
	baseNames := []string{fqdn, mesh}
	if ref, ok := serviceref.ParseKey(svc); ok {
		baseNames = append(
			baseNames,
			ref.Name+"."+ref.Namespace,        // <name>.<namespace>
			ref.Name+"."+ref.Namespace+".svc", // <name>.<namespace>.svc
		)
		// The bare same-namespace name is only collision-free when one namespace owns it.
		if bareNameCount[ref.Name] == 1 {
			baseNames = append(baseNames, ref.Name)
		}
	}
	domains := make([]string, 0, len(baseNames)*2)
	for _, n := range baseNames {
		// Portless + the mesh :18081 spelling (the mesh-DNS path uses the latter).
		domains = append(domains, n, fmt.Sprintf("%s:%d", n, meshconst.ProxyOutboundPort))
	}
	// proposal 023 M2: also match the route target's REAL Service port(s).
	for _, p := range ports {
		if p == 0 || p == meshconst.ProxyOutboundPort {
			continue // 0 = unset; :18081 already emitted above.
		}
		for _, n := range baseNames {
			domains = append(domains, fmt.Sprintf("%s:%d", n, p))
		}
	}
	return domains
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

// applyChainFilter enables svc's service-wide extension filter (025 M4 CHAIN scope)
// on its capture vhost, when one is configured.
func applyChainFilter(vh *routev3.VirtualHost, filters map[string]proxy.ExtensionFilter, svc string) {
	if ef, ok := filters[svc]; ok {
		proxy.ApplyServiceChainFilter(vh, &ef)
	}
}

// extensionHTTPFilters returns the default-disabled HCM entries for the union of
// escape-hatch filters in scope (route-referenced + service-wide chain filters, 025).
// Shared by the capture AND outbound HTTP listener builds: typed_per_filter_config —
// per-route or vhost-level — can only re-enable a filter already in the chain, and
// both route tables carry GAMMA/chain config, so both HCMs need the union.
func (c *SnapshotCache) extensionHTTPFilters() []*http_connection_managerv3.HttpFilter {
	var allRules []proxy.GammaRoute
	for _, rules := range c.serviceRoutesSnapshot() {
		allRules = append(allRules, rules...)
	}
	chainFilters := c.serviceChainFiltersSnapshot()
	chainExtras := make([]proxy.ExtensionFilter, 0, len(chainFilters))
	for _, ef := range chainFilters {
		chainExtras = append(chainExtras, ef)
	}
	// INBOUND-scope filters need their default-disabled chain entry too — rbac
	// brings no system entry (unlike ext_authz's sidecar entry), so without this
	// the inbound TPFC references an absent filter and Envoy rejects the listener
	// (found live: an INBOUND rbac ENFORCE silently never took effect).
	c.depMu.RLock()
	for _, ef := range c.serviceInboundFilters {
		chainExtras = append(chainExtras, ef)
	}
	c.depMu.RUnlock()
	union := proxy.CollectExtensionFilters(allRules, chainExtras...)
	// Node-local authz sidecar (proposal 027): the disabled ext_authz entry carries
	// the full transport (the per-route type cannot); routes opt in via TPFC.
	// Boot-time config — set before any listener generates.
	if c.authzSidecar {
		union = append(union, proxy.AuthzSidecarHTTPFilter(c.authzTimeout, c.authzFailureModeAllow))
	}
	return union
}

// SetAuthzSidecar configures the node-local authz sidecar ext_authz entry
// (proposal 027). Called once at startup, before listener generation.
func (c *SnapshotCache) SetAuthzSidecar(timeout time.Duration, failureModeAllow bool) {
	c.authzSidecar = true
	c.authzTimeout = timeout
	c.authzFailureModeAllow = failureModeAllow
	// Authz availability changes the extension-strip result: invalidate the
	// effective-routes memo (boot-time in production, but never serve a memo
	// built under the old availability).
	c.depMu.Lock()
	c.bumpDepGenLocked()
	c.depMu.Unlock()
}

// podExtensionHTTPFilters returns the extension union for one pod's EGRESS chains:
// the shared union plus, when the authz sidecar is enabled, a per-pod set_metadata
// entry (PREPENDED — it must run before ext_authz) carrying the calling workload's
// identity in aether.source. Egress-only: the inbound chains take the shared union
// (caller identity there is the verified XFCC, and stamping the destination pod as
// "source" would mislead policies).
func (c *SnapshotCache) podExtensionHTTPFilters(cniPod *cniv1.CNIPod) []*http_connection_managerv3.HttpFilter {
	union := c.extensionHTTPFilters()
	if c.authzSidecar && proxy.HasExtAuthz(union) {
		union = append([]*http_connection_managerv3.HttpFilter{proxy.SourceMetadataHTTPFilter(cniPod, c.trustDomain)}, union...)
	}
	return union
}
