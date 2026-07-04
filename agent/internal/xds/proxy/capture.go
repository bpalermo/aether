package proxy

import (
	"fmt"
	"net"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	original_dstv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/original_dst/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// CaptureHTTPRouteName is the RDS route configuration the per-pod transparent-capture
// listeners use (proposal 018, Phase 3a). It maps cluster.local authorities (the
// generated mesh-Service names) to the registry-backed service clusters.
const CaptureHTTPRouteName = "cap_http"

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
}

// CaptureListenerName returns the per-pod transparent-capture listener name.
func CaptureListenerName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("capture_%s", cniPod.GetName())
}

// GenerateCaptureListener builds the per-pod transparent-capture listener (proposal
// 018, Phase 3a): bound to capturePort inside the pod netns, it accepts the outbound
// traffic the CNI redirects from a mesh ClusterIP:meshPort, recovers the original
// destination, and routes by cluster.local authority (the request Host) to the
// service's registry-backed EDS cluster — the same SPIRE-mTLS egress path the
// explicit :18081 listener serves.
//
// TCP floor (Phase 3a): for non-HTTP services (tcpServices), per-ClusterIP filter
// chains route via tcp_proxy over per-source mTLS to the service's EDS cluster,
// preserving SPIFFE SAN validation and registry health. The HCM chain handles all
// HTTP services; the TCP chains are more specific (destination-IP match) and only
// exist for non-HTTP ClusterIPs so there is no precedence conflict.
//
// withPassthrough (proposal 022, M2a spike): when true, a passthrough filter chain
// is appended as the LAST chain (lowest precedence, after the HCM catch-all). The
// passthrough chain has no FilterChainMatch criteria — but in Envoy, the
// default_filter_chain field (not an entry in filter_chains) is the true
// fall-through. The passthrough is implemented as DefaultFilterChain so it is only
// used when no named chain matches. It routes via the passthrough_original_dst
// ORIGINAL_DST cluster, forwarding the original destination in plain TCP.
//
// Default off (no listener is generated unless transparent capture is on).
func GenerateCaptureListener(cniPod *cniv1.CNIPod, capturePort uint32, meshDomain string, emitStatsPod bool, tcpServices []CaptureTCPService, withPassthrough bool, extensionFilters []*http_connection_managerv3.HttpFilter) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	// Build filter chains. Order matters for Envoy's filter-chain matching:
	//   1. TLS SNI chains (TLSRoute, proposal 018 Phase 3b): server_names + prefix_ranges.
	//      The tls_inspector reads SNI before chain selection; more specific than
	//      destination-IP-only chains.
	//   2. Per-ClusterIP TCP floor chains (TCPRoute or passthrough, Phase 3a/3b):
	//      destination-IP match only (prefix_ranges=/32).
	//   3. Global HCM catch-all: no match criteria (catches all HTTP/gRPC traffic).
	//   4. (withPassthrough only) DefaultFilterChain: routes everything else to the
	//      ORIGINAL_DST passthrough cluster — non-mesh egress in plain TCP.
	//
	// TLS SNI chains are inserted first so Envoy evaluates server_names+IP before
	// the destination-IP-only TCP floor chain (more specific wins).
	chains := make([]*listenerv3.FilterChain, 0, len(tcpServices)*2+1)
	for _, svc := range tcpServices {
		// TLSRoute SNI chains (if any) go before the TCP floor chain.
		tlsChains := BuildCaptureTLSRouteFilterChains(svc, svc.TLSRouteRules)
		chains = append(chains, tlsChains...)
		// TCP floor chain: passthrough or TCPRoute-weighted.
		tc := BuildCaptureTCPRouteFilterChain(svc, svc.TCPRouteRules)
		if tc != nil {
			chains = append(chains, tc)
		}
	}
	chains = append(chains, buildCaptureHTTPFilterChain(cniPod, meshDomain, emitStatsPod, withPassthrough, extensionFilters))

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
		// use_original_dst keeps the recovered destination on the connection.
		ListenerFilters:               buildCaptureListenerFilters(),
		UseOriginalDst:                wrapperspb.Bool(true),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		StatPrefix:                    fmt.Sprintf("capture_%s", cniPod.GetName()),
		TrafficDirection:              corev3.TrafficDirection_OUTBOUND,
		FilterChains:                  chains,
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

// buildCaptureTCPFloorFilterChain builds a per-ClusterIP TCP floor filter chain for
// one non-HTTP mesh service. It matches the service's ClusterIP as the original
// destination (/32 prefix_ranges) and routes via tcp_proxy over the per-source mTLS
// upstream transport socket to the service's EDS cluster. Returns nil for invalid
// ClusterIPs (headless or unset) so callers can skip them.
//
// ALPN: the outbound tcp_proxy dials with ALPN "aether-tcp" (via
// UpstreamTCPTransportSocket). The destination inbound listener matches
// application_protocols:["aether-tcp"] on its TCP floor chain, so mTLS-demux is
// clean: h2 → HCM chains; aether-tcp → TCP floor chains.
//
// Filter-chain precedence: destination-IP is more specific than application-protocol
// in Envoy's match order, BUT this chain only exists for NON-HTTP ClusterIPs, so the
// global HCM catch-all chain never has a precedence conflict with these chains.
func buildCaptureTCPFloorFilterChain(svc CaptureTCPService) *listenerv3.FilterChain {
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
		Filters: []*listenerv3.Filter{
			buildNetworkNamespaceFilterState(),
			buildTCPProxyNetworkFilter(fmt.Sprintf("cap_tcp_%s", svc.ClusterName), svc.ClusterName),
		},
	}
}

// buildCaptureHTTPFilterChain mirrors the outbound HTTP chain but serves the capture
// route table (cluster.local authorities) over RDS. Same readiness/subset/on-demand/
// stats filters and per-source mTLS egress path.
//
// scopeToCleartext (redirect-all / withPassthrough, proposal 022 M2a): match the
// chain on transport_protocol="raw_buffer" AND the http_inspector-detected HTTP
// application protocols, so it catches ONLY cleartext HTTP —
//   - TLS egress (external HTTPS, kube-apiserver): tls_inspector marks it
//     transport_protocol="tls" → no match → DefaultFilterChain ORIGINAL_DST
//     passthrough. (Matching application_protocol ALONE would not exclude TLS —
//     a TLS ClientHello's ALPN h2/http/1.1 looks like HTTP — hence the combined
//     match; on a raw_buffer stream the application protocol comes from
//     http_inspector, not ALPN.)
//   - Cleartext NON-HTTP (raw TCP to a non-mesh destination): http_inspector
//     detects no HTTP → no match → passthrough to the original destination.
//     Without the application_protocols constraint this chain swallowed such
//     streams and answered a fake "HTTP/1.1 400 Bad Request" (#460 e2e finding).
//     Raw TCP to a TCP MESH service is unaffected: its per-VIP floor chain
//     matches on destination IP, which beats any protocol match.
//
// In scoped (non-redirect-all) mode the chain stays a catch-all: only mesh
// ClusterIPs are captured (all cleartext HTTP) and there is no passthrough fallback.
func buildCaptureHTTPFilterChain(cniPod *cniv1.CNIPod, meshDomain string, emitStatsPod bool, scopeToCleartext bool, extensionFilters []*http_connection_managerv3.HttpFilter) *listenerv3.FilterChain {
	hcm := buildHTTPConnectionManager("capture_http", ReporterSource, cniPod.GetName(), cniPod.GetNamespace(), nil)

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
			RouteConfigName: CaptureHTTPRouteName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	fc := &listenerv3.FilterChain{
		Name: fmt.Sprintf("capture_%s", cniPod.GetName()),
		Filters: []*listenerv3.Filter{
			buildNetworkNamespaceFilterState(),
			buildHTTPConnectionManagerFilter(hcm),
		},
	}
	if scopeToCleartext {
		// raw_buffer = cleartext (tls_inspector marks TLS "tls") + detected HTTP
		// (http_inspector). TLS egress AND cleartext non-HTTP both miss this chain
		// and fall to the passthrough DefaultFilterChain (see the func comment).
		fc.FilterChainMatch = &listenerv3.FilterChainMatch{
			TransportProtocol:    "raw_buffer",
			ApplicationProtocols: []string{"http/1.0", "http/1.1", "h2c"},
		}
	}
	return fc
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
// Note on filter chain matching vs DefaultFilterChain:
//   - filter_chains entries with empty FilterChainMatch are evaluated as "any"
//     and ARE compared against other chains (Envoy rejects a listener with
//     conflicting empty-match chains).
//   - DefaultFilterChain is evaluated only after ALL named chains fail to match.
//     It is the correct mechanism for a true "catch everything else" passthrough.
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

// BuildCaptureRouteConfiguration builds the transparent-capture route table: the
// given cluster.local authority -> service-cluster virtual hosts (built by the agent
// from the generated mesh Services, scoped to the node dependency set), plus the
// same on-demand catch-all the outbound listener uses. A captured request for a mesh
// authority (<svc>.<meshDomain>) with no scoped vhost — a cold or off-node service —
// then resolves via ODCDS (proposal 004 cold path) instead of a dead 404.
//
// When redirectAll is true (proposal 022 redirect-all capture), the catch-all's final
// fallthrough — a non-mesh authority in no dependency set, e.g. a real Kubernetes
// Service name a client dials with no HTTPRoute/upstream declaration — routes to the
// ORIGINAL_DST passthrough instead of 404'ing, so plain Service-to-Service
// reachability is preserved (the passthrough_original_dst cluster is only emitted in
// this mode). With scoped capture (redirectAll false) the catch-all keeps its hard
// 404: only mesh VIPs are captured, so a non-mesh authority is genuinely foreign.
// knownTargets pins each known in-scope mesh authority's non-mesh dial spellings
// to its cluster on the redirect-all catch-all, so a captured request to a known
// service never leaks to the ORIGINAL_DST passthrough (kube-proxy) while its
// dedicated cap_http vhost is mid-rebuild. Ignored when redirectAll is false (no
// passthrough to shadow a target).
func BuildCaptureRouteConfiguration(vhosts []*routev3.VirtualHost, meshDomain string, redirectAll bool, knownTargets ...KnownTargetRoute) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         CaptureHTTPRouteName,
		VirtualHosts: append(vhosts, buildOnDemandCatchAllVirtualHost(meshDomain, redirectAll, knownTargets...)),
	}
}
