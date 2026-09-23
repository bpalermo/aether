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
	"maps"
	"net"
	"slices"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/common/l4project"
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

// L4Backend is one weighted backend of an L4 route: the namespace-qualified
// "<ns>/<svc>" key, the resolved data-plane cluster name, and the weight.
//
// It is an alias for the shared projection type so the node agent's L4 reconciler
// and the edge gateway reconciler can both feed this layer from one projector
// (common/l4project.Backends). An explicit weight 0 means DRAIN and is omitted from
// the weighted-cluster set here rather than normalised to 1 (#492).
type L4Backend = l4project.Backend

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
func BuildCaptureTCPRouteFilterChain(svc CaptureTCPService, rules []L4ServiceRoute, sourceSpiffeID string) *listenerv3.FilterChain {
	if svc.ClusterIP == "" || svc.ClusterName == "" {
		return nil
	}
	if net.ParseIP(svc.ClusterIP) == nil {
		return nil
	}
	if len(rules) == 0 {
		// No rules: fall back to the passthrough floor chain.
		return buildCaptureTCPFloorFilterChain(svc, sourceSpiffeID)
	}

	// Collect all backends across all rules (TCPRoute rules are connection-level,
	// not request-level, so all rules contribute to a single weighted cluster set).
	tcpProxy := buildWeightedTCPProxy(
		fmt.Sprintf("cap_tcp_%s", svc.ClusterName),
		l4RulesToWeightedClusters(rules),
	)
	if tcpProxy == nil {
		return buildCaptureTCPFloorFilterChain(svc, sourceSpiffeID)
	}

	return &listenerv3.FilterChain{
		Name: fmt.Sprintf("cap_tcp_%s", svc.ClusterName),
		FilterChainMatch: &listenerv3.FilterChainMatch{
			PrefixRanges: []*corev3.CidrRange{
				{AddressPrefix: svc.ClusterIP, PrefixLen: wrapperspb.UInt32(32)},
			},
		},
		Filters: append(BuildSourceFilterStates(sourceSpiffeID), tcpProxy),
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
func BuildCaptureTLSRouteFilterChains(svc CaptureTCPService, rules []L4ServiceRoute, sourceSpiffeID string) []*listenerv3.FilterChain {
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
			Filters: append(BuildSourceFilterStates(sourceSpiffeID), tcpProxy),
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
// netns and routes via udp_proxy to the selected backend cluster.
//
// The CNI installs a matching nftables REDIRECT rule (programCaptureRedirect in
// cni/internal/plugin/capture.go) that steers outbound UDP destined for a mesh
// ClusterIP:meshPort into this listener. The redirect is unconditional (the
// --l4-routes flag was retired by proposal 031); the listener below exists only
// when UDPRoute backends do.
//
// WHAT udp_proxy CAN AND CANNOT EXPRESS (#873). udp_proxy's route_specifier is
// a oneof of `cluster` (a single name) and `matcher`, whose only terminal action
// is envoy.extensions.filters.udp.udp_proxy.v3.Route -- a message with exactly
// one field, `cluster`. The action registry is keyed on udp_proxy's private
// RouteActionContext and holds exactly that one action, so tcp_proxy's
// weighted_clusters has no udp_proxy counterpart and a UDPRoute traffic split
// cannot be represented by any route specifier. (Two things CAN produce a split
// and are deliberately not done here: weighting the ENDPOINTS of a single merged
// cluster, which would collapse the per-backend `udp:` clusters the dependency
// set, the EDS rewrite and every cluster stat are keyed on; and a sticky
// xxHash bucket via an envoy.matching.matchers.runtime_fraction predicate, which
// needs the `matcher` specifier — still work_in_progress in 1.39 — and splits
// per source socket rather than per datagram. Both are follow-ups to #873, not
// drive-bys.)
//
// So the listener binds ONE cluster, and selectUDPRoute decides which. What it
// CAN honour, and now does:
//   - weight 0 is an explicit DRAIN (Gateway API, #492): a drained backend is
//     never bound, and a service whose every backend is drained contributes
//     nothing rather than receiving all of the traffic;
//   - among the rest, the HEAVIEST backend wins, not the first one written.
//     Binding backends[0] made a 10/90 split written canary-first send 100% to
//     the canary, purely because of backendRef order.
//
// Returns nil (no listener) when nothing is left to route to.
//
// SECURITY NOTE: datagrams forwarded via this listener are NOT protected by
// mesh mTLS. mTLS is a TCP/TLS construct; DTLS is not implemented. Backend
// clusters are plain, with no transport socket. This is a known limitation of
// the UDP floor (proposal 018 Phase 3b).
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

	selection := selectUDPRoute(udpRoutes)
	if selection.Cluster == "" {
		// Nothing routable: no backends, or every backend drained. A listener
		// naming no cluster is worse than no listener — udp_proxy requires a
		// non-empty cluster, so Envoy would NACK the push.
		return nil, nil
	}

	// udp_proxy is a UDP *listener filter*, not a network filter in a filter chain.
	// A connection-less UDP listener must carry NO filter_chains — Envoy rejects it
	// with "N filter chain(s) specified for connection-less UDP listener" — so the
	// proxy config goes in listener_filters instead.
	udpProxyConfig := config.TypedConfig(&udp_proxyv3.UdpProxyConfig{
		StatPrefix: fmt.Sprintf("capture_udp_%s", podName),
		RouteSpecifier: &udp_proxyv3.UdpProxyConfig_Cluster{
			Cluster: selection.Cluster,
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
		ListenerFilters: []*listenerv3.ListenerFilter{
			{
				Name:       "envoy.filters.udp_listener.udp_proxy",
				ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: udpProxyConfig},
			},
		},
	}, nil
}

// UnsupportedUDPRouteShapes reports the UDPRoute inputs that
// GenerateUDPCaptureListener discards, as human-readable reasons. Empty means
// everything in udpRoutes is faithfully represented.
//
// It exists because the discard is otherwise invisible from both ends (#873):
// the UDPRoute is accepted by the API, common/l4project resolves and weights its
// backends correctly, and then the UDP path keeps ONE cluster. There is no NACK
// and no stat, so the first symptom is datagrams arriving somewhere unintended.
//
// It reads the SAME selection the generator uses — one call to selectUDPRoute,
// not a second copy of the rules. #874 shipped it as a deliberate duplicate
// pinned by a test, because a drifted detector confidently names the wrong
// cluster and is worse than no warning at all; sharing the decision makes that
// drift unrepresentable rather than merely tested for.
//
// This is a pure function by design: the proxy package builds config and does
// not log. The caller decides what to do with the reasons.
func UnsupportedUDPRouteShapes(udpRoutes map[string][]L4Backend) []string {
	return selectUDPRoute(udpRoutes).Reasons
}

// SelectedUDPCluster returns the cluster GenerateUDPCaptureListener would bind
// for these routes, or "" when it would generate no listener at all.
//
// It exists so a caller can tell whether a rebuild would change anything
// WITHOUT building the listener (and without re-emitting the warnings that come
// with it). The bound cluster is node-global, so one string answers it for every
// pod on the node.
func SelectedUDPCluster(udpRoutes map[string][]L4Backend) string {
	return selectUDPRoute(udpRoutes).Cluster
}

// udpRouteSelection is the single route a per-pod UDP capture listener can
// carry, plus what had to be discarded to get down to one.
type udpRouteSelection struct {
	// Service is the "<ns>/<svc>" UDPRoute parent whose backend was bound, or
	// "" when nothing is routable.
	Service string
	// Cluster is the bound backend cluster, or "" when nothing is routable.
	Cluster string
	// Reasons are the human-readable discards (see UnsupportedUDPRouteShapes).
	Reasons []string
}

// selectUDPRoute picks the one backend cluster the pod's UDP capture listener
// binds, and records everything it could not represent.
//
// Ordering is over a SORTED key list: udpRoutes is a map, and Go randomises map
// iteration, so an unsorted pick would send the listener to a different cluster
// on each rebuild of identical input and re-hash the listener on every push
// (#135 — determinism is protocol-visible here). See cache/ordering.go.
//
// WHY ONLY ONE SERVICE, and why a matcher would not help. There is one UDP
// capture listener per pod, and it cannot tell which service a datagram was
// addressed to. Verified against Envoy v1.39.0:
//
//   - the CNI captures UDP with an nftables `redirect`, which for locally
//     generated packets rewrites the destination to 127.0.0.1 before delivery;
//   - a datagram listener's only addressing cmsg is IP_PKTINFO
//     (listener_impl.cc buildIpPacketInfoOptions), so the matcher's
//     DestinationIPInput reads the POST-DNAT header — the ClusterIP is gone.
//     Envoy never reads IP_ORIGDSTADDR anywhere, and original_dst is
//     structurally TCP-only (OriginalDstFilter implements Network::ListenerFilter,
//     not UdpListenerFilter, and LDS rejects it on a UDP listener);
//   - DestinationPortInput is not packet-derived at all: the port is stamped
//     from the listening socket's own port (self_port), so it is the constant
//     ProxyCapturePort no matter what was dialled.
//
// A udp_proxy matcher therefore has nothing to discriminate on. Per-service
// selection needs either TPROXY capture or a listener per service on its own
// port — a CNI change, not a control-plane one.
func selectUDPRoute(udpRoutes map[string][]L4Backend) udpRouteSelection {
	var sel udpRouteSelection
	for _, svc := range slices.Sorted(maps.Keys(udpRoutes)) {
		live := liveUDPBackends(udpRoutes[svc])
		if len(live) == 0 {
			if reason := drainedServiceReason(svc, udpRoutes[svc]); reason != "" {
				sel.Reasons = append(sel.Reasons, reason)
			}
			continue
		}
		if sel.Cluster != "" {
			sel.Reasons = append(sel.Reasons, fmt.Sprintf(
				"service %q is dropped entirely: the pod has ONE UDP capture listener and it is already bound to %q (service %q)",
				svc, sel.Cluster, sel.Service))
			continue
		}
		chosen := heaviestUDPBackend(live)
		sel.Service, sel.Cluster = svc, chosen.Cluster
		if len(live) > 1 {
			sel.Reasons = append(sel.Reasons, fmt.Sprintf(
				"service %q has %d routable backends but udp_proxy carries a single cluster: only %q (the heaviest, weight %d) is used and the backend weights are discarded",
				svc, len(live), chosen.Cluster, chosen.Weight))
		}
	}
	return sel
}

// liveUDPBackends keeps the backends that can actually be bound: a named
// cluster, and a non-zero weight. An explicit weight 0 is a Gateway API DRAIN
// (#492) — the reconciler already defaulted an UNSET weight to 1, so a 0 here
// is always intentional and must not be normalised back to 1. This is the same
// rule l4RulesToWeightedClusters and l4BackendsToWeightedClusters apply on the
// TCP and TLS paths.
func liveUDPBackends(backends []L4Backend) []L4Backend {
	live := make([]L4Backend, 0, len(backends))
	for _, b := range backends {
		if b.Cluster == "" || b.Weight == 0 {
			continue
		}
		live = append(live, b)
	}
	return live
}

// heaviestUDPBackend returns the backend with the largest weight, ties broken by
// cluster name so the choice does not depend on backendRef order (#135). live
// must be non-empty.
func heaviestUDPBackend(live []L4Backend) L4Backend {
	best := live[0]
	for _, b := range live[1:] {
		if b.Weight > best.Weight || (b.Weight == best.Weight && b.Cluster < best.Cluster) {
			best = b
		}
	}
	return best
}

// drainedServiceReason explains a service that contributes no routable backend.
// A service with no backends at all is silent — there is nothing to report and
// nothing was discarded. A service whose backends were ALL dropped is reported:
// the route was accepted and produces no data path, which is worth saying out
// loud even though the drain itself is being honoured correctly.
func drainedServiceReason(svc string, backends []L4Backend) string {
	if len(backends) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"service %q contributes no UDP route: every one of its %d backends has weight 0 (drain) or no cluster, so the listener binds nothing for it",
		svc, len(backends))
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
			// weight 0 is an EXPLICIT drain (Gateway API): the backend receives no
			// traffic, so it must be omitted from the weighted set — not normalized
			// to 1. The reconciler already defaulted an UNSET weight to 1, so a 0 here
			// is always intentional. (A previous 0→1 made drain == equal-weight.)
			if b.Weight == 0 {
				continue
			}
			if _, seen := weightByCluster[b.Cluster]; !seen {
				order = append(order, b.Cluster)
			}
			weightByCluster[b.Cluster] += b.Weight
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
		// weight 0 = explicit drain (see l4RulesToWeightedClusters): omit, don't
		// normalize to 1.
		if b.Weight == 0 {
			continue
		}
		if _, seen := weightByCluster[b.Cluster]; !seen {
			order = append(order, b.Cluster)
		}
		weightByCluster[b.Cluster] += b.Weight
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
