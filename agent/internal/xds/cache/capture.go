package cache

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"aethermesh.dev/common/serviceref"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/meshdns"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// generateUDPCaptureListener builds a pod's per-pod UDP capture listener, or
// returns nil when capture is disabled or no UDPRoute arm can be built.
// The listener is generated from the current udpServiceRoutes snapshot joined
// with the parents' ClusterIPs.
func (c *SnapshotCache) generateUDPCaptureListener(cniPod *cniv1.CNIPod) (types.Resource, error) {
	if !c.captureEnabled {
		return nil, nil
	}
	// Offer the generator only the backends whose udp: cluster THIS snapshot
	// generation publishes (#873, the same hazard as proposal 037 Risk 1 and
	// #877). udp_proxy has no ODCDS cold path: a listener naming a cluster the
	// snapshot does not carry accepts datagrams and drops them, with no NACK,
	// no log and no stat to distinguish it from a backend that is down.
	// captureUDPClusters SKIPS a backend service the cluster cache does not
	// hold, so without this the generator could bind exactly that name.
	udpRoutes, unroutable := c.routableUDPRoutes(c.udpServiceRoutesSnapshot())
	clusterIPs := c.udpParentClusterIPs()

	// Say out loud what the UDP path throws away (#873). Each arm carries ONE
	// cluster, so a traffic split is discarded, and a parent whose VIP is not
	// known yet has no arm -- with no NACK and no stat, the first symptom would
	// be datagrams arriving somewhere unintended. This does not change what is
	// generated; it makes the gap discoverable without reading the generator.
	reasons := append(unroutable, proxy.UnsupportedUDPRouteShapes(udpRoutes, clusterIPs)...)
	if len(reasons) > 0 {
		c.metrics.UDPRouteUnsupported(context.Background(), int64(len(reasons)))
		for _, reason := range reasons {
			c.log.Warn("UDPRoute input discarded: the per-pod UDP capture listener cannot represent it",
				"pod", cniPod.GetName(),
				"reason", reason,
				"issue", "873")
		}
	}

	l, err := proxy.GenerateUDPCaptureListener(
		cniPod.GetName(),
		cniPod.GetNetworkNamespace(),
		meshconst.ProxyL4OutboundPort,
		udpRoutes,
		clusterIPs,
	)
	if err != nil {
		return nil, err
	}
	if l == nil {
		// No UDPRoute arm in scope: GenerateUDPCaptureListener returns a nil
		// *Listener. Return an untyped nil interface — NOT the typed-nil pointer.
		// Returning the *listenerv3.Listener directly would wrap a nil pointer in a
		// non-nil types.Resource interface, defeating the `!= nil` guard in
		// Listeners(); the empty-Name/nil-Address Listener then lands in the LDS
		// snapshot and Envoy NACKs the whole push with "address is necessary".
		return nil, nil
	}
	return l, nil
}

// udpParentClusterIPs returns "<ns>/<svc>" -> ClusterIP for every mesh Service
// the capture reconciler has reported, the join the UDP capture matcher keys
// on (proposal 038). It is the SAME source the TCP floor chains match their
// /32 on (SetCaptureTCPServices), so a VIP the TCP floor knows the UDP path
// knows too, and neither can see a Service the other cannot.
//
// Lock order: captureMu for reading; callers hold listenerMu, the direction
// SetCaptureTCPServices' own listener rebuild already establishes.
func (c *SnapshotCache) udpParentClusterIPs() map[string]string {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	out := make(map[string]string, len(c.captureTCPServices))
	for _, e := range c.captureTCPServices {
		out[e.serviceName] = e.clusterIP
	}
	return out
}

// generateCaptureListener builds a pod's transparent-capture listener, or returns
// nil when capture is disabled (so the listenerEntry carries no capture resource).
//
// extensionFilters is the pod's escape-hatch HCM chain (proposal 025): the node-
// global union from extensionHTTPFilters, per-pod-adjusted by
// podExtensionHTTPFilters. It is passed in rather than recomputed here because
// the union is node-global and every caller in a per-pod loop would otherwise
// rebuild it once (twice, on the outbound+capture path) per pod.
// trustDomain names the pod's own SPIFFE ID, which every mesh-originating chain
// on this listener stamps into filter state next to the netns (issue #815,
// proxy.SourceIdentityForPod). Callers that have it from the CNI ADD pass it
// through; the regeneration paths pass c.currentTrustDomain().
func (c *SnapshotCache) generateCaptureListener(cniPod *cniv1.CNIPod, trustDomain string, extensionFilters []*http_connection_managerv3.HttpFilter) (types.Resource, error) {
	if !c.captureEnabled {
		return nil, nil
	}
	// #877: the TCP floor is mTLS-only. captureTCPClusters returns nothing
	// without a node SVID and a validation context, so emitting cap_tcp_* chains
	// here regardless produced a listener that ACCEPTED connections with no
	// cluster behind them -- and tcp_proxy has no ODCDS cold path, so those
	// connections died silently. A mesh with SPIRE disabled looked configured
	// and swallowed every raw-TCP connection to a mesh service.
	//
	// Gate the chains on the same condition as the clusters, and say so. Failing
	// CLOSED is deliberate: building a plaintext TCP floor when identity is
	// unavailable would be a silent authentication downgrade, the #832/#843
	// class. Suppressing the chains instead lets the connection fall to the
	// passthrough and be REJECTed by kube-proxy at the mesh Service port -- an
	// immediate ECONNREFUSED the caller can act on.
	//
	// This is a startup state as well as a misconfiguration: identity arrives
	// asynchronously, and both SetNodeIdentity and a trust-domain change rebuild
	// every pod listener, so the chains appear as soon as it does.
	identityReady := c.tcpFloorIdentityReady()

	c.captureMu.RLock()
	tcpRoutes := c.tcpServiceRoutesSnapshot()
	tlsRoutes := c.tlsServiceRoutesSnapshot()
	if !identityReady && len(c.captureTCPServices) > 0 {
		c.warnTCPFloorWithoutIdentity(len(c.captureTCPServices))
	}
	var tcpServices []proxy.CaptureTCPService
	for _, e := range c.captureTCPServices {
		if !identityReady {
			break
		}
		tcpServices = append(tcpServices, proxy.CaptureTCPService{
			// TCP clusters are separate from HTTP clusters: they share the same EDS
			// resource (same endpoint set) but use no ALPN on the transport socket so
			// the destination inbound demuxes to the TCP floor DEFAULT chain.
			ClusterName:  proxy.TCPClusterName(e.serviceName, c.meshDomain),
			TCPPorts:     e.tcpPorts,
			PrimaryIsTCP: e.primaryIsTCP,
			PrimaryPort:  e.primaryPort,
			ClusterIP:    e.clusterIP,
			// L4 route rules (Phase 3b): override the passthrough floor chain when
			// a TCPRoute or TLSRoute is attached to this service.
			TCPRouteRules: tcpRoutes[e.serviceName],
			TLSRouteRules: tlsRoutes[e.serviceName],
		})
	}
	c.captureMu.RUnlock()

	l, err := proxy.GenerateCaptureListener(cniPod, proxy.SourceIdentityForPod(cniPod, trustDomain), meshconst.ProxyCapturePort, c.meshDomain, c.emitStatsPod, tcpServices, c.captureRedirectAll, extensionFilters)
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

// SetMeshDNSSnapshotPath sets the host-persistent file the mesh service->IP record
// table is written to (proposal 018, mesh-global FQDN; issue #578), or empty when
// mesh DNS is off. The agent no longer serves DNS: the standalone aether-mesh-dns
// DaemonSet watches this file and serves :18054, and the CNI DNATs each pod's :53
// straight to it. The cache only persists records here.
func (c *SnapshotCache) SetMeshDNSSnapshotPath(path string) { c.meshDNSSnapshotPath = path }

// SetMeshDNSRecords persists the mesh service -> IP table (from the mesh-Service
// reconciler) to the snapshot file the standalone resolver daemon watches. No-op
// when mesh DNS is off (empty path). A persist error is logged, never fatal.
//
// The write always happens (it re-stamps the envelope's writtenAt, which is the
// daemon's freshness signal), but the generation only advances when the record
// CONTENT changed, so a heartbeat rewrite is distinguishable from a real update.
func (c *SnapshotCache) SetMeshDNSRecords(records map[string]string) {
	if c.meshDNSSnapshotPath == "" {
		return
	}
	c.meshDNSMu.Lock()
	if !equalStringMaps(c.meshDNSRecords, records) {
		c.meshDNSGeneration++
	}
	c.meshDNSRecords = records
	generation := c.meshDNSGeneration
	c.meshDNSMu.Unlock()
	c.writeMeshDNSSnapshot(records, generation)
}

// RewriteMeshDNSSnapshot re-persists the last projected record table with a fresh
// writtenAt stamp and an UNCHANGED generation. It backs the periodic freshness
// heartbeat (see capture.MeshDNSHeartbeat): the capture reconciler is event-driven
// with no short resync, so on a quiet cluster the snapshot would otherwise look stale
// to the resolver daemon and its snapshot-age gauge could not distinguish "nothing
// changed" from "the agent is wedged".
//
// No-op when mesh DNS is off, and — importantly — before the first projection: writing
// an empty table would flip the daemon ready and NXDOMAIN the whole mesh.
func (c *SnapshotCache) RewriteMeshDNSSnapshot() {
	if c.meshDNSSnapshotPath == "" {
		return
	}
	c.meshDNSMu.Lock()
	records, generation := c.meshDNSRecords, c.meshDNSGeneration
	c.meshDNSMu.Unlock()
	if records == nil {
		return
	}
	c.writeMeshDNSSnapshot(records, generation)
}

// writeMeshDNSSnapshot persists the envelope; a write failure is logged, never fatal
// (the daemon keeps serving its current table).
func (c *SnapshotCache) writeMeshDNSSnapshot(records map[string]string, generation uint64) {
	if err := meshdns.WriteSnapshot(c.meshDNSSnapshotPath, records, generation); err != nil {
		c.log.Warn("failed to persist mesh-DNS snapshot", "path", c.meshDNSSnapshotPath, "error", err)
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
				serviceName:  s.ServiceName,
				clusterIP:    s.ClusterIP,
				primaryIsTCP: s.PrimaryIsTCP,
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
		// Node-global union, built once for the whole loop (see extensionHTTPFilters).
		shared := c.extensionHTTPFilters()
		c.listenerMu.Lock()
		// Read INSIDE listenerMu: see rebuildPodListenersLocked. A trust domain
		// sampled before this lock can be stale (empty at startup) by the time
		// the loop runs, and an empty one renders `spiffe:///` (#815/#819).
		trustDomain := c.currentTrustDomain()
		for netns, entry := range c.listeners {
			if entry.cniPod == nil {
				continue
			}
			newCapture, err := c.generateCaptureListener(entry.cniPod, trustDomain, c.podExtensionHTTPFilters(entry.cniPod, shared))
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

	// Only the node SVID is read here. The local workload identities used to be
	// collected too, to build the TCP floor clusters' per-source
	// transport_socket_matches — since #842 the client certificate is chosen per
	// connection from filter state, so a floor cluster has no per-workload input
	// and its bytes do not move when a pod arrives or leaves.
	c.localMu.RLock()
	nodeSpiffeID := c.nodeSpiffeID
	c.localMu.RUnlock()

	// The blackhole cluster backs the scoped-mode cap_tcp_blackhole chain, and
	// is emitted BEFORE the identity gate below: it carries no transport
	// socket and terminates connections rather than forwarding them, so it
	// needs no SVID. Its chain is likewise ungated, so gating the cluster would
	// leave that chain naming something absent — the Risk 1 shape.
	var blackhole []types.Resource
	if !c.captureRedirectAll {
		blackhole = append(blackhole, proxy.NewBlackholeCluster())
	}

	validationContextName := c.validationContextName()
	if nodeSpiffeID == "" || validationContextName == "" {
		// Node SVID or trust domain not yet available; skip the per-service TCP
		// clusters until both are (an empty trust domain renders `spiffe://`,
		// #815). The blackhole still goes out — see above.
		return blackhole
	}

	// SAN namespaces come from the service's TCP floor entry, which since
	// proposal 037 design (a) is keyed "tcp:<fqdn>" rather than by the bare
	// service name. Taking them from the TCP entry rather than whatever sits
	// under the bare key also pins against the right endpoint set: for a service
	// whose pods split across protocols the two entries have DIFFERENT endpoints,
	// and the floor must pin the namespaces of the pods it actually reaches.
	c.clusterMu.RLock()
	resources := make([]types.Resource, 0, len(entries)+len(blackhole))
	resources = append(resources, blackhole...)
	for _, e := range entries {
		tcpEntry, ok := c.tcpEntryLocked(e.serviceName)
		if !ok {
			// Service not yet in scope as a TCP service; skip until it is.
			continue
		}
		// The service's expected server SPIFFE IDs are precomputed on the entry
		// (refreshEntryMTLSLocked, issue #537) from its endpoints' namespaces
		// and the BARE service name for the sa/ segment — the same pinning the
		// HTTP cluster path uses.
		sanURIs := tcpEntry.sanURIs
		tcpName := proxy.TCPClusterName(e.serviceName, c.meshDomain)
		cl := proxy.NewTCPServiceCluster(tcpName, e.serviceName, e.serviceName)
		// NO SNI for the TCP floor: the egress floor connection must NOT carry the
		// destination port as SNI, or the peer's inbound per-port HCM chain
		// (server_names:[port]) would win over the inbound TCP floor's default chain
		// (server_names > application_protocols > default) — the connection lands on
		// the HCM, which can't parse the raw TCP stream and 503s. An empty SNI lets it
		// fall through to the inbound default floor chain (tcp_proxy to the app).
		proxy.InjectUpstreamTCPMTLS(cl, nodeSpiffeID, validationContextName, sanURIs, "")
		resources = append(resources, cl)

		resources = append(resources, c.tcpPortClustersLocked(
			e, tcpEntry, tcpName, sanURIs, nodeSpiffeID, validationContextName)...)
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
	c.localMu.RUnlock()
	validationContextName := c.validationContextName()
	if nodeSpiffeID == "" || validationContextName == "" {
		return nil
	}

	c.clusterMu.RLock()
	resources := make([]types.Resource, 0, len(services))
	for _, svc := range services {
		// The TCP floor entry, keyed "tcp:<fqdn>" (proposal 037 design (a)).
		entry, ok := c.tcpEntryLocked(svc)
		if !ok {
			continue // not yet in scope; will appear when registry delivers endpoints
		}
		// SAN pinning precomputed on the entry (refreshEntryMTLSLocked, issue
		// #537); see the node-proxy TCP variant above.
		sanURIs := entry.sanURIs
		tcpName := proxy.TCPClusterName(svc, c.meshDomain)
		cl := proxy.NewTCPServiceCluster(tcpName, svc, svc)
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

// reconcileUDPCaptureListeners rebuilds the per-pod UDP capture listeners when
// the arm set they can carry has changed (#873, proposal 038).
//
// Which arms exist is a function of the CLUSTER cache and of the capture
// reconciler's Service list, not only of the UDPRoute: routableUDPRoutes refuses
// to name a cluster this snapshot does not publish, because udp_proxy has no
// on-demand cluster path and would accept the datagrams and drop them; and an
// arm needs its parent's ClusterIP, which arrives on its own watch. Both are
// filled asynchronously, so a UDPRoute that lands before its backend registers
// or before its parent Service is observed produces no arm, and
// SetUDPServiceRoutes — the only other trigger — will not fire again. Running
// here, on every snapshot push, is the same discipline
// reconcileCaptureTCPChains follows for the TCP floor's identity gate, and for
// the same reason: it makes "the listener is silently absent forever"
// unreachable.
//
// The comparison is over the CANONICAL ARM SET (UDPCaptureArmsKey), not one
// string: the pre-038 compare held the single bound cluster, so a second
// service gaining an arm, or a VIP arriving late for one of several, was
// invisible to it. Steady state is one map walk and a string compare; nothing
// is rebuilt (and nothing is re-warned) unless an arm actually moved.
func (c *SnapshotCache) reconcileUDPCaptureListeners() {
	if !c.captureEnabled {
		return
	}
	routable, _ := c.routableUDPRoutes(c.udpServiceRoutesSnapshot())
	key := proxy.UDPCaptureArmsKey(proxy.UDPCaptureArms(routable, c.udpParentClusterIPs()))

	c.captureMu.Lock()
	changed := c.udpCaptureArmsSeen != key
	c.udpCaptureArmsSeen = key
	c.captureMu.Unlock()

	if !changed {
		return
	}
	c.regenerateAllUDPCaptureListeners()
}

// routableUDPRoutes blanks the cluster name of every UDPRoute backend whose
// udp: cluster captureUDPClusters will NOT publish in this generation, and
// returns one reason per blanked backend.
//
// Blanking rather than removing is deliberate: the generator already treats an
// unnamed cluster as unbindable, and keeping the backend in the list means a
// service left with nothing still reports itself (an accepted UDPRoute with no
// data path) instead of going quiet.
//
// Lock order: this takes clusterMu for reading, and one of its callers
// (generateUDPCaptureListener) already holds listenerMu. That direction is how
// the listener path already reads depMu and captureMu, and no clusterMu writer
// takes listenerMu, so it closes no cycle.
func (c *SnapshotCache) routableUDPRoutes(udpRoutes map[string][]proxy.L4Backend) (map[string][]proxy.L4Backend, []string) {
	if len(udpRoutes) == 0 {
		return udpRoutes, nil
	}
	out := make(map[string][]proxy.L4Backend, len(udpRoutes))
	var reasons []string

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	// Sorted: udpRoutes is a map, and an unsorted walk would emit the reasons
	// (and the WARN lines) in a fresh random order every rebuild (#135).
	for _, svc := range slices.Sorted(maps.Keys(udpRoutes)) {
		backends := udpRoutes[svc]
		kept := make([]proxy.L4Backend, 0, len(backends))
		for _, b := range backends {
			if b.Cluster != "" && c.udpClusterForLocked(b.Service) == b.Cluster {
				kept = append(kept, b)
				continue
			}
			reasons = append(reasons, fmt.Sprintf(
				"service %q backend %q is not routable: this snapshot publishes no %q cluster (the backend service is not in the cluster cache, or has no registered port), and udp_proxy has no on-demand cluster path — naming it would accept datagrams and drop them in silence",
				svc, b.Service, b.Cluster))
			// Keep the backend, minus its cluster: unbindable, but still counted
			// when the service turns out to have nothing left.
			b.Cluster = ""
			kept = append(kept, b)
		}
		out[svc] = kept
	}
	return out, reasons
}

// udpClusterForLocked returns the udp: cluster name captureUDPClusters will
// publish for a backend service, or "" when it will skip it. It is the SAME
// predicate captureUDPClusters applies, so the listener and CDS cannot disagree.
// Caller holds clusterMu.
func (c *SnapshotCache) udpClusterForLocked(svc string) string {
	if svc == "" {
		return ""
	}
	entry, ok := c.serviceEntryLocked(svc)
	if !ok || entry.loadAssignment == nil {
		return ""
	}
	// entry.sni carries the backend's registered application port.
	port, err := strconv.Atoi(entry.sni)
	if err != nil || port <= 0 || port > 65535 {
		return ""
	}
	return proxy.UDPClusterName(svc, c.meshDomain)
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
	// Sorted: services is a set built from the UDPRoute backends (a map).
	for _, svc := range slices.Sorted(maps.Keys(services)) {
		// udpClusterForLocked is the single skip predicate: the UDP capture
		// listener consults the SAME one before it binds a backend, so a chain
		// naming a cluster this loop skipped is unrepresentable (#873).
		udpName := c.udpClusterForLocked(svc)
		if udpName == "" {
			// Backend service not in scope yet, or no registered port; skip
			// until its cluster/EDS exists.
			continue
		}
		// A UDPRoute backend may be classified either way, and this needs only
		// service-level facts (the app port in entry.sni and the bare-name EDS),
		// so take whichever entry carries them.
		entry, _ := c.serviceEntryLocked(svc)
		// entry.sni carries the backend's registered application port. The UDP floor
		// has no inbound mTLS hop, so udp_proxy must reach that app port directly (not
		// the mesh inbound :18008 the shared bare-name EDS carries) — build an inline
		// app-port UDP load assignment from the service's endpoints.
		port, _ := strconv.Atoi(entry.sni)
		la := proxy.UDPLoadAssignment(entry.loadAssignment, udpName, uint32(port))
		// A published UDP cluster with no routable endpoint is a SILENT
		// blackhole, and it is the shape #931 shipped in: udp_proxy takes the
		// datagram, finds an empty healthy set (NewUDPServiceCluster sets
		// HealthyPanicThreshold: 0, so it will not spray to unhealthy hosts),
		// and discards it.
		//
		// Nothing else reports it. There is no NACK -- the config is valid. The
		// agent log is clean -- nothing was discarded at projection time, so the
		// #874 path stays quiet. And udp_proxy's own
		// downstream_sess_rx_datagrams does not move either, because it counts
		// per SESSION and a session needs a host: on a live cluster it read 1
		// after four datagrams had reached the socket.
		//
		// So this is the only place the condition can be named. It is reported
		// rather than skipped: skipping would remove the listener and turn a
		// blackhole into a different blackhole, while the cluster staying
		// published means the service recovers on its own the moment an endpoint
		// goes healthy.
		if n := unroutableUDPEndpoints(la); n > 0 {
			c.metrics.UDPNoHealthyBackend(context.Background(), int64(n))
			c.log.Warn("UDP cluster published with no routable endpoint: udp_proxy will discard datagrams for it, silently",
				"service", svc, "cluster", udpName, "endpoints", n, "issue", "931")
		}
		cl := proxy.NewUDPServiceCluster(udpName, svc, la)
		resources = append(resources, cl)
	}
	return resources
}

// unroutableUDPEndpoints reports how many endpoints a UDP load assignment holds
// when NONE of them is routable, and 0 otherwise.
//
// "Routable" is Envoy's own reading, not ours: HEALTHY and UNKNOWN are both load
// balanced (UNKNOWN is the default for an endpoint nobody has said anything
// about), while UNHEALTHY, DRAINING and TIMEOUT are not. Returning the COUNT
// rather than a bool makes the warning say how much was lost, and returning 0
// for an empty assignment keeps this quiet for a service that simply has no
// endpoints yet -- that is a cold start, not a blackhole.
func unroutableUDPEndpoints(la *endpointv3.ClusterLoadAssignment) int {
	total := 0
	for _, lle := range la.GetEndpoints() {
		for _, lb := range lle.GetLbEndpoints() {
			switch lb.GetHealthStatus() {
			case corev3.HealthStatus_HEALTHY, corev3.HealthStatus_UNKNOWN:
				return 0
			default:
				total++
			}
		}
	}
	return total
}

// captureVhosts builds the cap_http virtual hosts: each in-scope service that has a
// cluster.local authority routes (both the portless and :meshPort spellings) to its
// <svc>.<meshDomain> cluster. Scoped to the dependency set so cap_http only references
// clusters the scoped snapshot carries; unknown authorities hit the 404 default.
func (c *SnapshotCache) captureVhosts() []*routev3.VirtualHost {
	// Read-only membership tests only, so take the shared memo rather than a copy.
	deps := c.dependencySetShared()
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
	// Both appenders range maps (captureAuthorities, gammaRoutes), so without
	// this the cap_http RouteConfiguration's repeated virtual_hosts field is in
	// a fresh random order on every snapshot — the #135 mechanism on the primary
	// client route table of a capture-enabled node. See ordering.go.
	sortVirtualHostsByName(vhosts)
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

		// A service in scope that serves NO HTTP port gets a 421 vhost instead
		// of the ordinary one (proposal 037). Emitting the ordinary vhost would
		// point at an h2 cluster the cache never builds for such a service, and
		// the caller would get a 503 with cluster_not_found —
		// indistinguishable from a cluster that vanished mid-reload, and never
		// reaching ODCDS so even the coordinator's 404 does not occur.
		if spellings := c.tcpSpellingsIfNoHTTPPortLocked(svc); spellings != "" {
			vhosts = append(vhosts, proxy.BuildNoHTTPPortVirtualHost(mesh, domains, spellings))
			continue
		}

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
	// Both loops above range maps. These become routes on the redirect-all
	// catch-all virtual host, and Envoy evaluates a virtual host's routes in
	// order, first match wins — so map order is not just a re-hash here, it
	// decides which authority regex wins when two services' short-name
	// spellings overlap (the bare-name collision the doc above calls out).
	// Cluster is unique per target (seen dedupes by service key), so this is a
	// total order.
	slices.SortStableFunc(targets, func(a, b proxy.KnownTargetRoute) int {
		return strings.Compare(a.Cluster, b.Cluster)
	})
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
	ma := make(map[string]captureTCPEntry, len(a))
	for _, e := range a {
		ma[e.serviceName] = e
	}
	for _, e := range b {
		prev, ok := ma[e.serviceName]
		if !ok || prev.clusterIP != e.clusterIP {
			return false
		}
		// The derived TCP port set is part of the identity: a change to it
		// changes the chains, so it must trigger a regeneration. Both sides are
		// sorted at derivation, so ordinary endpoint churn -- pods coming and
		// going with the SAME declared ports -- produces an identical slice and
		// compares equal.
		//
		// That is the whole of Risk 4's mitigation: regenerating a per-pod
		// capture listener drains its connections, so this comparison has to be
		// over the DERIVED set and not over the reload event.
		if !slices.Equal(prev.tcpPorts, e.tcpPorts) ||
			prev.primaryIsTCP != e.primaryIsTCP ||
			prev.primaryPort != e.primaryPort {
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
//
// The result is node-global, so a caller looping over pods computes it ONCE and
// threads it through podExtensionHTTPFilters. The returned slice is shared by
// every such pod and must be treated as immutable — appending to it would land
// one pod's entry in another's chain.
func (c *SnapshotCache) extensionHTTPFilters() []*http_connection_managerv3.HttpFilter {
	// Every source below is a map, and CollectExtensionFilters emits in
	// first-seen order — so without sorting the keys, the HCM's repeated
	// http_filters field comes out in a different order on each rebuild. That
	// is worse than a re-hash: HTTP filter order is the filter chain's
	// EXECUTION order, so two escape-hatch filters would run in a random order
	// relative to each other. Service keys are unique, giving a total order.
	routes := c.serviceRoutesSnapshot()
	var allRules []proxy.GammaRoute
	for _, svc := range slices.Sorted(maps.Keys(routes)) {
		allRules = append(allRules, routes[svc]...)
	}
	chainFilters := c.serviceChainFiltersSnapshot()
	chainExtras := make([]proxy.ExtensionFilter, 0, len(chainFilters))
	for _, svc := range slices.Sorted(maps.Keys(chainFilters)) {
		chainExtras = append(chainExtras, chainFilters[svc])
	}
	// INBOUND-scope filters need their default-disabled chain entry too — rbac
	// brings no system entry (unlike ext_authz's sidecar entry), so without this
	// the inbound TPFC references an absent filter and Envoy rejects the listener
	// (found live: an INBOUND rbac ENFORCE silently never took effect).
	c.depMu.RLock()
	for _, svc := range slices.Sorted(maps.Keys(c.serviceInboundFilters)) {
		chainExtras = append(chainExtras, c.serviceInboundFilters[svc])
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
// the shared union (from extensionHTTPFilters) plus, when the authz sidecar is
// enabled, a per-pod set_metadata entry (PREPENDED — it must run before ext_authz)
// carrying the calling workload's identity in aether.source. Egress-only: the
// inbound chains take the shared union (caller identity there is the verified XFCC,
// and stamping the destination pod as "source" would mislead policies).
//
// shared is never mutated: the prepend allocates a fresh slice, so the caller's
// node-global union stays valid for the next pod.
func (c *SnapshotCache) podExtensionHTTPFilters(cniPod *cniv1.CNIPod, shared []*http_connection_managerv3.HttpFilter) []*http_connection_managerv3.HttpFilter {
	if c.authzSidecar && proxy.HasExtAuthz(shared) {
		return append([]*http_connection_managerv3.HttpFilter{proxy.SourceMetadataHTTPFilter(cniPod, c.currentTrustDomain())}, shared...)
	}
	return shared
}

// deriveTCPPorts returns each service's NON-PRIMARY raw-TCP ports, from the
// port_protocols its endpoints declare (proposal 037).
//
// The primary port is excluded on purpose: it is what the portless floor chain
// already reaches, so a chain for it would add nothing and would need the same
// cluster the floor already uses. Everything returned here is a port that has
// no data path today.
//
// Classification comes from the ENDPOINTS rather than from the mesh Service's
// aether.io/app-protocol annotation. That annotation is the registrar's
// projection of a per-pod fact, one hop removed from its source, and #878 is
// what happens when the two copies disagree. Deriving here means the chain and
// the cluster that serves it are built from one map in one snapshot
// generation, which is also the structural half of #877 — a chain whose
// cluster was never built kills connections silently, and tcp_proxy has no
// ODCDS cold path to recover.
//
// The result is sorted so that two derivations over the same declared ports
// compare equal regardless of map iteration order. equalTCPEntries depends on
// that: endpoint churn must not read as a change, or every pod ADD/DEL would
// regenerate per-pod capture listeners and drain their connections (Risk 4).
func deriveTCPPorts(endpointsByService map[string][]*registryv1.ServiceEndpoint) map[string][]uint32 {
	out := make(map[string][]uint32, len(endpointsByService))
	for service, endpoints := range endpointsByService {
		if ports := nonPrimaryTCPPorts(endpoints); len(ports) > 0 {
			out[service] = ports
		}
	}
	return out
}

// nonPrimaryTCPPorts returns one service's raw-TCP ports excluding its primary,
// sorted and de-duplicated across its endpoints. Empty when the service has
// none — which is every service that existed before proposal 037, since a pod
// with one declared protocol has exactly one class and its primary carries it.
func nonPrimaryTCPPorts(endpoints []*registryv1.ServiceEndpoint) []uint32 {
	if len(endpoints) == 0 {
		return nil
	}
	// Endpoints of one service share a primary port (proposal 005), so the
	// first is representative.
	primary := endpoints[0].GetPort()
	seen := map[uint32]struct{}{}
	for _, ep := range endpoints {
		for port, proto := range ep.GetPortProtocols() {
			if proto == registryv1.PortProtocol_PORT_PROTOCOL_TCP &&
				port != primary && port > 0 && port <= 65535 {
				seen[port] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	ports := make([]uint32, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	slices.Sort(ports)
	return ports
}

// refreshCaptureTCPPorts updates each capture-TCP service's derived per-port
// set and, ONLY if any of them changed, rebuilds the per-pod capture listeners
// so the new destination_port chains reach Envoy (proposal 037).
//
// It reuses SetCaptureTCPServices' entry list and its change detection rather
// than adding a second regeneration trigger: one comparison, over the derived
// state, is the whole of Risk 4's mitigation. Regenerating a per-pod capture
// listener drains that listener's connections, and a registry reload happens on
// every pod ADD/DEL anywhere on the node — so a trigger keyed on "a reload
// occurred" rather than "the derived set changed" would turn ordinary churn
// into dropped connections.
func (c *SnapshotCache) refreshCaptureTCPPorts(derived map[string][]uint32, primary map[string]uint32) {
	c.captureMu.Lock()
	next := make([]captureTCPEntry, 0, len(c.captureTCPServices))
	for _, e := range c.captureTCPServices {
		e.tcpPorts = derived[e.serviceName]
		e.primaryPort = primary[e.serviceName]
		next = append(next, e)
	}
	changed := !equalTCPEntries(c.captureTCPServices, next)
	c.captureTCPServices = next
	c.captureMu.Unlock()

	if !changed || !c.captureEnabled {
		return
	}
	// Same rebuild path SetCaptureTCPServices uses: per-pod capture listeners
	// embed the TCP chains, so they all have to be regenerated.
	shared := c.extensionHTTPFilters()
	c.listenerMu.Lock()
	c.rebuildPodListenersLocked("derived TCP port set change", shared)
	c.listenerMu.Unlock()
}

// tcpFloorIdentityReady reports whether the node has what the TCP floor's
// upstream mTLS needs: a node SVID and a validation context. It is exactly the
// condition captureTCPClusters gates on, factored out so the CHAINS and the
// CLUSTERS cannot drift apart again (#877).
func (c *SnapshotCache) tcpFloorIdentityReady() bool {
	c.localMu.RLock()
	nodeSpiffeID := c.nodeSpiffeID
	c.localMu.RUnlock()
	return nodeSpiffeID != "" && c.validationContextName() != ""
}

// warnTCPFloorWithoutIdentity logs, at most once a minute, that TCP mesh
// services are configured but unroutable for want of identity (#877).
//
// Rate-limited because this is also the normal startup state for a few seconds,
// and a per-listener-build log would be one line per pod per rebuild. It is a
// WARN rather than an INFO because the steady state is a real outage: with
// SPIRE off, every raw-TCP mesh service silently refuses.
func (c *SnapshotCache) warnTCPFloorWithoutIdentity(services int) {
	c.tcpFloorWarnMu.Lock()
	defer c.tcpFloorWarnMu.Unlock()
	if time.Since(c.tcpFloorWarnedAt) < time.Minute {
		return
	}
	c.tcpFloorWarnedAt = time.Now()
	c.log.Warn("TCP mesh services are configured but have no capture chains: the node has no SVID or no trust domain yet, and the TCP floor is mTLS-only",
		"services", services,
		"effect", "raw-TCP connections to these services fall to passthrough and are refused",
		"issue", "aether#877")
}

// reconcileCaptureTCPChains rebuilds the per-pod capture listeners when the TCP
// floor's identity readiness has CHANGED since they were last built (#877).
//
// The chains are gated on a node SVID and a validation context, which arrive
// asynchronously — so a listener built before identity lands carries none, and
// something has to rebuild it afterwards or the gate becomes a permanent
// outage. This is the same reason recomputeInboundReadyClusters runs from
// generateSnapshot rather than from its mutators: SetNodeIdentity is called
// exactly once ever by the SPIRE bridge (`if firstServe`), so a trigger hanging
// off it can be missed permanently — which is how main-worker-05 ran a whole
// agent lifetime with no probe clusters at all on 2026-09-19.
//
// A comparison and an early return in the steady state; nothing is rebuilt
// unless readiness actually flipped.
func (c *SnapshotCache) reconcileCaptureTCPChains() {
	if !c.captureEnabled {
		return
	}
	ready := c.tcpFloorIdentityReady()

	c.captureMu.Lock()
	changed := c.tcpFloorIdentitySeen != ready
	c.tcpFloorIdentitySeen = ready
	hasTCP := len(c.captureTCPServices) > 0
	c.captureMu.Unlock()

	if !changed || !hasTCP {
		return
	}
	shared := c.extensionHTTPFilters()
	c.listenerMu.Lock()
	c.rebuildPodListenersLocked("TCP floor identity change", shared)
	c.listenerMu.Unlock()
}

// primaryPortOf returns the primary application port a TCP floor entry
// addresses, from its sni field (which buildTCPClustersLocked sets to the
// service's default port). Zero when unset or unparseable, which callers treat
// as "no alias".
func primaryPortOf(entry clusterEntry) uint32 {
	if entry.sni == "" {
		return 0
	}
	p, err := strconv.Atoi(entry.sni)
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return uint32(p)
}

// tcpPortClustersLocked returns a TCP service's port-qualified clusters:
// tcp:<fqdn>:<port> for every port it serves as raw TCP (proposal 037).
// Caller must hold clusterMu.
//
// Two shapes, and the difference is the SNI:
//
//   - The PRIMARY port is an alias. Same EDS as the floor and NO SNI, because
//     the destination's default inbound chain is what serves it and a non-empty
//     SNI would route it to a per-port chain that does not exist (#306). It
//     exists so a port-qualified reference resolves for every TCP port and not
//     only the non-primary ones — without it a TCPRoute naming the primary port
//     would point at a cluster that is not there.
//   - Every NON-PRIMARY port carries its own load assignment and DOES set SNI,
//     which is how the destination demuxes to the right loopback port. Safe
//     because only a post-037 agent advertises such a port, and that is the
//     same agent that builds the matching inbound chain.
//
// Built from the same derived port set as the capture chains, in the same
// snapshot generation: a chain naming a cluster that is not in the snapshot is
// killed silently, because tcp_proxy has no ODCDS cold path (Risk 1).
func (c *SnapshotCache) tcpPortClustersLocked(
	e captureTCPEntry,
	tcpEntry clusterEntry,
	tcpName string,
	sanURIs []string,
	nodeSpiffeID, validationContextName string,
) []types.Resource {
	var out []types.Resource

	if aliasName := proxy.TCPPortClusterName(tcpName, primaryPortOf(tcpEntry)); aliasName != "" {
		if _, ok := c.clusters[aliasName]; ok {
			ac := proxy.NewTCPServiceCluster(aliasName, e.serviceName, e.serviceName)
			proxy.InjectUpstreamTCPMTLS(ac, nodeSpiffeID, validationContextName, sanURIs, "")
			out = append(out, ac)
		}
	}

	for _, port := range e.tcpPorts {
		portEntry, ok := c.clusters[proxy.TCPPortClusterName(tcpName, port)]
		if !ok || portEntry.loadAssignment == nil {
			continue
		}
		pc := proxy.NewTCPServiceCluster(
			proxy.TCPPortClusterName(tcpName, port),
			portEntry.loadAssignment.GetClusterName(),
			e.serviceName,
		)
		proxy.InjectUpstreamTCPMTLS(pc, nodeSpiffeID, validationContextName, portEntry.sanURIs, strconv.Itoa(int(port)))
		out = append(out, pc)
	}
	return out
}

// tcpSpellingsIfNoHTTPPortLocked returns the TCP spellings a caller should use
// for a service that serves NO HTTP port, or "" when the service has one (and
// therefore gets an ordinary vhost). Caller must hold clusterMu or be on a path
// that does.
//
// "No HTTP port" is read from the CACHE, not from the app-protocol annotation:
// a service has an HTTP port exactly when the HTTP pass built it a default
// entry under the bare service key. That is the same fact the vhost would point
// at, so the two cannot disagree — which is the failure #878 was.
//
// Returns "" for a service the cache knows nothing about yet, so a service
// mid-warm gets the ordinary cold path rather than a 421 it would have to
// retry past.
func (c *SnapshotCache) tcpSpellingsIfNoHTTPPortLocked(svc string) string {
	c.clusterMu.RLock()
	_, hasHTTP := c.clusters[svc]
	tcpEntry, hasTCP := c.clusters[proxy.TCPClusterName(svc, c.meshDomain)]
	c.clusterMu.RUnlock()

	if hasHTTP || !hasTCP {
		return ""
	}

	fqdn := proxy.ServiceClusterName(svc, c.meshDomain)
	spellings := fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyL4OutboundPort)
	if p := primaryPortOf(tcpEntry); p != 0 {
		spellings += fmt.Sprintf(" or %s:%d", fqdn, p)
	}
	return spellings
}
