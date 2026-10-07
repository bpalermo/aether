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

	// The source pod's SPIFFE ID used to be stamped a SECOND time, under
	// "aether.source.spiffe_id" with the plain "envoy.string" factory. That key
	// was the #815 transport_socket_matcher's input (release two, chart
	// 0.92.28) and survived #842 (chart 0.93.0, 2026-09-20) for one release so
	// a rollback to a pre-#842 cluster — whose matcher still read it — stayed
	// hitless. It was RETIRED in #1165: every reader (the access log's
	// `source_spiffe_id`, the QUIC selection matcher) now reads
	// SourceIdentityCertMapperFilterStateKey instead.
	//
	// ROLLBACK FLOOR: a proxy running these listeners, handed a pre-0.93.0
	// cluster (the netns- or aether.source-keyed matcher), matches nothing and
	// takes OnNoMatch — the AGENT'S OWN SVID,
	// spiffe://<td>/ns/aether-system/sa/aether-agent (#825) — so a rollback
	// below 0.93.0 is no longer hitless. //agent/test/envoy_validate asserts the
	// retired name appears in no listener of any fixture.
	//
	// The netns key (networkNamespaceFilterStateKey) is NOT retired with it: it
	// carries no per-pod state on a listener (its value is a constant format
	// string), and accesslog.go's `source_netns` is still its reader. See
	// buildNetworkNamespaceFilterState for what dropping it would take.

	// SourceIdentityCertMapperFilterStateKey carries the source pod's SPIFFE
	// ID, written as a literal string by every listener chain that can
	// originate mesh traffic, under the name Envoy's upstream certificate mapper reads, and with a HASHABLE
	// factory so it reaches the upstream connection-pool key (issue #842).
	//
	// THE NAME IS NOT OURS TO CHOOSE. The
	// envoy.tls.upstream_certificate_mappers.filter_state_override mapper
	// hardcodes the lookup — it walks
	// TransportSocketOptions::downstreamSharedFilterStateObjects() comparing
	// obj.name_ against the literal "envoy.tls.certificate_mappers.on_demand_secret"
	// (source/extensions/transport_sockets/tls/cert_mappers/filter_state_override/config.cc
	// at the pinned 1.40.0-dev.20260904.13144fb snapshot) and falls back to its
	// configured default_value on any miss. There is no configurable key, so a
	// namespaced "aether.*" name is not available here. It is an Envoy-owned
	// name by construction, which is why it is spelled in full rather than
	// built from a prefix: a typo is a SILENT fallback to the node identity,
	// never an error.
	//
	// THE FACTORY IS THE POINT. set_filter_state's default "envoy.string"
	// factory builds Router::StringAccessorImpl, which is NOT Envoy::Hashable;
	// CommonUpstreamTransportSocketFactory::hashKey folds a downstream shared
	// filter-state object into the upstream pool key only behind
	// dynamic_cast<const Hashable*>. "envoy.hashable_string" builds
	// HashableString — StringAccessorImpl PLUS Hashable, hash() =
	// xxHash64(value) — so the source identity finally contributes bytes to the
	// pool key and upstream pools partition per (host, source identity). That
	// is what let connection_pool_per_downstream_connection come off every mesh
	// cluster (#831/#841 established the flag was the only thing preventing a
	// cross-source client-certificate leak; #842 replaced it with the correct
	// partition).
	//
	// THE VALUE MUST STAY BOUNDED. Envoy exposes the hashable factory under an
	// explicitly named, documented-with-a-warning extension rather than a
	// boolean precisely because anything per-request here multiplies upstream
	// connections. A SPIFFE ID is bounded by ServiceAccounts-per-node, which is
	// exactly the partition the client certificate needs. Never put a
	// per-request or per-pod value in this key.
	//
	// IT IS THE ONLY SOURCE-IDENTITY KEY (since #1165). Readers:
	//   - the cluster's filter_state_override certificate mapper (above);
	//   - CommonUpstreamTransportSocketFactory::hashKey (the pool partition);
	//   - accesslog.go's `source_spiffe_id` (HTTP and L4 logs) — HashableString
	//     is a StringAccessor, so %FILTER_STATE(<key>:PLAIN)% renders it; the
	//     attribute NAME did not change when its key moved here;
	//   - the QUIC selection matcher (route.go, ApplyQUICClusterSelection),
	//     through envoy.matching.inputs.filter_state, which reads
	//     serializeAsString() — again the StringAccessor surface.
	//
	// EXPORTED so the out-of-tree runtime harness (//agent/test/mtlspool) stamps the
	// same key production does rather than re-spelling the literal.
	SourceIdentityCertMapperFilterStateKey = "envoy.tls.certificate_mappers.on_demand_secret"

	// genericStringFactory builds Router::StringAccessorImpl — readable by
	// %FILTER_STATE(...)% and by matcher inputs, invisible to the upstream pool
	// hash.
	genericStringFactory = "envoy.string"
	// hashableStringFactory builds HashableString: the same StringAccessor
	// surface PLUS Envoy::Hashable, so the value reaches
	// CommonUpstreamTransportSocketFactory::hashKey. See
	// SourceIdentityCertMapperFilterStateKey.
	hashableStringFactory = "envoy.hashable_string"
)

// buildNetworkNamespaceFilterState copies Envoy's OWN downstream-netns filter
// state object into an aether-namespaced, upstream-shared copy.
//
// The source object is native: Envoy sets `envoy.network.network_namespace`
// (Network::DownstreamNetworkNamespace) in the ActiveTcpSocket constructor, at
// accept time, whenever the listener's address carries a
// network_namespace_filepath — which every per-pod listener here does
// (listener.go, capture.go, ingress.go, l4route.go). It is LifeSpan::Connection
// and PLAIN-formattable (serializeAsString is overridden; serializeAsProto is
// not, so `:PLAIN` is mandatory — the formatter defaults to TYPED, which renders
// "-"). Not populated for QUIC/HTTP3 or internal listeners.
//
// SINCE RELEASE TWO THIS COPY HAS EXACTLY ONE READER: accesslog.go's
// `source_netns` attribute. The cluster transport_socket_matcher moved off it
// in #822 (and was itself removed in #842), and the SharedWithUpstream: ONCE below
// is now vestigial for this key (nothing upstream reads it). It is left as-is
// deliberately — changing it would rewrite every per-pod filter chain's bytes
// for no behavioural gain.
//
// Dropping this copy (#815's "release three") was evaluated on 2026-09-19 and
// declined: it buys ~240 B per chain, costs a deploy that drains every mesh
// pod's filter chains, and needs the access-log field migrated. If it is ever
// dropped, the access log does NOT need it: an HCM
// access-log substitution can read the native object directly, because the
// per-stream filter state parents onto the connection-lifespan store. But
// beware the semantics change — the native object is the netns the LISTENER is
// bound in, so on the INBOUND listener it is the DESTINATION pod's netns, and a
// field still called `source_netns` would then be wrong. Rename it (the schema
// already has reporter-relative `pod_name`/`pod_namespace`, so `local_netns`
// fits) rather than repointing it in place.
func buildNetworkNamespaceFilterState() *listenerv3.Filter {
	return buildSetFilterState(networkNamespaceFilterStateKey, genericStringFactory, "%FILTER_STATE(envoy.network.network_namespace:PLAIN)%")
}

// buildCertMapperIdentityFilterState stamps the same SPIFFE ID under the name
// Envoy's filter_state_override upstream certificate mapper reads, using the
// HASHABLE string factory so the value also partitions the upstream connection
// pool. See SourceIdentityCertMapperFilterStateKey for why the key is an
// Envoy-owned literal and why the factory is load-bearing.
//
// The value is a LITERAL inline string (not a %FILTER_STATE(...)%
// substitution — Envoy has no notion of the pod's mesh identity, the control
// plane does): the identity proxy.SpiffeIDFromPod derives for the pod, i.e.
// exactly the name of the SDS secret whose certificate the upstream connection
// must present. A literal contains no '%', so SubstitutionFormatString passes
// it through unchanged.
//
// SharedWithUpstream: ONCE is not optional for this one — unlike the netns
// key it is REQUIRED for the feature to work at all. Both readers
// (hashKey and the mapper) look only at
// TransportSocketOptions::downstreamSharedFilterStateObjects(), which is
// populated exclusively from shared filter-state objects. An unshared object is
// invisible to both, and the failure is silent: the mapper returns its
// default_value (the node identity) and the pool key loses the identity term —
// i.e. exactly the #831 leak, minus the flag that used to mask it.
func buildCertMapperIdentityFilterState(sourceSpiffeID string) *listenerv3.Filter {
	return buildSetFilterState(SourceIdentityCertMapperFilterStateKey, hashableStringFactory, sourceSpiffeID)
}

// BuildSourceFilterStates returns the source-attribution network filters every
// mesh-originating filter chain carries, in a FIXED order (netns, then the
// certificate-mapper identity key): these land in the chain's repeated
// `filters` field, whose order is part of the listener's bytes and therefore of
// its delta-xDS hash.
//
// Until #1165 a third entry — the retired "aether.source.spiffe_id" copy —
// sat between the two; see SourceIdentityCertMapperFilterStateKey.
//
// sourceSpiffeID may be empty — before the trust domain is known there is no
// identity to stamp — in which case only the netns filter is emitted, which is
// byte-for-byte what this chain carried before issue #815. A chain built in
// that window selects no certificate by identity, so its egress presents the
// node identity (the mapper's default_value) until the next rebuild — the same
// degradation, for the same reason, as the pre-#842 OnNoMatch path.
func BuildSourceFilterStates(sourceSpiffeID string) []*listenerv3.Filter {
	filters := []*listenerv3.Filter{buildNetworkNamespaceFilterState()}
	if sourceSpiffeID != "" {
		filters = append(filters, buildCertMapperIdentityFilterState(sourceSpiffeID))
	}
	return filters
}

// buildSetFilterState creates a set_filter_state network filter that stores a
// value in filter state under objectKey, built by the named object factory.
func buildSetFilterState(objectKey, factoryKey string, inlineStringFormatString string) *listenerv3.Filter {
	filter := &set_filter_state_v3.Config{
		OnNewConnection: []*setFilterStatev3.FilterStateValue{
			{
				Key: &setFilterStatev3.FilterStateValue_ObjectKey{
					ObjectKey: objectKey,
				},
				FactoryKey: factoryKey,
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
				// Shared with the upstream connection so the cluster's certificate
				// mapper and the pool hash can read it (they see only
				// downstreamSharedFilterStateObjects()). ONCE (immediate upstream connection only) is sufficient for the
				// single-hop transport: TRANSITIVE — which re-propagates through any
				// further upstream hops — was HBONE/tunnel-era (#86) plumbing for the
				// two-layer upstream path and was left behind when #98 flattened the
				// transport; scoping it to one hop keeps a future chained/internal
				// hop from silently inheriting source-identity cert selection.
				SharedWithUpstream: setFilterStatev3.FilterStateValue_ONCE,
			},
		},
	}
	return networkFilter("envoy.filters.network.set_filter_state", filter)
}

// The HTTP connection managers' downstream idle timeout
// (common_http_protocol_options.idle_timeout) depends on WHO the downstream is.
// One constant used to serve both sides, and it was right for only one of them
// (aether#1350).
const (
	// peerFacingIdleTimeout is for an HCM whose downstream is a PEER PROXY: the
	// per-pod mesh inbound (TCP and QUIC, mTLS or cleartext). It is the
	// server-side backstop to the upstream config.UpstreamIdleTimeout: peers
	// reclaim their idle connections at 30s, so this only catches peers that do
	// not (Envoy's default is 1 hour, which let a peer's leaked upstream
	// connections pin ~7k inbound connections per listener). Kept well above
	// the upstream timeout so the client side always disconnects first.
	peerFacingIdleTimeout = 5 * time.Minute

	// appFacingIdleTimeout is for an HCM whose downstream is the LOCAL
	// APPLICATION: the per-pod capture chain (capture_http) and the per-pod
	// outbound listener (outbound_http). Here the proxy is the server of a
	// client it does not control, and the rule is that the server's idle
	// timeout must EXCEED the client's: HTTP/1.1 has no GOAWAY, so a request
	// written onto a kept-alive connection the proxy is closing at that instant
	// is lost, and a client that does not retry on a reused-connection failure
	// sees a reset (1 request in 270,000 under an Envoy-based load driver,
	// aether#1350). Ordinary clients idle their connections out after 30-120s;
	// the ones 5 minutes did not exceed are those that keep a spare connection
	// for as long as the server lets them.
	//
	// 1 hour is Envoy's own default, stated explicitly so that it is a decision
	// and a test can pin it. The cost: a connection an application leaks lives
	// on the proxy for up to an hour instead of 5 minutes. Nothing caps
	// downstream connections per listener; the node proxy's overload manager
	// (chart, proxy.overload) is what reclaims them under memory pressure, by
	// scaling this very timer down to 2s (reduce_timeouts,
	// HTTP_DOWNSTREAM_CONNECTION_IDLE) and disabling keep-alive.
	appFacingIdleTimeout = time.Hour
)

// buildHTTPConnectionManager creates an HTTP connection manager for processing HTTP traffic.
// It includes a router HTTP filter and uses the provided route configuration.
// If routeConfig is nil, routes will be retrieved via RDS. reporter tags the OTel
// access logger (ReporterSource for egress, ReporterDestination for inbound); the
// logger is attached only when access logging is enabled (buildAccessLog → nil).
//
// idleTimeout is the downstream connection idle timeout. It has no default on
// purpose: the right value depends on who the downstream is, so every caller
// says which (peerFacingIdleTimeout, appFacingIdleTimeout, or the edge's own
// edgeDefaultIdleTimeout).
func buildHTTPConnectionManager(name, reporter, podName, podNamespace string, routeConfig *routev3.RouteConfiguration, idleTimeout time.Duration) *http_connection_managerv3.HttpConnectionManager {
	return &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  http_connection_managerv3.HttpConnectionManager_AUTO,
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(idleTimeout),
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
