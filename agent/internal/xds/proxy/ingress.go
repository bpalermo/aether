package proxy

import (
	"fmt"
	"strconv"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	healthcheckv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// inboundTCPFloorStatPrefix is the stat prefix for the inbound TCP-floor tcp_proxy
// filter. Per-pod naming follows the same aether.pod tag extraction shape.
const inboundTCPFloorStatPrefix = "in_tcp"

const (
	// defaultInboundAddress is the bind address for the per-pod inbound listener
	// (all interfaces within the pod's network namespace).
	defaultInboundAddress = "0.0.0.0"
	// defaultInboundPort is the mesh inbound port. The per-pod inbound listener binds
	// it inside the pod's network namespace, and source proxies dial the destination
	// pod at <pod_ip>:defaultInboundPort. In aether's 18xxx range, out of Istio's
	// reserved 15000-15090 band — 15008 is Istio's HBONE number (proposal 030,
	// Phase B: dialers flipped here after the whole fleet dual-bound in Phase A).
	defaultInboundPort = 18008
	// legacyInboundPort is the pre-030 inbound port, still bound (additional
	// address) so proxies one release behind — still dialing 15008 — keep
	// working through the Phase B roll. Phase C (next release, after the fleet
	// fully rolls) drops this bind.
	legacyInboundPort = 15008

	// MeshLivePath is the liveness path answered locally by the inbound listener: a
	// 200 proves the proxy config is loaded and the listener is serving, and (since
	// the request arrived over the listener's mTLS) that mTLS termination is up.
	MeshLivePath = "/-/-/live"
	// MeshReadyPath is the readiness path: it returns 200 only when, in addition to
	// the liveness conditions, the pod's application health-probe cluster is healthy.
	MeshReadyPath = "/-/-/ready"

	livenessHealthCheckFilterName  = "envoy.filters.http.health_check.live"
	readinessHealthCheckFilterName = "envoy.filters.http.health_check.ready"
)

// InboundListenerName returns the name of a pod's inbound listener.
func InboundListenerName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("inbound_%s", cniPod.GetName())
}

// NewInboundListener builds a pod's inbound listener. It is bound into the pod's
// network namespace at :defaultInboundPort so the pod is reachable at
// <pod_ip>:defaultInboundPort, terminates mTLS presenting the pod's own SVID (so
// callers cryptographically verify they reached this pod, not just the node),
// requires and validates the caller's workload SVID, sets XFCC natively from the
// verified peer (SANITIZE_SET), and forwards the request to the pod's application
// on loopback (app_<pod>). Because the listener lives in the pod's netns, it follows
// the pod's lifecycle (drains on removal) and pod-scoped network policy applies to it.
//
// When cleartext is true (SPIRE disabled) the listener accepts CLEARTEXT instead
// of terminating mTLS: no SVID is issued, the SPIRE bridge delivers no SDS
// secrets, and the outbound clusters already dial cleartext (h2c) — so the inbound
// side must accept cleartext to keep the mesh hop routable. In that mode the
// listener carries a single default HCM chain (no transport socket, AUTO codec so
// h2c and HTTP/1 are both detected, no ALPN/SNI demux, no XFCC since there is no
// verified peer). This is for SPIRE-off testing/conformance (the suite asserts
// routing correctness, not mTLS); the production posture keeps per-pod mTLS.
func NewInboundListener(cniPod *cniv1.CNIPod, trustDomain string, emitStatsPod bool, cleartext bool, extensionFilters []*http_connection_managerv3.HttpFilter, inboundFilter *ExtensionFilter) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	tlsCertificateSecretName := SpiffeIDFromPod(cniPod, trustDomain)
	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)

	var chains []*listenerv3.FilterChain
	var listenerFilters []*listenerv3.ListenerFilter
	if cleartext {
		// No tls_inspector listener filter: there is no TLS to inspect, and a single
		// default HCM chain serves every request.
		chains = []*listenerv3.FilterChain{buildInboundCleartextFilterChain(cniPod, emitStatsPod, extensionFilters, inboundFilter)}
	} else {
		listenerFilters = buildInboundListenerFilters()
		chains = buildInboundFilterChains(cniPod, tlsCertificateSecretName, validationContextName, emitStatsPod, extensionFilters, inboundFilter)
	}

	return &listenerv3.Listener{
		Name:                          InboundListenerName(cniPod),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultInboundPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		// Proposal 030 Phase B: the legacy port stays bound (same netns, same
		// filter chains) so one-release-stale dialers still connect; Phase C
		// drops it after this release fully rolls. Same-listener
		// additional address (not a second listener) keeps stats/drain unified.
		AdditionalAddresses: []*listenerv3.AdditionalAddress{{
			Address: &corev3.Address{
				Address: &corev3.Address_SocketAddress{
					SocketAddress: &corev3.SocketAddress{
						Protocol: corev3.SocketAddress_TCP,
						Address:  defaultInboundAddress,
						PortSpecifier: &corev3.SocketAddress_PortValue{
							PortValue: legacyInboundPort,
						},
						NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
					},
				},
			},
		}},
		// Per-pod listener stats are kept deliberately (downstream_cx_* per pod
		// is the connection-leak debugging signal — see the 2026-06-11 cx-leak).
		// The "inbound_<pod>" shape is what the aether.pod stats_tag extracts, so
		// exports collapse to listener.inbound.* labeled by pod while the stats
		// stay per-pod. HCM stats (5x larger) aggregate node-wide instead.
		StatPrefix:       fmt.Sprintf("inbound_%s", cniPod.GetName()),
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  listenerFilters,
		FilterChains:     chains,
	}, nil
}

// buildInboundCleartextFilterChain builds the single default HCM chain used when
// SPIRE is disabled: no transport socket (plain TCP), AUTO codec (so cleartext h2c
// from the source proxy and HTTP/1 are both handled), routing every request to the
// pod's primary application cluster. XFCC is not set (no verified peer identity).
func buildInboundCleartextFilterChain(cniPod *cniv1.CNIPod, emitStatsPod bool, extensionFilters []*http_connection_managerv3.HttpFilter, inboundFilter *ExtensionFilter) *listenerv3.FilterChain {
	defaultPort := AppPortFromPod(cniPod)
	rc := buildInboundRouteConfiguration(AppClusterName(cniPod, defaultPort))
	applyInboundFilter(rc, inboundFilter)
	hcm := buildHTTPConnectionManager("inbound", ReporterDestination, cniPod.GetName(), cniPod.GetNamespace(), rc)
	filters := []*http_connection_managerv3.HttpFilter{
		buildLivenessHealthCheckFilter(),
		buildReadinessHealthCheckFilter(HealthProbeClusterName(cniPod)),
		inboundStatsFilter(cniPod, emitStatsPod),
	}
	filters = append(filters, extensionFilters...)
	hcm.HttpFilters = append(filters, routerHttpFilter())
	return &listenerv3.FilterChain{
		Name:             fmt.Sprintf("in_%s", cniPod.GetName()),
		FilterChainMatch: nil, // default chain: cleartext has no ALPN/SNI to demux on
		Filters:          []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
		// No TransportSocket: cleartext (SPIRE off).
	}
}

// buildInboundFilterChains builds the full set of inbound filter chains, demuxed by
// Envoy's match precedence (server_names > application_protocols > default):
//
//  1. HCM chains: one per served port (SNI-matched on the port number) plus a
//     no-SNI chain matching application_protocols:["h2"] — the mesh HTTP/2 transport.
//
//  2. TCP floor chain: the DEFAULT (no match). The source proxy's tcp_proxy egress
//     sends NO ALPN, so it matches no HCM chain (no "h2", no port SNI) and lands
//     here, terminating mTLS and routing via tcp_proxy to the pod's app on loopback.
//     Demux is the standard "h2" ALPN for HTTP vs no-ALPN for TCP — no bespoke token.
//
// SNI is port routing only — identity stays the terminated mTLS SVID.
func buildInboundFilterChains(cniPod *cniv1.CNIPod, tlsCertificateSecretName, validationContextName string, emitStatsPod bool, extensionFilters []*http_connection_managerv3.HttpFilter, inboundFilter *ExtensionFilter) []*listenerv3.FilterChain {
	defaultPort := AppPortFromPod(cniPod)
	ports := AppPortsFromPod(cniPod)

	chains := make([]*listenerv3.FilterChain, 0, len(ports)+2)

	// TCP floor chain: the DEFAULT (no match) → tcp_proxy to the primary app loopback
	// port. A no-ALPN mTLS connection (the source tcp_proxy egress) lands here.
	chains = append(chains, buildInboundTCPFloorFilterChain(cniPod, defaultPort, tlsCertificateSecretName, validationContextName))

	// No-SNI HCM chain: matches application_protocols ["h2"] (the mesh HTTP/2
	// transport) on the primary port. Per-port SNI chains follow.
	chains = append(chains, buildInboundFilterChain(cniPod, "", defaultPort, tlsCertificateSecretName, validationContextName, emitStatsPod, extensionFilters, inboundFilter))
	// One chain per served port, SNI-matched on the port number.
	for _, port := range ports {
		chains = append(chains, buildInboundFilterChain(cniPod, strconv.Itoa(int(port)), port, tlsCertificateSecretName, validationContextName, emitStatsPod, extensionFilters, inboundFilter))
	}
	return chains
}

// buildInboundTCPFloorFilterChain builds the mTLS-terminating TCP floor chain for
// the inbound listener. It is the listener's DEFAULT chain (no match criteria): a
// no-ALPN mTLS connection — the source proxy's tcp_proxy floor egress, which sends
// no ALPN — matches nothing more specific (HTTP chains require "h2" or a port SNI)
// and lands here, terminating mTLS and routing all bytes via tcp_proxy to the pod's
// primary application cluster (app_<pod>_<defaultPort>) on loopback.
func buildInboundTCPFloorFilterChain(cniPod *cniv1.CNIPod, defaultPort uint16, tlsCertificateSecretName, validationContextName string) *listenerv3.FilterChain {
	appCluster := AppClusterName(cniPod, defaultPort)
	return &listenerv3.FilterChain{
		Name:             fmt.Sprintf("in_tcp_%s", cniPod.GetName()),
		FilterChainMatch: nil, // default chain: no ALPN / no SNI → the TCP floor
		TransportSocket:  DownstreamTransportSocket(tlsCertificateSecretName, validationContextName),
		Filters: []*listenerv3.Filter{
			buildTCPProxyNetworkFilter(fmt.Sprintf("%s_%s", inboundTCPFloorStatPrefix, cniPod.GetName()), appCluster),
		},
	}
}

// buildInboundFilterChain builds an mTLS-terminating HTTP filter chain for one
// served port: it forwards all requests to that port's app cluster and sets
// XFCC from the verified peer certificate's URI SAN (the caller's SVID). When
// sni is non-empty the chain is SNI-matched (server_names); the empty-sni chain
// is the default (no match criteria). chainPort selects both the app cluster
// and the chain name suffix.
func buildInboundFilterChain(cniPod *cniv1.CNIPod, sni string, chainPort uint16, tlsCertificateSecretName, validationContextName string, emitStatsPod bool, extensionFilters []*http_connection_managerv3.HttpFilter, inboundFilter *ExtensionFilter) *listenerv3.FilterChain {
	rc := buildInboundRouteConfiguration(AppClusterName(cniPod, chainPort))
	applyInboundFilter(rc, inboundFilter)
	hcm := buildHTTPConnectionManager("inbound", ReporterDestination, cniPod.GetName(), cniPod.GetNamespace(), rc)
	// Liveness/readiness are answered locally before the router; everything else
	// passes through to the pod's application. The stats filter sits after the
	// health-check filters (so locally-answered probe requests are not counted)
	// and before the router, recording the destination-reported edge at the log
	// phase (proposal 007 Phase 2).
	filters := []*http_connection_managerv3.HttpFilter{
		buildLivenessHealthCheckFilter(),
		buildReadinessHealthCheckFilter(HealthProbeClusterName(cniPod)),
		inboundStatsFilter(cniPod, emitStatsPod),
	}
	// Escape-hatch extension entries (default-disabled; 027 M3): probes above stay
	// exempt, the INBOUND filter's TPFC (below) opts the app traffic in.
	filters = append(filters, extensionFilters...)
	hcm.HttpFilters = append(filters, routerHttpFilter())
	// SANITIZE_SET replaces any client-supplied XFCC with details derived from the
	// verified peer certificate, exposing the caller's SPIFFE ID (URI SAN) to the app.
	hcm.ForwardClientCertDetails = http_connection_managerv3.HttpConnectionManager_SANITIZE_SET
	hcm.SetCurrentClientCertDetails = &http_connection_managerv3.HttpConnectionManager_SetCurrentClientCertDetails{
		Subject: wrapperspb.Bool(true),
		Uri:     true,
	}

	name := fmt.Sprintf("in_%s", cniPod.GetName())
	// The no-SNI HCM chain matches application_protocols:["h2"] (the mesh's HTTP/2
	// transport) so a no-ALPN mTLS connection — the TCP floor — falls through to the
	// floor's default chain instead. Per-port chains match the port SNI (more
	// specific than application_protocols, so they win for HTTP regardless of ALPN).
	match := &listenerv3.FilterChainMatch{ApplicationProtocols: []string{"h2"}}
	if sni != "" {
		name = fmt.Sprintf("in_%s_%s", cniPod.GetName(), sni)
		match = &listenerv3.FilterChainMatch{ServerNames: []string{sni}}
	}

	return &listenerv3.FilterChain{
		Name:             name,
		FilterChainMatch: match,
		Filters:          []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
		TransportSocket:  DownstreamTransportSocket(tlsCertificateSecretName, validationContextName),
	}
}

// buildLivenessHealthCheckFilter answers MeshLivePath locally with 200. Reaching it
// means the listener is serving with its config loaded; because the listener
// requires mTLS, a successful response also proves mTLS termination is up. It does
// not depend on the application, so it stays green while the app is (re)starting.
func buildLivenessHealthCheckFilter() *http_connection_managerv3.HttpFilter {
	return buildHealthCheckFilter(livenessHealthCheckFilterName, MeshLivePath, nil)
}

// buildReadinessHealthCheckFilter answers MeshReadyPath with 200 only when the pod's
// application health-probe cluster (health_<pod>, which actively health-checks the
// app's readiness path) is healthy; otherwise it returns 503. This composes the
// liveness conditions with actual application readiness.
func buildReadinessHealthCheckFilter(healthClusterName string) *http_connection_managerv3.HttpFilter {
	return buildHealthCheckFilter(readinessHealthCheckFilterName, MeshReadyPath, map[string]*typev3.Percent{
		healthClusterName: {Value: 100},
	})
}

// buildHealthCheckFilter builds a non-pass-through health_check HTTP filter that
// intercepts requests whose :path exactly matches path and answers them locally.
// When clusterMinHealthy is set, it returns 503 unless every named cluster meets its
// minimum healthy percentage. Requests on other paths pass through to the router.
func buildHealthCheckFilter(name, path string, clusterMinHealthy map[string]*typev3.Percent) *http_connection_managerv3.HttpFilter {
	hc := &healthcheckv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{Exact: path},
					},
				},
			},
		},
		ClusterMinHealthyPercentages: clusterMinHealthy,
	}
	return httpFilter(name, hc)
}

// buildInboundRouteConfiguration routes all inbound requests to the per-pod
// application cluster, which forwards to the pod's own application on loopback.
func buildInboundRouteConfiguration(appClusterName string) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name: "in_http",
		// The app_<pod> cluster churns on pod restart; don't let the inline route's
		// cluster reference wedge the listener during the delta-xDS make-before-break
		// window if it is momentarily unknown.
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts: []*routev3.VirtualHost{
			{
				Name:    "catch_all",
				Domains: []string{"*"},
				Routes: []*routev3.Route{
					{
						Match: &routev3.RouteMatch{
							PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
						},
						Action: &routev3.Route_Route{
							Route: &routev3.RouteAction{
								ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: appClusterName},
							},
						},
					},
				},
			},
		},
	}
}

// applyInboundFilter enables the pod's destination-side (INBOUND scope, 027 M3)
// filter on the inbound route config's vhost — every app-bound request on this
// pod is checked; the health/readiness filters run BEFORE the extension anchor so
// probes stay exempt.
func applyInboundFilter(rc *routev3.RouteConfiguration, inboundFilter *ExtensionFilter) {
	if rc == nil || inboundFilter == nil {
		return
	}
	for _, vh := range rc.GetVirtualHosts() {
		ApplyServiceChainFilter(vh, inboundFilter)
	}
}
