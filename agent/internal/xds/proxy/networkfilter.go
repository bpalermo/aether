package proxy

import (
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// networkNamespaceFilterStateKey is the filter state key for the network namespace
	networkNamespaceFilterStateKey = "aether.network.network_namespace"

	// sourceIdentityFilterStateKey carries the SOURCE POD'S SPIFFE ID, written as
	// a literal string by every listener chain that can originate mesh traffic,
	// and read by the cluster transport_socket_matcher
	// (UpstreamTransportSocketMatcher, transportsocketmatch.go) since release two.
	//
	// THREE-RELEASE CONTRACT (issue #815) — read this before touching either key.
	//
	// The matcher's exact_match_map used to be keyed on
	// networkNamespaceFilterStateKey. A netns path is unique PER POD, so every
	// local pod ADD/DEL rewrote a field of EVERY mesh cluster on the node; under
	// delta-xDS each rewritten EDS cluster re-warmed for the full 15 s EDS
	// initial_fetch_timeout and the warming→active swap then DrainAndDeletes
	// every upstream connection pool on the node. Measured on the live cluster:
	// 20–33 clusters warming for exactly 15 s per pod ADD, 30 s for two.
	//
	// The thing the matcher SELECTS was always per-ServiceAccount (the match name
	// is the SPIFFE ID, UpstreamTransportSocketMatches). Keying the map on the
	// SPIFFE ID instead makes the Cluster proto byte-stable across pod churn: a
	// pod of an already-present ServiceAccount changes no cluster at all and
	// Envoy's hash gate blocks the update.
	//
	//	RELEASE ONE (#819/#821, SHIPPED — talos-main rev224, 2026-09-19): every
	//	chain that sets the netns key ALSO sets this one. Clusters untouched and
	//	byte-identical.
	//
	//	RELEASE TWO (SHIPPED, this release): the matcher's FilterStateInput.key is
	//	this key. Both keys are still stamped on every originating chain.
	//	UPGRADE CONSTRAINT: a proxy must run release-one (or later) LISTENERS
	//	before it is handed release-two CLUSTERS. Upgrading a node straight from a
	//	pre-#819 build is NOT supported — the agent publishes both in one
	//	snapshot, but LDS and CDS are separate responses with no ordering
	//	guarantee (the ADS server is go-control-plane's default, not
	//	sotw.WithOrderedADS), so a cluster with the new input can face a listener
	//	that only sets the old key. Such a connection matches nothing, takes
	//	OnNoMatch, and presents the NODE identity instead of the pod's (#686
	//	territory). Go through release one first.
	//
	//	RELEASE THREE, NOT YET: drop networkNamespaceFilterStateKey from the
	//	listeners. Allowed only once no supported upgrade path can run
	//	release-two-or-later clusters against pre-release-one listeners — i.e.
	//	once the oldest supported chart/agent version is >= release one AND every
	//	proxy has been through a filter-chain-changing LDS update or a restart
	//	since. Doing it in release two would strand any proxy still holding a
	//	pre-release-one cluster on the OnNoMatch path in exactly the same way, in
	//	the other direction. The netns key costs one set_filter_state entry per
	//	chain; there is no hurry.
	//
	// Namespaced under "aether." like the netns key so it can never collide with
	// an Envoy-owned filter state object name.
	sourceIdentityFilterStateKey = "aether.source.spiffe_id"
)

// buildNetworkNamespaceFilterState creates a filter that captures the network namespace
// from Envoy's filter state and makes it available to upstream filters.
func buildNetworkNamespaceFilterState() *listenerv3.Filter {
	return buildSetFilterState(networkNamespaceFilterStateKey, "%FILTER_STATE(envoy.network.network_namespace:PLAIN)%")
}

// buildSourceIdentityFilterState stores the source pod's SPIFFE ID in filter
// state as a LITERAL inline string (not a %FILTER_STATE(...)% substitution —
// Envoy has no notion of the pod's mesh identity, the control plane does).
// The value is the identity proxy.SpiffeIDFromPod derives for the pod, i.e.
// exactly the name of the SDS secret whose certificate the upstream connection
// must present.
//
// A literal contains no '%', so SubstitutionFormatString passes it through
// unchanged; the format parser only treats %…% pairs as commands.
//
// Shared with the upstream connection with the SAME semantics as the netns key
// (SharedWithUpstream ONCE) because it is destined for the same consumer: the
// cluster's transport-socket matcher reads it through
// TransportSocketOptions::downstreamSharedFilterStateObjects(), which is
// populated only from filter state objects marked shared. ONCE scopes the
// propagation to the immediate upstream hop, so a future chained/internal hop
// cannot silently inherit source-identity cert selection.
func buildSourceIdentityFilterState(sourceSpiffeID string) *listenerv3.Filter {
	return buildSetFilterState(sourceIdentityFilterStateKey, sourceSpiffeID)
}

// buildSourceFilterStates returns the source-attribution network filters every
// mesh-originating filter chain carries, in a FIXED order (netns first, then
// identity): these land in the chain's repeated `filters` field, whose order is
// part of the listener's bytes and therefore of its delta-xDS hash.
//
// sourceSpiffeID may be empty — before the trust domain is known there is no
// identity to stamp — in which case only the netns filter is emitted, which is
// byte-for-byte what this chain carried before issue #815.
func buildSourceFilterStates(sourceSpiffeID string) []*listenerv3.Filter {
	filters := []*listenerv3.Filter{buildNetworkNamespaceFilterState()}
	if sourceSpiffeID != "" {
		filters = append(filters, buildSourceIdentityFilterState(sourceSpiffeID))
	}
	return filters
}

// buildSetFilterState creates a set_filter_state network filter that stores a value in filter state.
func buildSetFilterState(objectKey string, inlineStringFormatString string) *listenerv3.Filter {
	filter := &set_filter_state_v3.Config{
		OnNewConnection: []*setFilterStatev3.FilterStateValue{
			{
				Key: &setFilterStatev3.FilterStateValue_ObjectKey{
					ObjectKey: objectKey,
				},
				FactoryKey: "envoy.string",
				Value: &setFilterStatev3.FilterStateValue_FormatString{
					FormatString: &corev3.SubstitutionFormatString{
						Format: &corev3.SubstitutionFormatString_TextFormatSource{
							TextFormatSource: &corev3.DataSource{
								Specifier: &corev3.DataSource_InlineString{
									InlineString: inlineStringFormatString,
								},
							},
						},
					},
				},
				// Shared with the upstream connection so the service cluster's
				// transport-socket matcher can read the source pod's network namespace
				// and select that pod's client certificate for the outbound mTLS.
				// ONCE (immediate upstream connection only) is sufficient for the
				// single-hop transport: TRANSITIVE — which re-propagates through any
				// further upstream hops — was HBONE/tunnel-era (#86) plumbing for the
				// two-layer upstream path and was left behind when #98 flattened the
				// transport; scoping it to one hop keeps a future chained/internal
				// hop from silently inheriting source-netns cert selection.
				SharedWithUpstream: setFilterStatev3.FilterStateValue_ONCE,
			},
		},
	}
	return networkFilter("envoy.filters.network.set_filter_state", filter)
}

// downstreamIdleTimeout bounds idle downstream connections on the per-pod HCMs
// (inbound mesh mTLS conns from peer proxies, and app→proxy conns on the
// outbound listener). It is the server-side backstop to the upstream
// config.UpstreamIdleTimeout: peers reclaim their idle conns at 30s, so this
// only catches clients that don't (Envoy's default is 1 hour, which let a
// peer's leaked upstream conns pin ~7k inbound conns per listener). Kept well
// above the upstream timeout so the client side always disconnects first.
const downstreamIdleTimeout = 5 * time.Minute

// buildHTTPConnectionManager creates an HTTP connection manager for processing HTTP traffic.
// It includes a router HTTP filter and uses the provided route configuration.
// If routeConfig is nil, routes will be retrieved via RDS. reporter tags the OTel
// access logger (ReporterSource for egress, ReporterDestination for inbound); the
// logger is attached only when access logging is enabled (buildAccessLog → nil).
func buildHTTPConnectionManager(name, reporter, podName, podNamespace string, routeConfig *routev3.RouteConfiguration) *http_connection_managerv3.HttpConnectionManager {
	return &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  http_connection_managerv3.HttpConnectionManager_AUTO,
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(downstreamIdleTimeout),
		},
		AccessLog: buildAccessLog(reporter, podName, podNamespace),
		Tracing:   buildTracing(),
		HttpFilters: []*http_connection_managerv3.HttpFilter{
			routerHttpFilter(),
		},
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: routeConfig,
		},
	}
}

// buildHTTPConnectionManagerFilter creates a network filter wrapping an HTTP connection manager.
func buildHTTPConnectionManagerFilter(config *http_connection_managerv3.HttpConnectionManager) *listenerv3.Filter {
	return networkFilter("envoy.http_connection_manager", config)
}

// buildTCPProxyNetworkFilter builds an envoy.filters.network.tcp_proxy network
// filter that routes all TCP traffic to the named upstream cluster. statPrefix is
// used for Envoy stats (tcp.<statPrefix>.*). The upstream transport socket is NOT
// set here — it is injected at snapshot time via InjectUpstreamMTLS (same path as
// the HCM egress clusters), so per-source mTLS and SPIFFE SAN pinning apply.
func buildTCPProxyNetworkFilter(statPrefix, clusterName string) *listenerv3.Filter {
	return networkFilter("envoy.filters.network.tcp_proxy", &tcp_proxyv3.TcpProxy{
		StatPrefix:       statPrefix,
		ClusterSpecifier: &tcp_proxyv3.TcpProxy_Cluster{Cluster: clusterName},
	})
}

// networkFilter creates a network filter with the given name and configuration.
func networkFilter(name string, msg proto.Message) *listenerv3.Filter {
	return &listenerv3.Filter{
		Name: name,
		ConfigType: &listenerv3.Filter_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
