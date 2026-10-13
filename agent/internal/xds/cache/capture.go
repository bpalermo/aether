package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/meshdns"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
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
				"pod", cniPod.GetNamespace()+"/"+cniPod.GetName(),
				"reason", reason,
				"issue", "873")
		}
	}

	l, err := proxy.GenerateUDPCaptureListener(
		cniPod,
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
		if svc, ok := captureTCPService(e, c.meshDomain, tcpRoutes[e.serviceName], tlsRoutes[e.serviceName]); ok {
			tcpServices = append(tcpServices, svc)
		}
	}
	c.captureMu.RUnlock()

	// The HTTP chains are NOT gated on identity: an HTTP cluster fetches its
	// client certificate per connection and is published either way.
	l, err := proxy.GenerateCaptureListener(cniPod, proxy.SourceIdentityForPod(cniPod, trustDomain), meshconst.ProxyCapturePort, c.meshDomain, c.emitStatsPod, tcpServices, c.captureHTTPPorts(), c.captureRedirectAll, extensionFilters)
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

// captureTCPService renders one capture entry as the listener generator's
// input, and reports false for a service that gets no chain of its own: one
// this node holds no endpoint of, or one with neither a raw-TCP port nor a
// TLSRoute.
//
// Every fact comes from the entry's derived ports. The mesh Service's
// aether.io/app-protocol annotation is not read anywhere (proposal 043,
// Phase 1): it said one thing about the whole service, and for a service with
// a raw-TCP port and an HTTP port it said the wrong one (#1672).
func captureTCPService(e captureTCPEntry, meshDomain string, tcpRoutes, tlsRoutes []proxy.L4ServiceRoute) (proxy.CaptureTCPService, bool) {
	legacyTCP := e.ports.legacyTCPPort()
	tcpPorts := make([]uint32, 0, len(e.ports.tcp))
	for _, p := range e.ports.tcp {
		if p != legacyTCP {
			tcpPorts = append(tcpPorts, p)
		}
	}
	if legacyTCP == 0 && len(tcpPorts) == 0 && len(tlsRoutes) == 0 {
		return proxy.CaptureTCPService{}, false
	}
	return proxy.CaptureTCPService{
		// TCP clusters are separate from HTTP clusters: they use no ALPN on
		// the transport socket, so the destination inbound demuxes them to a
		// tcp_proxy chain.
		ClusterName:   proxy.TCPClusterName(e.serviceName, meshDomain),
		ClusterIP:     e.clusterIP,
		HTTPPorts:     e.ports.http,
		TCPPorts:      tcpPorts,
		LegacyTCPPort: legacyTCP,
		// L4 route rules (Phase 3b): override the passthrough floor chain when
		// a TCPRoute or TLSRoute is attached to this service.
		TCPRouteRules: tcpRoutes,
		TLSRouteRules: tlsRoutes,
	}, true
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
	if !c.nodeWritesAllowed() {
		// A standby (proposal 041): the owning agent is still the writer. The
		// table is kept and written by SetNodeWriteGate's takeover rewrite.
		return
	}
	if err := meshdns.WriteSnapshot(c.meshDNSSnapshotPath, records, generation); err != nil {
		c.log.Warn("failed to persist mesh-DNS snapshot", "path", c.meshDNSSnapshotPath, "error", err)
	}
}

// SetCaptureAuthorities replaces the mesh service -> cluster.local FQDN map (fed by
// the agent's capture reconciler from the generated mesh Services) and signals a
// snapshot rebuild on change so the capture route tables re-derive.
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
	c.captureMu.Lock()
	// The reconciler knows a service's name and VIP; its PORTS are derived by
	// the cache from the registry endpoints (refreshCaptureServicePorts), on
	// the registry's schedule. Carry the last derivation into every entry
	// built here. Dropping it -- as this did until
	// #1094 -- made an unchanged re-projection compare as a change (the derived
	// fields went to zero), so every mesh-Service event rebuilt every capture
	// listener WITHOUT its destination_port chains: a connection to a declared
	// port fell to the any-port shim (past proposal 037 Phase 4, to nothing),
	// and the connections riding the removed chains were drained, until the
	// next registry load put them back. An agent restart interleaves one
	// reconcile per mesh Service with its first registry loads, which is how
	// the reference cluster hit it 12 s after a restart.
	entries := make([]captureTCPEntry, 0, len(services))
	for _, s := range services {
		if s.ServiceName != "" && s.ClusterIP != "" {
			entries = append(entries, captureTCPEntry{
				serviceName: s.ServiceName,
				clusterIP:   s.ClusterIP,
				ports:       c.captureDerived[s.ServiceName],
				declared:    declaredServicePorts(s.DeclaredHTTP, s.DeclaredOther),
			})
		}
	}
	changed := !equalTCPEntries(c.captureTCPServices, entries)
	c.captureTCPServices = entries
	c.captureMu.Unlock()
	// Closed AFTER the listener rebuild below has run when it matters: a waiter
	// that wakes here goes on to open the xDS socket, and the snapshot it serves
	// must already carry these chains. Deferred so every return path closes it.
	defer c.captureProjectedOnce.Do(func() { close(c.captureProjected) })

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
	// The declared ports are an input of the entries' loopback virtual hosts
	// too (a backendRef to a service known by them): rebuild those.
	c.bumpRuleGenLocked(true)
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
	// That refresh is also where a Service's new ports replace an HTTP entry
	// retained for its grace period (retentionEvidence).
	c.signalDependencyChange()
}

// CaptureProjected returns a channel closed once the mesh-Service reconciler
// has delivered its first SetCaptureTCPServices projection (#1094).
//
// That projection defines which per-VIP and per-port TCP chains the capture
// listener carries. A snapshot served before it would replace a restarted
// agent's still-correct listener in Envoy with one that has none of them, so
// the xDS server holds its first serve on this (bounded; see
// AgentXdsServer.SetCaptureGate). Closed, not sent on: any number of waiters.
func (c *SnapshotCache) CaptureProjected() <-chan struct{} { return c.captureProjected }

// captureTCPClusters returns the TCP floor clusters for non-HTTP services, and the
// load assignments they subscribe to. These are separate EDS clusters (prefixed
// "tcp:") that have the same membership as the service's bare-name EDS resource
// but use NO ALPN on their transport socket so the destination inbound routes to
// the TCP floor DEFAULT chain. Called from generateSnapshot.
//
// The floor cluster and its primary-port alias subscribe to their OWN EDS names,
// and the bare service's load assignment is returned under each of them from
// this same pass (proxy.LoadAssignmentAlias), so a floor cluster and its CLA
// always ride the same snapshot. Sharing the bare name with the default HTTP
// cluster was only accidentally safe: Envoy's delta-ADS WatchMap deduplicates
// subscription interest per resource name, and a floor added in a LATER CDS
// update than the default cluster (a TCPRoute attached after the service is in
// the dependency set) sent no subscribe and warmed for the full 15 s
// initial_fetch_timeout -- the #1008 mechanism (aether#1013).
func (c *SnapshotCache) captureTCPClusters() ([]types.Resource, []types.Resource) {
	if !c.captureEnabled {
		return nil, nil
	}

	c.captureMu.RLock()
	entries := make([]captureTCPEntry, len(c.captureTCPServices))
	copy(entries, c.captureTCPServices)
	c.captureMu.RUnlock()

	if len(entries) == 0 {
		return nil, nil
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
		return blackhole, nil
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
	var clas []types.Resource
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
		bare := c.bareServiceCLALocked(e.serviceName)
		// Its own stat key, tcp_<ns>/<svc>: never the bare service key the HTTP
		// cluster reports under (aether#1023, proxy.TCPStatKey). The alias and
		// per-port clusters below carry tcp_<ns>/<svc>_<port>.
		cl := proxy.NewTCPServiceCluster(tcpName, tcpName, proxy.TCPStatKey(e.serviceName))
		// NO SNI for the TCP floor: the egress floor connection must NOT carry the
		// destination port as SNI, or the peer's inbound per-port HCM chain
		// (server_names:[port]) would win over the inbound TCP floor's default chain
		// (server_names > application_protocols > default) — the connection lands on
		// the HCM, which can't parse the raw TCP stream and 503s. An empty SNI lets it
		// fall through to the inbound default floor chain (tcp_proxy to the app).
		proxy.InjectUpstreamTCPMTLS(cl, nodeSpiffeID, validationContextName, sanURIs, "")
		resources = append(resources, cl)
		if cla := proxy.LoadAssignmentAlias(bare, tcpName); cla != nil {
			clas = append(clas, cla)
		}

		portClusters, portCLAs := c.tcpPortClustersLocked(
			e, tcpEntry, tcpName, bare, sanURIs, nodeSpiffeID, validationContextName)
		resources = append(resources, portClusters...)
		clas = append(clas, portCLAs...)
	}
	c.clusterMu.RUnlock()

	return resources, clas
}

// edgeTCPClusters returns TCP floor clusters for services referenced by the edge's
// L4 routes (TCPRoute/TLSRoute parented to a Gateway), and their load
// assignments. Called from generateSnapshot when in edge mode; parallel to
// captureTCPClusters but for the edge identity (EdgeUpstreamTCPTransportSocket
// fetches SDS from spire_agent, not ADS). Like the node proxy's floor, each
// cluster subscribes to its own EDS name and the bare service's load assignment
// is republished under it (aether#1013): the edge also carries the service's
// default HTTP cluster when an HTTPRoute targets it.
//
// The edge has one identity so no per-source matcher is needed: the cluster gets
// a single transport socket presenting the edge SVID with no ALPN and no SNI,
// so the destination inbound demuxes to the TCP floor DEFAULT chain.
func (c *SnapshotCache) edgeTCPClusters() ([]types.Resource, []types.Resource) {
	services := c.collectEdgeTCPServices()
	if len(services) == 0 {
		return nil, nil
	}

	c.localMu.RLock()
	nodeSpiffeID := c.nodeSpiffeID
	c.localMu.RUnlock()
	validationContextName := c.validationContextName()
	if nodeSpiffeID == "" || validationContextName == "" {
		return nil, nil
	}

	c.clusterMu.RLock()
	resources := make([]types.Resource, 0, len(services))
	clas := make([]types.Resource, 0, len(services))
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
		cl := proxy.NewTCPServiceCluster(tcpName, tcpName, proxy.TCPStatKey(svc))
		// Edge variant: fetch SVID/bundle from spire_agent (not ADS), no ALPN, no SNI (TCP floor).
		cl.TransportSocket = proxy.EdgeUpstreamTCPTransportSocket(nodeSpiffeID, validationContextName, sanURIs)
		resources = append(resources, cl)
		bare := c.bareServiceCLALocked(svc)
		if cla := proxy.LoadAssignmentAlias(bare, tcpName); cla != nil {
			clas = append(clas, cla)
		}
		portClusters, portCLAs := c.edgeTCPPortClustersLocked(svc, tcpName, bare, sanURIs, nodeSpiffeID, validationContextName)
		resources = append(resources, portClusters...)
		clas = append(clas, portCLAs...)
	}
	c.clusterMu.RUnlock()

	return resources, clas
}

// edgeTCPPortClustersLocked returns a service's per-port raw-TCP clusters for
// the edge, tcp:<fqdn>:<port> (proposal 043, Phase 1: "Edge: per-port TCP
// clusters are published"), and the load assignments of the ones that are
// aliases. Caller must hold clusterMu.
//
// The same two shapes the node proxy publishes (tcpPortClustersLocked), with
// the edge's own transport socket: the legacy target's port is an alias of
// the floor (no SNI), every other raw-TCP port has its own membership and
// carries its port as SNI.
func (c *SnapshotCache) edgeTCPPortClustersLocked(svc, tcpName string, bare *endpointv3.ClusterLoadAssignment, sanURIs []string, nodeSpiffeID, validationContextName string) ([]types.Resource, []types.Resource) {
	var out, clas []types.Resource
	for _, port := range c.servicePorts[svc].tcp {
		name := proxy.TCPPortClusterName(tcpName, port)
		portEntry, ok := c.clusters[name]
		if !ok {
			continue
		}
		pc := proxy.NewTCPServiceCluster(name, name, proxy.TCPPortStatKey(svc, port))
		if portEntry.loadAssignment == nil {
			pc.TransportSocket = proxy.EdgeUpstreamTCPTransportSocket(nodeSpiffeID, validationContextName, sanURIs)
			out = append(out, pc)
			if cla := proxy.LoadAssignmentAlias(bare, name); cla != nil {
				clas = append(clas, cla)
			}
			continue
		}
		pc.TransportSocket = proxy.EdgeUpstreamTCPPortTransportSocket(nodeSpiffeID, validationContextName, portEntry.sanURIs, strconv.Itoa(int(port)))
		out = append(out, pc)
	}
	return out, clas
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
	// The entry that HOLDS the service's bare load assignment, for the
	// endpoints and for the port alike: an entry of the service that holds none
	// (a retained HTTP or TCP entry beside a live floor entry, #1635) carries
	// the port of a listing that is gone.
	entry, ok := c.bareServiceHolderLocked(svc)
	if !ok {
		return ""
	}
	// entry.sni carries the backend's registered application port.
	if _, ok := entryPort(entry.sni); !ok {
		return ""
	}
	return proxy.UDPClusterName(svc, c.meshDomain)
}

// entryPort parses a cluster entry's sni field (the destination port, rendered
// by strconv.Itoa at build time) into a port. It parses at 16 bits, so a value
// outside 1..65535 is REJECTED rather than narrowed: Atoi + uint32() would wrap
// 2^32+80 to 80 and address the wrong port (CodeQL
// go/incorrect-integer-conversion, #1220).
func entryPort(sni string) (uint32, bool) {
	p, err := strconv.ParseUint(sni, 10, 16)
	if err != nil || p == 0 {
		return 0, false
	}
	return uint32(p), true
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
		// service-level facts (the app port in entry.sni and the bare-name EDS):
		// both from the one entry that holds the load assignment, as the
		// predicate above reads them.
		entry, _ := c.bareServiceHolderLocked(svc)
		// entry.sni carries the backend's registered application port. The UDP floor
		// has no inbound mTLS hop, so udp_proxy must reach that app port directly (not
		// the mesh inbound :18008 the shared bare-name EDS carries) — build an inline
		// app-port UDP load assignment from the service's endpoints.
		port, ok := entryPort(entry.sni)
		if !ok {
			// Unreachable while udpClusterForLocked applies the same parse under
			// the same lock; kept so a malformed port is skipped, never wrapped.
			continue
		}
		la := proxy.UDPLoadAssignment(entry.loadAssignment, udpName, port)
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
		cl := proxy.NewUDPServiceCluster(udpName, proxy.UDPStatKey(svc), la)
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
		if !ok || prev.clusterIP != e.clusterIP || !prev.declared.equal(e.declared) {
			return false
		}
		// The derived ports are part of the identity: a change to them
		// changes the chains, so it must trigger a regeneration. Every slice
		// is sorted at derivation, so ordinary endpoint churn -- pods coming
		// and going with the SAME declared ports -- compares equal.
		//
		// That is the whole of Risk 4's mitigation: regenerating a per-pod
		// capture listener drains its connections, so this comparison has to be
		// over the DERIVED set and not over the reload event.
		if !prev.ports.equal(e.ports) {
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
	// It also decides whether an ext_authz service filter is kept at all,
	// which the loopback virtual hosts carry.
	c.bumpRuleGenLocked(true)
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

// refreshCaptureServicePorts updates each capture service's derived ports
// and, ONLY if any of them changed, rebuilds the per-pod capture listeners so
// the new chains reach Envoy (proposals 037 and 043).
//
// It reuses SetCaptureTCPServices' entry list and its change detection rather
// than adding a second regeneration trigger: one comparison, over the derived
// state, is the whole of Risk 4's mitigation. Regenerating a per-pod capture
// listener drains that listener's connections, and a registry reload happens on
// every pod ADD/DEL anywhere on the node — so a trigger keyed on "a reload
// occurred" rather than "the derived set changed" would turn ordinary churn
// into dropped connections.
func (c *SnapshotCache) refreshCaptureServicePorts(derived map[string]servicePorts) {
	c.captureMu.Lock()
	// Keep the derivation itself, not only its application to today's entries:
	// a service the reconciler projects AFTER this load (the order at agent
	// start) or projects again later gets these ports from
	// SetCaptureTCPServices (#1094). The map is freshly built by the caller
	// and never mutated afterwards, so holding it is safe.
	c.captureDerived = derived
	next := make([]captureTCPEntry, 0, len(c.captureTCPServices))
	for _, e := range c.captureTCPServices {
		e.ports = derived[e.serviceName]
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
	c.rebuildPodListenersLocked("derived port set change", shared)
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

// tcpFloorStartupGrace is how long after start a SPIRE-enabled node may build
// capture listeners without an identity before the #877 warning speaks.
const tcpFloorStartupGrace = time.Minute

// warnTCPFloorWithoutIdentity logs, at most once a minute, that TCP mesh
// services are configured but unroutable for want of identity (#877).
//
// Rate-limited because this is also the normal startup state for a few seconds,
// and a per-listener-build log would be one line per pod per rebuild. It is a
// WARN rather than an INFO because the steady state is a real outage: with
// SPIRE off, every raw-TCP mesh service silently refuses.
//
// With SPIRE on it is also silent for tcpFloorStartupGrace after the cache is
// created (issue #1123): the node agent now builds its local pods' listeners
// from storage WHILE it waits for its SVID, so every restart passes through
// this state, and the identity gate plus the spire-svid readiness check (with
// its own dwell) already own a missing SVID. Past the grace, or with SPIRE
// off, it warns exactly as before.
func (c *SnapshotCache) warnTCPFloorWithoutIdentity(services int) {
	if c.spireEnabled && time.Since(c.createdAt) < tcpFloorStartupGrace {
		return
	}
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
// off it can be missed permanently — which is how node E ran a whole
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
	p, _ := entryPort(entry.sni)
	return p
}

// tcpPortClustersLocked returns a TCP service's port-qualified clusters:
// tcp:<fqdn>:<port> for every port it serves as raw TCP (proposal 037).
// Caller must hold clusterMu.
//
// Two shapes, and the difference is the SNI:
//
//   - The PRIMARY port is an alias. Same membership as the floor -- its own EDS
//     name, with the bare service's load assignment (bare) republished under it
//     and returned alongside (aether#1013) -- and NO SNI, because
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
	bare *endpointv3.ClusterLoadAssignment,
	sanURIs []string,
	nodeSpiffeID, validationContextName string,
) ([]types.Resource, []types.Resource) {
	var out, clas []types.Resource

	if aliasName := proxy.TCPPortClusterName(tcpName, primaryPortOf(tcpEntry)); aliasName != "" {
		// An ALIAS entry holds no load assignment of its own; an entry of that
		// name that does is a per-port cluster, published by the loop below.
		if alias, ok := c.clusters[aliasName]; ok && alias.loadAssignment == nil {
			ac := proxy.NewTCPServiceCluster(aliasName, aliasName, proxy.TCPPortStatKey(e.serviceName, primaryPortOf(tcpEntry)))
			proxy.InjectUpstreamTCPMTLS(ac, nodeSpiffeID, validationContextName, sanURIs, "")
			out = append(out, ac)
			if cla := proxy.LoadAssignmentAlias(bare, aliasName); cla != nil {
				clas = append(clas, cla)
			}
		}
	}

	for _, port := range e.ports.tcp {
		portEntry, ok := c.clusters[proxy.TCPPortClusterName(tcpName, port)]
		if !ok || portEntry.loadAssignment == nil {
			continue
		}
		pc := proxy.NewTCPServiceCluster(
			proxy.TCPPortClusterName(tcpName, port),
			portEntry.loadAssignment.GetClusterName(),
			proxy.TCPPortStatKey(e.serviceName, port),
		)
		proxy.InjectUpstreamTCPMTLS(pc, nodeSpiffeID, validationContextName, portEntry.sanURIs, strconv.Itoa(int(port)))
		out = append(out, pc)
	}
	return out, clas
}

// generatedMeshNames returns, sorted, the mesh name of every service that has
// a generated Service with an address.
func (c *SnapshotCache) generatedMeshNames() []string {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	out := make([]string, 0, len(c.captureTCPServices))
	for _, e := range c.captureTCPServices {
		if name := proxy.ServiceClusterName(e.serviceName, c.meshDomain); name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// declaredByService returns what the generated Services say their real ports
// are, per service (captureTCPEntry.declared). The map is built per call; the
// slices in it are shared and read-only.
func (c *SnapshotCache) declaredByService() map[string]servicePorts {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	out := make(map[string]servicePorts, len(c.captureTCPServices))
	for _, e := range c.captureTCPServices {
		if e.declared.known() {
			out[e.serviceName] = e.declared
		}
	}
	return out
}

// effectivePorts returns, per service, the ports the tables are built from:
// what the registry endpoints say (all), and for a service this node holds no
// endpoint of, what its generated Service declares (declared).
//
// That second half is what makes a COLD request strict: the node routes and
// refuses it by the service's declared ports, exactly as it will once it
// holds an endpoint, instead of fetching whatever cluster the authority
// spells. A cold service's ports carry no legacy target, so its HTTP ports all
// route to their own "<fqdn>:<port>" cluster, which the on-demand filter
// fetches; the bare cluster name is never named for it.
func effectivePorts(all, declared map[string]servicePorts) map[string]servicePorts {
	if len(declared) == 0 {
		return all
	}
	out := make(map[string]servicePorts, len(all)+len(declared))
	for svc, p := range declared {
		out[svc] = p
	}
	for svc, p := range all {
		if p.known() {
			out[svc] = p
		}
	}
	return out
}

// declaredServicePorts is what a generated Service says of a service's ports,
// as the cache keeps it: sorted, a number in one class only (HTTP wins), and
// never a port reserved for the mesh. The reconciler already hands it over
// that way; the cache does not depend on it, because a number in two classes
// puts one authority in two virtual hosts and the proxy rejects the table.
func declaredServicePorts(http, other []uint32) servicePorts {
	keep := func(in []uint32, not []uint32) []uint32 {
		out := make([]uint32, 0, len(in))
		for _, p := range in {
			if p != 0 && !reservedMeshPort(p) && !slices.Contains(not, p) {
				out = append(out, p)
			}
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	h := keep(http, nil)
	return servicePorts{http: h, tcp: keep(other, h)}
}
