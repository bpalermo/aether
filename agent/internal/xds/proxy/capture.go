package proxy

import (
	"fmt"
	"net"
	"time"

	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"google.golang.org/protobuf/types/known/durationpb"

	"aethermesh.dev/agent/internal/xds/config"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	original_dstv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/original_dst/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The capture listener's HTTP chains (proposal 043, "The chains of a capture
// listener"): cap_http for every port but the proxy port, with ONE route table
// that branches on the port the client dialled (capturematcher.go,
// MergeCaptureRouteTables), and cap_http_18081 for the proxy port, served the
// loopback table's virtual hosts (BuildProxyPortCaptureRouteConfiguration).
//
// The agent still BUILDS one table per HTTP port number of the mesh,
// cap_http_<P>, and cap_http_other for every other port: they state the rule
// port by port, and the published cap_http is compiled from them. None of
// those names is published, except cap_http_18081.
const (
	// CaptureHTTPRoutePrefix prefixes the name of a per-port table:
	// cap_http_<P>.
	CaptureHTTPRoutePrefix = "cap_http_"
	// CaptureHTTPOtherRouteName is the per-port table of the port numbers no
	// service declares as HTTP.
	CaptureHTTPOtherRouteName = "cap_http_other"
	// captureTCPBlackholeChainName is the scoped-mode chain of the TCP mesh
	// port (BuildCaptureTCPBlackholeFilterChain).
	captureTCPBlackholeChainName = "cap_tcp_blackhole"
)

// CaptureHTTPRouteName returns the name of the per-port table of HTTP port
// number port: cap_http_<port>. For the proxy port it is also the name of a
// chain and of a published table.
func CaptureHTTPRouteName(port uint32) string {
	return fmt.Sprintf("%s%d", CaptureHTTPRoutePrefix, port)
}

const (
	listenerFilterOriginalDstName   = "envoy.filters.listener.original_dst"
	listenerFilterHTTPInspectorName = "envoy.filters.listener.http_inspector"
)

// CaptureTCPService describes a non-HTTP mesh service that requires a
// per-ClusterIP TCP-proxy floor chain on the transparent-capture listener.
// When the capture listener sees outbound traffic whose original-dst ClusterIP
// matches one of these services, it routes via tcp_proxy over per-source mTLS
// to the service's EDS cluster — preserving SPIFFE SAN validation and
// registry health without demux through the HCM.
type CaptureTCPService struct {
	// ClusterName is the EDS cluster the tcp_proxy forwards to
	// (<service>.<meshDomain>, same as the HTTP route target).
	ClusterName string
	// ClusterIP is the k8s Service ClusterIP matched by the filter chain's
	// prefix_ranges (/32 host match).
	ClusterIP string
	// TCPRouteRules are the L4ServiceRoutes from a TCPRoute parentRef=Service
	// (proposal 018, Phase 3b). When non-empty, the passthrough floor chain is
	// replaced with a weighted_clusters tcp_proxy routing to these backends.
	// Empty = passthrough (Phase 3a behaviour).
	TCPRouteRules []L4ServiceRoute
	// TLSRouteRules are the L4ServiceRoutes from a TLSRoute parentRef=Service.
	// When non-empty, per-SNI filter chains (server_names match) are inserted
	// BEFORE the per-ClusterIP TCP floor chain on the capture listener.
	TLSRouteRules []L4ServiceRoute
	// HTTPPorts are the port numbers the service declares as HTTP. No chain is
	// built from them; on these ports (and on the proxy port, when there is
	// one) TLS to the service's address is not matched against its TLSRoutes,
	// so that it leaves through the passthrough as cleartext HTTP's sibling
	// (captureAddressMatcher).
	HTTPPorts []uint32
	// TCPPorts are the service's raw-TCP application ports other than its
	// legacy target, derived from its endpoints' port_protocols (proposals 037
	// and 043). Each gets its own destination_port-qualified chain to its own
	// cluster, tcp:<fqdn>:<p>, so a client dialing <VIP>:<p> reaches that port.
	TCPPorts []uint32
	// LegacyTCPPort is the service's legacy target (the port its pods declare
	// in endpoint.aether.io/port) when that port is raw TCP, and zero
	// otherwise. It is one more declared raw-TCP port: it gets a chain on its
	// own number, and on the TCP mesh port (:18082), which is the legacy
	// spelling of it (proposal 043, "The legacy spellings during Phases 1 and
	// 2"). Both forward to the floor cluster, tcp:<fqdn>.
	//
	// Those two are the only ports a raw-TCP legacy target is reached on.
	// There is no port-less "any port of the address" chain any more (it was
	// cap_tcp_anyport_<cluster>, removed in chart 3.0.0): a connection to a
	// port the service does not declare is not the service's.
	//
	// No service-level protocol is read to set it: the cache derives it from
	// the registry endpoints, never from the mesh Service's annotation.
	LegacyTCPPort uint32
}

// CaptureListenerName returns the per-pod transparent-capture listener name.
func CaptureListenerName(cniPod *cniv1.CNIPod) string {
	return "capture_" + PodResourceKey(cniPod)
}

// GenerateCaptureListener builds the per-pod transparent-capture listener (proposal
// 018, Phase 3a): bound to capturePort inside the pod netns, it accepts the outbound
// traffic the CNI redirects from a mesh ClusterIP:meshPort, recovers the original
// destination, and routes by cluster.local authority (the request Host) to the
// service's registry-backed EDS cluster — the same SPIRE-mTLS egress path the
// explicit :18081 listener serves.
//
// TCP floor (Phase 3a): for a service's raw-TCP ports (tcpServices), per-address
// chains route via tcp_proxy over per-source mTLS to the service's EDS clusters,
// preserving SPIFFE SAN validation and registry health. The matching tree tries
// them, and the service's TLSRoutes, before the HTTP chains, and falls back to
// the HTTP chains when they do not match (captureChainMatcher).
//
// withPassthrough (proposal 022, M2a spike): when true, the listener's
// default_filter_chain is the passthrough: what the matching tree does not
// select a chain for. It routes via the passthrough_original_dst ORIGINAL_DST
// cluster, forwarding the original destination in plain TCP.
//
// Default off (no listener is generated unless transparent capture is on).
// sourceSpiffeID is the capturing pod's own SPIFFE ID (SourceIdentityForPod),
// stamped into filter state next to the netns on every chain that originates
// mesh traffic (see SourceIdentityCertMapperFilterStateKey). "" reproduces the
// pre-#815 shape exactly.
//
// The listener has two HTTP chains whatever the mesh declares: cap_http and
// cap_http_18081. The caller publishes a route table of each name in the same
// snapshot (#817). Its chains are selected by filter_chain_matcher
// (captureChainMatcher, which says why); no chain carries a
// filter_chain_match.
func GenerateCaptureListener(cniPod *cniv1.CNIPod, sourceSpiffeID string, capturePort uint32, meshDomain string, emitStatsPod bool, tcpServices []CaptureTCPService, withPassthrough bool, extensionFilters []*http_connection_managerv3.HttpFilter) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	// Build the filter chains. Their order selects nothing: the matching tree
	// does (captureChainMatcher), and names them.
	var chains []*listenerv3.FilterChain
	services := make([]captureServiceChains, 0, len(tcpServices))
	for _, svc := range tcpServices {
		sc, ok := buildCaptureServiceChains(svc, sourceSpiffeID)
		if !ok {
			continue
		}
		services = append(services, sc)
		chains = append(chains, sc.chains...)
	}
	// Scoped mode only: catch raw TCP to the TCP mesh port that no service
	// chain claimed, before the HCM catch-all fabricates an HTTP 400 for it
	// (proposal 037; the #460 shape). Redirect-all has the passthrough ->
	// kube-proxy REJECT path instead, which is already attributable.
	if !withPassthrough {
		chains = append(chains, BuildCaptureTCPBlackholeFilterChain(sourceSpiffeID))
	}
	// The connection-level L4 access log (aether#1023) on every tcp_proxy chain
	// built above: floor, per-port, TCPRoute-weighted, the TLSRoute SNI chains and the scoped-mode blackhole. Applied here, over the
	// finished list, rather than in each builder, so a new L4 chain kind cannot
	// be added without one. The passthrough DefaultFilterChain is attached
	// below and is NOT logged: it is every non-mesh egress connection, the
	// volume the log must not carry.
	attachL4AccessLog(chains, cniPod.GetName(), cniPod.GetNamespace())
	chains = append(chains,
		buildCaptureHTTPFilterChain(cniPod, sourceSpiffeID, meshDomain, emitStatsPod, extensionFilters, CaptureHTTPChainName, true),
		buildCaptureHTTPFilterChain(cniPod, sourceSpiffeID, meshDomain, emitStatsPod, extensionFilters, CaptureHTTPRouteName(meshconst.ProxyOutboundPort), false),
	)
	// The matching tree decides; a filter_chain_match left on a chain would be
	// ignored by the proxy, and would only mislead whoever reads the config.
	for _, fc := range chains {
		fc.FilterChainMatch = nil
	}

	l := &listenerv3.Listener{
		Name: CaptureListenerName(cniPod),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					// 0.0.0.0: the CNI REDIRECTs the original ClusterIP dst to this
					// port on the pod's loopback/interface; the bind must catch it.
					Address: defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: capturePort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		// original_dst recovers the pre-REDIRECT ClusterIP:meshPort (SO_ORIGINAL_DST);
		// http_inspector detects HTTP/1 vs HTTP/2; tls_inspector detects TLS on the
		// captured stream for future downstreams that speak TLS at the app layer.
		// The original_dst filter alone keeps the recovered destination on the
		// connection (localAddressRestored()); use_original_dst stays OFF (#1007).
		ListenerFilters: buildCaptureListenerFilters(),
		// The inspectors stall INCONCLUSIVE first writes: a raw-TCP client whose first
		// chunk is <6 bytes (e.g. a 4-byte "p9\r\n") — or a server-first protocol that
		// writes nothing (MySQL-style greeting) — never satisfies http_inspector, so
		// chain selection never runs and Envoy's default 15s listener_filters_timeout
		// CLOSES the conn (continue_on defaults false). Cap the wait at 1s and CONTINUE:
		// matching proceeds with whatever was sniffed — a TCP-service destination still
		// wins on the /32 destination-IP tier (dest IP outranks every protocol tier), so
		// short-first-write and server-first TCP work with at most +1s connect latency.
		// Real HTTP/TLS clients send their preface immediately and never hit the timeout.
		// Found via the tcp-echo probes: payloads p1..p99 (<6B) failed, p100+ worked.
		ListenerFiltersTimeout:           durationpb.New(time.Second),
		ContinueOnListenerFiltersTimeout: true,
		// transparent: IP_TRANSPARENT on every worker socket, applied at PREBIND
		// inside the pod netns (proposal 038). It is what lets the CNI's
		// prerouting `tproxy to :18001` assign a diverted packet to this socket
		// -- without it tproxy is a no-op and the packet finds no socket -- and
		// what lets the accepted socket carry the ORIGINAL destination as its
		// local endpoint, so the filter chains keep matching on VIP:port with no
		// port rewrite and no conntrack NAT. Under today's REDIRECT it is
		// harmless: original_dst still reads SO_ORIGINAL_DST first (utility.cc
		// tries it before the IP_TRANSPARENT fallback), so this lands BEFORE the
		// CNI rules change and bakes under REDIRECT; flipping it restarts every
		// capture listener (new sockets, old drained), which must not coincide
		// with the rule switch. Needs CAP_NET_ADMIN in the userns owning the pod
		// netns -- the init userns here -- which the proxy container grants
		// (pinned by //charts/aether:aether_proxy_net_admin_test).
		Transparent: wrapperspb.Bool(true),
		// use_original_dst is OFF (#1007). The explicit original_dst LISTENER
		// FILTER (buildCaptureListenerFilters, first in the list) is what sets
		// localAddressRestored(), which is all the ORIGINAL_DST passthrough
		// cluster needs (Envoy original_dst_cluster.cc reads the downstream
		// local address only when it was restored) and what every filter chain
		// matches on. The listener FIELD adds nothing to that -- it only adds a
		// second copy of the same filter (listener_impl.cc
		// buildOriginalDstListenerFilter) -- plus the restored-destination
		// HANDOFF (active_tcp_socket.cc): a connection whose restored address
		// matches another listener is handed to "the listener bound to that
		// address", looked up by the address STRING alone. Every pod's inbound
		// listener binds 0.0.0.0:18008 in its own netns, so the lookup has no
		// netns and the most recently added pod's inbound listener wins: a
		// diverted connection to <endpoint>:18008 ran through an unrelated
		// pod's inbound chains, which presented that pod's SVID and failed the
		// client's SAN check. Mesh capture never needs the handoff: every
		// decision is a chain match on the restored destination or the
		// ORIGINAL_DST passthrough. On a diverted flow SO_ORIGINAL_DST succeeds
		// -- the flow is conntrack-tracked, just not NATed -- and returns the
		// same VIP:port getsockname would (the TPROXY spike, e2e/spike/tproxy-phase0b.py T1, removed after it was superseded by the in-tree netns test; see git history before 95788da5).
		UseOriginalDst:                wrapperspb.Bool(false),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		StatPrefix:                    podStatName(CaptureListenerName(cniPod)),
		TrafficDirection:              corev3.TrafficDirection_OUTBOUND,
		FilterChains:                  chains,
		FilterChainMatcher:            captureChainMatcher(services, !withPassthrough),
	}

	// SPIKE/M2a passthrough: the DefaultFilterChain is Envoy's true "no named chain
	// matched" fallback (distinct from filter_chains entries with no match criteria,
	// which would conflict with the HCM catch-all). Any connection whose original-dst
	// doesn't match a mesh-service ClusterIP (TCP floor chain) and doesn't look like
	// HTTP (HCM chain) falls through here and is forwarded plaintext to its real dst
	// via the ORIGINAL_DST cluster. This covers: HTTPS-to-external, TLS to kube-apiserver,
	// any non-HTTP non-mesh service, and in-cluster-but-not-mesh services.
	if withPassthrough {
		l.DefaultFilterChain = BuildCapturePassthroughFilterChain()
	}

	return l, nil
}

// buildCaptureListenerFilters: recover the original destination, then sniff HTTP and TLS.
// original_dst must run first (recovers SO_ORIGINAL_DST before any inspection);
// http_inspector and tls_inspector detect the application protocol on the captured stream.
func buildCaptureListenerFilters() []*listenerv3.ListenerFilter {
	return []*listenerv3.ListenerFilter{
		listenerFilter(listenerFilterOriginalDstName, &original_dstv3.OriginalDst{}),
		listenerFilter(listenerFilterHTTPInspectorName, &http_inspectorv3.HttpInspector{}),
		tlsInspector(),
	}
}

// buildCaptureTCPPortFilterChain builds a destination_port-qualified TCP chain
// for ONE of a service's non-primary raw-TCP ports (proposal 037).
//
// Envoy evaluates destination_port ahead of prefix_ranges, and the capture
// listener's original_dst listener filter restores the original destination
// before chain selection, so the port this matches is the one the client
// actually dialed. That ordering is what lets a
// per-port TCP chain coexist with the HCM catch-all on the same VIP: a chain
// matching only the ClusterIP would outrank the HCM's application_protocols
// match and swallow the service's HTTP traffic, which is precisely why a
// service could not be both protocols before.
//
// The chain targets the port's own cluster, tcp:<fqdn>:<port>, not the floor
// cluster: the floor forwards to the pod's PRIMARY port, so routing a
// non-primary port through it would deliver :9000 traffic to :8080.
func buildCaptureTCPPortFilterChain(svc CaptureTCPService, port uint32, sourceSpiffeID string) *listenerv3.FilterChain {
	if svc.ClusterIP == "" || svc.ClusterName == "" || port == 0 || port > 65535 {
		return nil
	}
	if net.ParseIP(svc.ClusterIP) == nil {
		return nil
	}
	name := fmt.Sprintf("cap_tcp_%s_%d", svc.ClusterName, port)
	return &listenerv3.FilterChain{
		Name: name,
		FilterChainMatch: &listenerv3.FilterChainMatch{
			PrefixRanges: []*corev3.CidrRange{
				{AddressPrefix: svc.ClusterIP, PrefixLen: wrapperspb.UInt32(32)},
			},
			DestinationPort: wrapperspb.UInt32(port),
		},
		Filters: append(
			BuildSourceFilterStates(sourceSpiffeID),
			buildTCPProxyNetworkFilter(name, TCPPortClusterName(svc.ClusterName, port)),
		),
	}
}

// buildCaptureTCPFloorFilterChain builds a per-ClusterIP TCP floor filter chain for
// one non-HTTP mesh service. It matches the service's ClusterIP as the original
// destination (/32 prefix_ranges) and routes via tcp_proxy over the per-source mTLS
// upstream transport socket to the service's EDS cluster. Returns nil for invalid
// ClusterIPs (headless or unset) so callers can skip them.
//
// ALPN: the outbound tcp_proxy dials with NO ALPN (via
// MeshUpstreamTCPTransportSocket). The destination inbound listener's HTTP
// chains match application_protocols:["h2"], so mTLS-demux is clean: h2 → HCM
// chains; no ALPN → the TCP floor's default chain.
//
// Filter-chain precedence: destination-IP is more specific than application-protocol
// in Envoy's match order, BUT this chain only exists for NON-HTTP ClusterIPs, so the
// global HCM catch-all chain never has a precedence conflict with these chains.
func buildCaptureTCPFloorFilterChain(svc CaptureTCPService, sourceSpiffeID string) *listenerv3.FilterChain {
	if svc.ClusterIP == "" || svc.ClusterName == "" {
		return nil
	}
	if net.ParseIP(svc.ClusterIP) == nil {
		return nil
	}
	return &listenerv3.FilterChain{
		Name: fmt.Sprintf("cap_tcp_%s", svc.ClusterName),
		FilterChainMatch: &listenerv3.FilterChainMatch{
			// Match only the exact ClusterIP so this chain intercepts only TCP
			// traffic destined for this specific non-HTTP service VIP.
			PrefixRanges: []*corev3.CidrRange{
				{AddressPrefix: svc.ClusterIP, PrefixLen: wrapperspb.UInt32(32)},
			},
		},
		Filters: append(
			BuildSourceFilterStates(sourceSpiffeID),
			buildTCPProxyNetworkFilter(fmt.Sprintf("cap_tcp_%s", svc.ClusterName), svc.ClusterName),
		),
	}
}

// buildCaptureHTTPFilterChain builds one of the listener's two HTTP chains:
// cap_http, or the proxy port's cap_http_18081. name is both the chain's name
// and its RDS route table. dialled adds the filter state that records the
// port the client dialled, which cap_http's table branches on.
//
// Each mirrors the outbound HTTP chain (same readiness/subset/on-demand/stats
// filters, same per-source mTLS egress path).
//
// The chain carries no match of its own: the listener's matching tree sends a
// connection here only when it is cleartext HTTP (redirect-all), or on its
// port alone (scoped capture, which only unit tests build). Cleartext HTTP is
// transport_protocol "raw_buffer" (tls_inspector marks TLS "tls") with an
// application protocol http_inspector detected:
//   - TLS egress (external HTTPS, kube-apiserver) is not raw_buffer and
//     leaves through the passthrough. (The application protocol alone would
//     not exclude it: a TLS ClientHello's ALPN h2/http/1.1 looks like HTTP.)
//   - Cleartext non-HTTP (raw TCP to a non-mesh destination) has no HTTP
//     application protocol and leaves through the passthrough too. Without
//     that condition an HTTP chain swallowed such streams and answered a fake
//     "HTTP/1.1 400 Bad Request" (#460).
func buildCaptureHTTPFilterChain(cniPod *cniv1.CNIPod, sourceSpiffeID, meshDomain string, emitStatsPod bool, extensionFilters []*http_connection_managerv3.HttpFilter, name string, dialled bool) *listenerv3.FilterChain {
	// One stat prefix for both chains: the HTTP stats of a pod's captured
	// traffic stay one family whatever port was dialled.
	hcm := buildHTTPConnectionManager("capture_http", ReporterSource, cniPod.GetName(), cniPod.GetNamespace(), nil, appFacingIdleTimeout)

	prefix := []*http_connection_managerv3.HttpFilter{
		readinessHttpFilter(),
		subsetHeadersHttpFilter(),
		onDemandHttpFilter(),
		outboundStatsFilter(cniPod, meshDomain, emitStatsPod),
	}
	// Escape-hatch extension filters (proposal 025): the union of allow-listed filters
	// referenced by this listener's routes, default-disabled. Each referencing route
	// re-enables + configures its filter via typed_per_filter_config (which can only
	// override a filter already in the chain). Inert when none.
	prefix = append(prefix, extensionFilters...)
	hcm.HttpFilters = append(prefix, hcm.HttpFilters...)

	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: name,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	// The caller's namespace, for a short name in Host (shortname.go).
	networkFilters := BuildSourceFilterStates(sourceSpiffeID)
	if ns := cniPod.GetNamespace(); ns != "" {
		networkFilters = append(networkFilters, BuildCallerNamespaceFilterState(ns))
	}
	if dialled {
		networkFilters = append(networkFilters, BuildDialledPortFilterState())
	}
	return &listenerv3.FilterChain{
		Name:    name,
		Filters: append(networkFilters, buildHTTPConnectionManagerFilter(hcm)),
	}
}

// BuildCapturePassthroughFilterChain returns the DefaultFilterChain for the
// redirect-all capture listener (proposal 022, M2a spike). It is used as
// Listener.DefaultFilterChain — the fallback when no named filter chain matches.
//
// It routes ALL unmatched TCP traffic (non-mesh destinations) to the
// passthrough_original_dst ORIGINAL_DST cluster, which connects to the original
// pre-REDIRECT destination in plain TCP. This is the passthrough "egress intact"
// proof path: HTTPS to github.com, DNS, kube-apiserver, non-mesh in-cluster
// services — all of these have NO matching per-ClusterIP TCP floor chain and
// (being non-HTTP or TLS-encrypted) also do not match the HCM chain's route
// table, so they fall here.
//
// DefaultFilterChain is what the listener's matching tree falls back to when
// it selects no chain: the true "catch everything else" passthrough.
//
// No buildNetworkNamespaceFilterState filter is added: the passthrough path needs
// the network namespace to resolve the ORIGINAL_DST address, but the namespace
// is already known to Envoy via the listener binding — the ORIGINAL_DST cluster
// type uses the SO_ORIGINAL_DST sockopt recovered by the original_dst listener
// filter, not filter state.
func BuildCapturePassthroughFilterChain() *listenerv3.FilterChain {
	return &listenerv3.FilterChain{
		Name: "cap_passthrough",
		Filters: []*listenerv3.Filter{
			buildTCPProxyNetworkFilter("cap_passthrough", PassthroughClusterName),
		},
	}
}

// BuildCaptureRouteConfiguration builds ONE of the per-port capture tables
// (MergeCaptureRouteTables compiles them into the published cap_http): name is
// the table's (cap_http_<P> or cap_http_other), vhosts the
// virtual hosts the agent built for it from the mesh Services and the registry
// endpoints, and catchAll the table's own lowest-priority "*" virtual host
// (BuildCaptureCatchAllVirtualHost).
//
// Like the outbound table it is emitted unconditionally, catch-all and all,
// and it owns its virtual_hosts slice rather than appending onto the caller's
// (see BuildOutboundRouteConfiguration).
//
// short, when set, adds the short names of the table (ShortNameVirtualHosts),
// built from vhosts against catchAll: a short name that is no mesh name for
// its caller gets what catchAll gives any Host that is no mesh name.
func BuildCaptureRouteConfiguration(name string, vhosts []*routev3.VirtualHost, catchAll *routev3.VirtualHost, short *ShortNames) *routev3.RouteConfiguration {
	all := make([]*routev3.VirtualHost, 0, 2*len(vhosts)+1)
	all = append(all, vhosts...)
	if short != nil {
		all = append(all, shortNameVirtualHosts(vhosts, short.MeshDomain, nil, nil, catchAll.GetRoutes(), short.MeshOnly)...)
	}
	if catchAll != nil {
		all = append(all, catchAll)
	}
	return &routev3.RouteConfiguration{
		Name:         name,
		VirtualHosts: oneClaimPerDomain(name, all),
	}
}

// BuildCaptureTCPBlackholeFilterChain returns the scoped-mode chain that
// catches raw TCP to the TCP mesh port on a VIP that claims no TCP port
// (proposal 037).
//
// The matching tree selects it on the destination port alone, after every
// service address's branch: it takes only what none of those claimed
// (captureHTTPBranch).
//
// Emitted in scoped capture only. In redirect-all mode the passthrough chain
// forwards to the original destination, the generated mesh Service has no
// backing endpoint for that port, and kube-proxy REJECTs — already deterministic
// and already attributable, so a blackhole there would only hide it.
func BuildCaptureTCPBlackholeFilterChain(sourceSpiffeID string) *listenerv3.FilterChain {
	return &listenerv3.FilterChain{
		Name: "cap_tcp_blackhole",
		FilterChainMatch: &listenerv3.FilterChainMatch{
			DestinationPort: wrapperspb.UInt32(meshconst.ProxyL4OutboundPort),
		},
		Filters: append(
			BuildSourceFilterStates(sourceSpiffeID),
			buildTCPProxyNetworkFilter("cap_tcp_blackhole", BlackholeClusterName),
		),
	}
}

// qualifyChainByPort returns a copy of src renamed, and matched additionally on
// destination_port when port is non-zero (proposal 037). When statPrefix is
// not empty the copy's tcp_proxy reports under it; otherwise it keeps the
// prefix src has.
//
// Cloning rather than mutating: the caller builds one chain and derives several
// spellings from it, and the TCPRoute-weighted and passthrough shapes must stay
// identical across them. Sharing the proto would make a later edit to one
// spelling silently change the others.
func qualifyChainByPort(src *listenerv3.FilterChain, port uint32, name, statPrefix string) *listenerv3.FilterChain {
	out, _ := proto.Clone(src).(*listenerv3.FilterChain)
	out.Name = name
	if out.FilterChainMatch == nil {
		out.FilterChainMatch = &listenerv3.FilterChainMatch{}
	}
	if port != 0 {
		out.FilterChainMatch.DestinationPort = wrapperspb.UInt32(port)
	}
	if statPrefix == "" {
		return out
	}
	// A spelling that is counted separately gets its own tcp_proxy stat
	// prefix: the TCP mesh port and the legacy target's own port each count
	// under their chain's name.
	for _, f := range out.GetFilters() {
		tc := &tcp_proxyv3.TcpProxy{}
		if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(tc) != nil {
			continue
		}
		tc.StatPrefix = statPrefix
		if any, err := anypb.New(tc); err == nil {
			f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: any}
		}
	}
	return out
}

// legacyTCPChains returns the chains of a service whose legacy target is raw
// TCP (proposal 043, "The legacy spellings during Phases 1 and 2"): the TCP
// mesh port and the legacy target's own port, and nothing else.
//
// Each is a copy of the same floor chain -- passthrough or TCPRoute-weighted
// -- so they cannot drift from one another.
//
// Until chart 3.0.0 a service with no HTTP port also had a port-less chain,
// cap_tcp_anyport_<cluster>, on which ANY other port of its address reached
// the legacy target. It is gone: a raw-TCP service is reached on the ports it
// declares. A connection to another port of its address matches no chain of
// the service and takes the listener's default, the passthrough to the
// address that was dialled, which has no such port.
func legacyTCPChains(svc CaptureTCPService, sourceSpiffeID string) []*listenerv3.FilterChain {
	tc := BuildCaptureTCPRouteFilterChain(svc, svc.TCPRouteRules, sourceSpiffeID)
	if tc == nil {
		return nil
	}
	var out []*listenerv3.FilterChain
	claimed := []uint32{meshconst.ProxyL4OutboundPort}
	if svc.LegacyTCPPort != meshconst.ProxyL4OutboundPort {
		claimed = append(claimed, svc.LegacyTCPPort)
	}
	for _, p := range claimed {
		name := fmt.Sprintf("cap_tcp_%s_%d", svc.ClusterName, p)
		out = append(out, qualifyChainByPort(tc, p, name, name))
	}
	return out
}

// attachL4AccessLog sets the L4 access log (buildL4AccessLog) on every
// tcp_proxy in chains (aether#1023). A no-op when access logging is off, so a
// disabled mesh's capture listener is byte-identical to before.
func attachL4AccessLog(chains []*listenerv3.FilterChain, podName, podNamespace string) {
	al := buildL4AccessLog(podName, podNamespace)
	if al == nil {
		return
	}
	for _, fc := range chains {
		for _, f := range fc.GetFilters() {
			tc := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(tc) != nil {
				continue
			}
			tc.AccessLog = al
			f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(tc)}
		}
	}
}
