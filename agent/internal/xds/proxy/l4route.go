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
// UDPRoute (parentRef=Service): per-pod transparent UDP capture listener on the
// L4 mesh port + a udp_proxy matcher keyed on the dialled ClusterIP (proposal
// 038). The CNI diverts outbound UDP to ProxyL4OutboundPort into the pod's own
// netns with the header intact (cni/internal/plugin/capture.go,
// programCaptureDivert). Backends use "udp:<svc>.<domain>" EDS clusters (plain,
// no mTLS — see NewUDPServiceCluster).
//
// SECURITY NOTE: mesh mTLS does NOT cover the UDP floor. Datagrams are forwarded
// to backends in plaintext. This is a known limitation of the UDP floor (proposal
// 018 Phase 3b); DTLS is not implemented.

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/common/l4project"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	matcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
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
func CaptureUDPListenerName(podName string) string {
	return fmt.Sprintf("capture_udp_%s", podName)
}

// UDPCaptureArm is one matcher arm of a pod's UDP capture listener: datagrams
// the pod addressed to ClusterIP (the UDPRoute parent Service's VIP) go to
// Cluster (the heaviest live backend of that route).
type UDPCaptureArm struct {
	// Service is the "<ns>/<svc>" UDPRoute parent.
	Service string
	// ClusterIP is the parent's mesh-Service VIP, the matcher key.
	ClusterIP string
	// Cluster is the bound backend's udp: cluster.
	Cluster string
}

// udpCaptureSelection is what the listener carries plus what had to be
// discarded to get there.
type udpCaptureSelection struct {
	// Arms is sorted by Service (#135: map input, deterministic output).
	Arms []UDPCaptureArm
	// Reasons are the human-readable discards (see UnsupportedUDPRouteShapes).
	Reasons []string
}

// GenerateUDPCaptureListener builds a per-pod UDP capture listener for UDPRoute
// routing (proposal 018 Phase 3b, transparent capture per proposal 038).
//
// It binds captureUDPPort inside the pod netns with IP_TRANSPARENT and routes
// via a udp_proxy MATCHER keyed on the datagram's ORIGINAL destination IP: one
// arm per UDPRoute parent whose ClusterIP is known, to that route's selected
// backend cluster. Under TPROXY-style capture the IP header reaches the socket
// intact (cni/internal/plugin/capture.go, programCaptureDivert), so
// DestinationIPInput reads the VIP the pod dialled -- the thing the nat
// REDIRECT this replaces used to destroy, which is why the listener used to
// carry ONE cluster for the whole node (#873).
//
// PORT. The listener binds the port the client dialled (ProxyL4OutboundPort,
// 18082 -- the L4 mesh spelling, shared with raw TCP; 038 D1), not a private
// capture port: Envoy sends a datagram reply from the listener socket's bound
// port, and a connected client drops a reply whose source port differs from
// the one it sent to. The CNI diverts udp dport 18082 with NO port rewrite. The
// TCP capture listener can keep a private port because a TCP reply's source is
// the accepted socket's local endpoint, which tproxy leaves as the original.
//
// clusterIPs maps "<ns>/<svc>" to the Service's ClusterIP; the cache joins it
// from the same source the TCP floor chains use (SetCaptureTCPServices). A
// route whose parent has no known VIP yet produces no arm and a reason, and the
// cache's per-push reconcile picks it up when the VIP arrives.
//
// WHAT udp_proxy CAN AND CANNOT EXPRESS (#873). udp_proxy's only route action is
// envoy.extensions.filters.udp.udp_proxy.v3.Route -- a message with exactly one
// field, `cluster`. tcp_proxy's weighted_clusters has no udp_proxy counterpart,
// so a UDPRoute traffic SPLIT cannot be represented by any route specifier;
// each arm binds ONE cluster. What it honours: weight 0 is an explicit DRAIN
// (Gateway API, #492) and is never bound; among the rest the HEAVIEST backend
// wins, ties broken by name, not backendRef order.
//
// Returns nil (no listener) when no arm can be built.
//
// SECURITY NOTE: datagrams forwarded via this listener are NOT protected by
// mesh mTLS. mTLS is a TCP/TLS construct; DTLS is not implemented. Backend
// clusters are plain, with no transport socket (proposal 018 Phase 3b).
func GenerateUDPCaptureListener(podName, netns string, captureUDPPort uint32, udpRoutes map[string][]L4Backend, clusterIPs map[string]string) (*listenerv3.Listener, error) {
	if podName == "" {
		return nil, fmt.Errorf("pod name is required")
	}
	if netns == "" {
		return nil, fmt.Errorf("network namespace is required")
	}
	if len(udpRoutes) == 0 {
		return nil, nil
	}

	sel := selectUDPCaptureArms(udpRoutes, clusterIPs)
	if len(sel.Arms) == 0 {
		// Nothing routable: no backends, every backend drained, or no VIP
		// known. A matcher with no arms is worse than no listener -- udp_proxy
		// would accept every datagram and drop it with no stat.
		return nil, nil
	}

	arms := make(map[string]*matcherv3.Matcher_OnMatch, len(sel.Arms))
	for _, arm := range sel.Arms {
		arms[arm.ClusterIP] = &matcherv3.Matcher_OnMatch{
			OnMatch: &matcherv3.Matcher_OnMatch_Action{
				Action: &xdscorev3.TypedExtensionConfig{
					Name:        udpRouteActionName,
					TypedConfig: config.TypedConfig(&udp_proxyv3.Route{Cluster: arm.Cluster}),
				},
			},
		}
	}

	// udp_proxy is a UDP *listener filter*, not a network filter in a filter chain.
	// A connection-less UDP listener must carry NO filter_chains -- Envoy rejects it
	// with "N filter chain(s) specified for connection-less UDP listener" -- so the
	// proxy config goes in listener_filters instead.
	udpProxyConfig := config.TypedConfig(&udp_proxyv3.UdpProxyConfig{
		StatPrefix: fmt.Sprintf("capture_udp_%s", podName),
		RouteSpecifier: &udp_proxyv3.UdpProxyConfig_Matcher{
			Matcher: &matcherv3.Matcher{
				MatcherType: &matcherv3.Matcher_MatcherTree_{
					MatcherTree: &matcherv3.Matcher_MatcherTree{
						Input: &xdscorev3.TypedExtensionConfig{
							Name:        udpDestinationIPInputName,
							TypedConfig: config.TypedConfig(&network_inputsv3.DestinationIPInput{}),
						},
						TreeType: &matcherv3.Matcher_MatcherTree_ExactMatchMap{
							ExactMatchMap: &matcherv3.Matcher_MatcherTree_MatchMap{Map: arms},
						},
					},
				},
				// No on_no_match: a datagram to a VIP with no UDPRoute is dropped
				// here (udp_proxy counts it as no_route), which is what the old
				// scoped REDIRECT did for it too -- it was never captured.
			},
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
		// IP_TRANSPARENT (proposal 038): lets the divert deliver a datagram whose
		// destination is a non-local VIP to this socket, and lets the reply
		// leave with that VIP as its source. Applied per worker socket at
		// PREBIND inside the pod netns; needs CAP_NET_ADMIN, which the proxy
		// pod grants (charts/aether, aether_proxy_net_admin_test).
		Transparent:      wrapperspb.Bool(true),
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

const (
	// udpDestinationIPInputName is the matcher input that reads the datagram's
	// destination IP (envoy.matching.common_inputs.network.destination_ip).
	udpDestinationIPInputName = "destination-ip"
	// udpRouteActionName is the udp_proxy route action's name; only its typed
	// config is looked up, the name is cosmetic.
	udpRouteActionName = "route"
)

// UnsupportedUDPRouteShapes reports the UDPRoute inputs that
// GenerateUDPCaptureListener discards, as human-readable reasons. Empty means
// everything in udpRoutes is faithfully represented.
//
// It exists because the discard is otherwise invisible from both ends (#873):
// the UDPRoute is accepted by the API, common/l4project resolves and weights its
// backends correctly, and then the UDP path keeps ONE cluster per service. There
// is no NACK and no stat, so the first symptom is datagrams arriving somewhere
// unintended.
//
// It reads the SAME selection the generator uses -- one call to
// selectUDPCaptureArms, not a second copy of the rules -- so the detector and
// the generator cannot drift (#874).
//
// This is a pure function by design: the proxy package builds config and does
// not log. The caller decides what to do with the reasons.
func UnsupportedUDPRouteShapes(udpRoutes map[string][]L4Backend, clusterIPs map[string]string) []string {
	return selectUDPCaptureArms(udpRoutes, clusterIPs).Reasons
}

// UDPCaptureArms returns the arms GenerateUDPCaptureListener would build for
// these routes, sorted by service; empty when it would build no listener.
//
// It exists so a caller can tell whether a rebuild would change anything
// WITHOUT building the listener (and without re-emitting the warnings that come
// with it). The arm set is node-global, so one answer serves every pod.
func UDPCaptureArms(udpRoutes map[string][]L4Backend, clusterIPs map[string]string) []UDPCaptureArm {
	return selectUDPCaptureArms(udpRoutes, clusterIPs).Arms
}

// UDPCaptureArmsKey canonicalises an arm set for change detection: the same
// arms in any order give the same key, and any change to a VIP, a cluster or
// the membership gives a different one. (#873's one-string compare could only
// see the first service move; a second service or a late VIP was invisible.)
func UDPCaptureArmsKey(arms []UDPCaptureArm) string {
	parts := make([]string, 0, len(arms))
	for _, a := range arms {
		parts = append(parts, a.Service+"|"+a.ClusterIP+"|"+a.Cluster)
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}

// selectUDPCaptureArms picks, per UDPRoute parent, the one backend cluster its
// matcher arm binds, and records everything it could not represent.
//
// Ordering is over a SORTED key list: udpRoutes is a map, and Go randomises map
// iteration, so an unsorted walk would emit the reasons in a fresh random order
// every rebuild (#135 -- determinism is protocol-visible; see cache/ordering.go).
// The matcher's arms are a proto map and hash the same in any order.
func selectUDPCaptureArms(udpRoutes map[string][]L4Backend, clusterIPs map[string]string) udpCaptureSelection {
	var sel udpCaptureSelection
	seenVIP := map[string]string{}
	for _, svc := range slices.Sorted(maps.Keys(udpRoutes)) {
		live := liveUDPBackends(udpRoutes[svc])
		if len(live) == 0 {
			if reason := drainedServiceReason(svc, udpRoutes[svc]); reason != "" {
				sel.Reasons = append(sel.Reasons, reason)
			}
			continue
		}
		vip := clusterIPs[svc]
		if vip == "" {
			sel.Reasons = append(sel.Reasons, fmt.Sprintf(
				"service %q has no known ClusterIP yet: the UDP capture matcher keys on the dialled VIP, so no arm is built until the mesh Service is observed",
				svc))
			continue
		}
		if prev, dup := seenVIP[vip]; dup {
			// Two parents on one VIP cannot happen for distinct Services; guard
			// it anyway so a proto map never silently keeps the last writer.
			sel.Reasons = append(sel.Reasons, fmt.Sprintf(
				"service %q shares ClusterIP %s with %q and is dropped: a matcher arm per VIP can bind one cluster", svc, vip, prev))
			continue
		}
		chosen := heaviestUDPBackend(live)
		seenVIP[vip] = svc
		sel.Arms = append(sel.Arms, UDPCaptureArm{Service: svc, ClusterIP: vip, Cluster: chosen.Cluster})
		if len(live) > 1 {
			sel.Reasons = append(sel.Reasons, fmt.Sprintf(
				"service %q has %d routable backends but udp_proxy carries a single cluster per route: only %q (the heaviest, weight %d) is used and the backend weights are discarded",
				svc, len(live), chosen.Cluster, chosen.Weight))
		}
	}
	return sel
}

// liveUDPBackends keeps the backends that can actually be bound: a named
// cluster, and a non-zero weight. An explicit weight 0 is a Gateway API DRAIN
// (#492) -- the reconciler already defaulted an UNSET weight to 1, so a 0 here
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
// A service with no backends at all is silent -- there is nothing to report and
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
