package proxy

// L4 route types and Envoy config builders for TCPRoute, TLSRoute, and UDPRoute
// (proposal 018, Phase 3b — east-west GAMMA-style L4 routing).
//
// TCPRoute (parentRef=Service): weighted backends replace the passthrough floor
// chain. The capture listener's per-ClusterIP floor chain switches from a single-
// cluster tcp_proxy to a weighted_clusters tcp_proxy. Each backend resolves to a
// "tcp:<svc>.<meshDomain>" cluster (same path as the Phase 3a floor).
//
// TLSRoute (parentRef=Service): SNI-based routing. The tls_inspector on the
// capture listener already reads the SNI. A TLSRoute adds per-SNI filter chains
// (filter_chain_match.server_names) that route via weighted tcp_proxy to backend
// TCP clusters. No TLS termination — the mTLS between proxies stays intact; the
// SNI is the app's TLS SNI on the raw captured stream.
//
// UDPRoute (parentRef=Service): per-pod UDP capture listener + udp_proxy. The
// CNI installs an nftables REDIRECT rule for outbound UDP to a mesh ClusterIP
// (in cni/internal/plugin/capture.go, programCaptureRedirect). Backends use
// "udp:<svc>.<domain>" EDS clusters (plain, no mTLS — see NewUDPServiceCluster).
//
// SECURITY NOTE: mesh mTLS does NOT cover the UDP floor. Datagrams are forwarded
// to backends in plaintext. This is a known limitation of the UDP floor (proposal
// 018 Phase 3b); DTLS is not implemented.

import (
	"fmt"
	"net"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// L4ServiceRoute holds the L4 routing rules for one mesh service derived from a
// TCPRoute or TLSRoute parentRef=Service. It is the L4 parallel to GammaRoute.
//
// For TCPRoute: one or more rules with weighted backends; no SNI match.
// For TLSRoute: one or more rules, each associated with a set of SNI hostnames;
// a connection's SNI must match one of the rule's hostnames to be routed.
type L4ServiceRoute struct {
	// SNIHostnames are the TLS SNI names matched by this rule (TLSRoute only).
	// Empty means this is a TCPRoute rule (matches all connections to the VIP).
	SNIHostnames []string
	// Backends are the weighted backend clusters this rule forwards to. All
	// backends must already be resolved to "tcp:<svc>.<meshDomain>" cluster names.
	Backends []L4Backend
}

// L4Backend is one weighted backend of an L4 route.
type L4Backend struct {
	// Service is the bare service name (for dependency-set tracking).
	Service string
	// Cluster is the resolved data-plane TCP cluster name ("tcp:<svc>.<meshDomain>").
	Cluster string
	// Weight is the load-balancing weight (1 when unset).
	Weight uint32
}

// UDPServiceRoute holds the UDP routing rules for one mesh service derived from a
// UDPRoute parentRef=Service.
type UDPServiceRoute struct {
	// Backends are the weighted backend clusters.
	Backends []L4Backend
}

// BuildCaptureTCPRouteFilterChain builds a per-ClusterIP TCP floor filter chain
// for a service that has a TCPRoute. It matches the service's ClusterIP as the
// original destination (/32 prefix_ranges) and routes via tcp_proxy
// weighted_clusters to the rule's backends.
//
// This replaces the passthrough floor chain (buildCaptureTCPFloorFilterChain) for
// services that have a TCPRoute: the single-cluster tcp_proxy becomes a
// weighted_clusters tcp_proxy routing to the rule's backends.
//
// Returns nil for invalid inputs so callers can skip them cleanly.
func BuildCaptureTCPRouteFilterChain(svc CaptureTCPService, rules []L4ServiceRoute) *listenerv3.FilterChain {
	if svc.ClusterIP == "" || svc.ClusterName == "" {
		return nil
	}
	if net.ParseIP(svc.ClusterIP) == nil {
		return nil
	}
	if len(rules) == 0 {
		// No rules: fall back to the passthrough floor chain.
		return buildCaptureTCPFloorFilterChain(svc)
	}

	// Collect all backends across all rules (TCPRoute rules are connection-level,
	// not request-level, so all rules contribute to a single weighted cluster set).
	tcpProxy := buildWeightedTCPProxy(
		fmt.Sprintf("cap_tcp_%s", svc.ClusterName),
		l4RulesToWeightedClusters(rules),
	)
	if tcpProxy == nil {
		return buildCaptureTCPFloorFilterChain(svc)
	}

	return &listenerv3.FilterChain{
		Name: fmt.Sprintf("cap_tcp_%s", svc.ClusterName),
		FilterChainMatch: &listenerv3.FilterChainMatch{
			PrefixRanges: []*corev3.CidrRange{
				{AddressPrefix: svc.ClusterIP, PrefixLen: wrapperspb.UInt32(32)},
			},
		},
		Filters: []*listenerv3.Filter{
			buildNetworkNamespaceFilterState(),
			tcpProxy,
		},
	}
}

// BuildCaptureTLSRouteFilterChains builds per-SNI filter chains for a TLSRoute
// attached to a service. Each rule's SNI hostnames become one filter chain with
// server_names match; the chain routes via tcp_proxy to the rule's weighted
// backends.
//
// Placement: these chains are inserted AFTER the per-ClusterIP TCP floor chain
// (so the ClusterIP match takes precedence for non-TLSRoute traffic) but BEFORE
// the global HCM catch-all. The tls_inspector already installed on the capture
// listener reads the SNI so filter_chain_match.server_names works.
//
// NOTE: the filter chains also match prefix_ranges (the service ClusterIP) to
// avoid matching SNI from other services, since a client can set any SNI.
// Without the IP match, a TLSRoute for service A could accidentally intercept
// TLS connections headed for service B if they happen to carry the same SNI.
func BuildCaptureTLSRouteFilterChains(svc CaptureTCPService, rules []L4ServiceRoute) []*listenerv3.FilterChain {
	if svc.ClusterIP == "" || net.ParseIP(svc.ClusterIP) == nil {
		return nil
	}
	var chains []*listenerv3.FilterChain
	for i, rule := range rules {
		if len(rule.SNIHostnames) == 0 || len(rule.Backends) == 0 {
			continue
		}
		statPrefix := fmt.Sprintf("cap_tls_%s_%d", svc.ClusterName, i)
		tcpProxy := buildWeightedTCPProxy(statPrefix, l4BackendsToWeightedClusters(rule.Backends))
		if tcpProxy == nil {
			continue
		}
		chains = append(chains, &listenerv3.FilterChain{
			Name: fmt.Sprintf("cap_tls_%s_%d", svc.ClusterName, i),
			FilterChainMatch: &listenerv3.FilterChainMatch{
				// Scope the SNI match to this specific service's ClusterIP so a
				// TLSRoute for one service doesn't intercept another service's
				// TLS connections that happen to use the same hostname.
				PrefixRanges: []*corev3.CidrRange{
					{AddressPrefix: svc.ClusterIP, PrefixLen: wrapperspb.UInt32(32)},
				},
				ServerNames: rule.SNIHostnames,
			},
			Filters: []*listenerv3.Filter{
				buildNetworkNamespaceFilterState(),
				tcpProxy,
			},
		})
	}
	return chains
}

// CaptureUDPListenerName returns the per-pod UDP capture listener name.
// The listener is control-plane-only until the CNI UDP redirect lands.
func CaptureUDPListenerName(podName string) string {
	return fmt.Sprintf("capture_udp_%s", podName)
}

// GenerateUDPCaptureListener builds a per-pod UDP capture listener for UDPRoute
// routing (proposal 018, Phase 3b). It binds to captureUDPPort inside the pod
// netns and routes via udp_proxy to the service's backends.
//
// The CNI installs a matching nftables REDIRECT rule (programCaptureRedirect in
// cni/internal/plugin/capture.go) that steers outbound UDP destined for a mesh
// ClusterIP:meshPort into this listener when --l4-routes is enabled.
//
// SECURITY NOTE: datagrams forwarded via this listener are NOT protected by
// mesh mTLS. mTLS is a TCP/TLS construct; DTLS is not implemented. Backend
// clusters are plain EDS with no transport socket. This is a known limitation
// of the UDP floor (proposal 018 Phase 3b).
//
// Returns nil if udpRoutes is empty (no listener generated until there are routes).
func GenerateUDPCaptureListener(podName, netns string, captureUDPPort uint32, udpRoutes map[string][]L4Backend) (*listenerv3.Listener, error) {
	if podName == "" {
		return nil, fmt.Errorf("pod name is required")
	}
	if netns == "" {
		return nil, fmt.Errorf("network namespace is required")
	}
	if len(udpRoutes) == 0 {
		return nil, nil
	}

	// UDP proxy does not support weighted_clusters directly in all Envoy versions;
	// use a single cluster (first backend, or the VIP cluster) as the primary.
	// For proper weighted UDP support, a future revision can use the udp_proxy's
	// cluster_specifier with weighted_clusters once it reaches stable.
	// For now, collect all backends into a single primary cluster (the first
	// non-empty service cluster). This is the minimal viable control-plane shape.
	var primaryCluster string
	for _, backends := range udpRoutes {
		if len(backends) > 0 {
			primaryCluster = backends[0].Cluster
			break
		}
	}
	if primaryCluster == "" {
		return nil, nil
	}

	udpFilter := networkFilter("envoy.filters.udp_listener.udp_proxy", &udp_proxyv3.UdpProxyConfig{
		StatPrefix: fmt.Sprintf("capture_udp_%s", podName),
		RouteSpecifier: &udp_proxyv3.UdpProxyConfig_Cluster{
			Cluster: primaryCluster,
		},
	})

	return &listenerv3.Listener{
		Name: CaptureUDPListenerName(podName),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_UDP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: captureUDPPort,
					},
					NetworkNamespaceFilepath: netns,
				},
			},
		},
		StatPrefix:       fmt.Sprintf("capture_udp_%s", podName),
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		ListenerFilters:  []*listenerv3.ListenerFilter{},
		FilterChains: []*listenerv3.FilterChain{
			{
				Name:    fmt.Sprintf("capture_udp_%s", podName),
				Filters: []*listenerv3.Filter{udpFilter},
			},
		},
	}, nil
}

// buildWeightedTCPProxy builds a tcp_proxy network filter routing to one or more
// weighted clusters. If clusters has exactly one entry, uses the simpler
// TcpProxy_Cluster form; otherwise emits TcpProxy_WeightedClusters.
// Returns nil if clusters is empty.
func buildWeightedTCPProxy(statPrefix string, clusters []*tcp_proxyv3.TcpProxy_WeightedCluster_ClusterWeight) *listenerv3.Filter {
	if len(clusters) == 0 {
		return nil
	}
	if len(clusters) == 1 {
		return networkFilter("envoy.filters.network.tcp_proxy", &tcp_proxyv3.TcpProxy{
			StatPrefix:       statPrefix,
			ClusterSpecifier: &tcp_proxyv3.TcpProxy_Cluster{Cluster: clusters[0].Name},
		})
	}
	return networkFilter("envoy.filters.network.tcp_proxy", &tcp_proxyv3.TcpProxy{
		StatPrefix: statPrefix,
		ClusterSpecifier: &tcp_proxyv3.TcpProxy_WeightedClusters{
			WeightedClusters: &tcp_proxyv3.TcpProxy_WeightedCluster{
				Clusters: clusters,
			},
		},
	})
}

// l4RulesToWeightedClusters flattens all backends from all L4ServiceRoute rules
// into a deduplicated weighted cluster list. Duplicate cluster names are merged
// by summing their weights (so two rules forwarding to the same cluster combine
// their traffic share).
func l4RulesToWeightedClusters(rules []L4ServiceRoute) []*tcp_proxyv3.TcpProxy_WeightedCluster_ClusterWeight {
	weightByCluster := make(map[string]uint32)
	var order []string
	for _, rule := range rules {
		for _, b := range rule.Backends {
			if b.Cluster == "" {
				continue
			}
			if _, seen := weightByCluster[b.Cluster]; !seen {
				order = append(order, b.Cluster)
			}
			w := b.Weight
			if w == 0 {
				w = 1
			}
			weightByCluster[b.Cluster] += w
		}
	}
	return weightsToClusterWeights(order, weightByCluster)
}

// l4BackendsToWeightedClusters converts a flat backend list to weighted cluster
// weights for a single-rule chain (TLSRoute per-SNI rules).
func l4BackendsToWeightedClusters(backends []L4Backend) []*tcp_proxyv3.TcpProxy_WeightedCluster_ClusterWeight {
	weightByCluster := make(map[string]uint32, len(backends))
	var order []string
	for _, b := range backends {
		if b.Cluster == "" {
			continue
		}
		if _, seen := weightByCluster[b.Cluster]; !seen {
			order = append(order, b.Cluster)
		}
		w := b.Weight
		if w == 0 {
			w = 1
		}
		weightByCluster[b.Cluster] += w
	}
	return weightsToClusterWeights(order, weightByCluster)
}

func weightsToClusterWeights(order []string, weightByCluster map[string]uint32) []*tcp_proxyv3.TcpProxy_WeightedCluster_ClusterWeight {
	out := make([]*tcp_proxyv3.TcpProxy_WeightedCluster_ClusterWeight, 0, len(order))
	for _, name := range order {
		out = append(out, &tcp_proxyv3.TcpProxy_WeightedCluster_ClusterWeight{
			Name:   name,
			Weight: weightByCluster[name],
		})
	}
	return out
}
