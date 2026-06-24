package proxy

import (
	"sort"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	matcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	tsinputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/transport_socket/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// filterStateInputName is the transport-socket-specific matcher input that
	// reads a filter state object from TransportSocketOptions.
	//
	// This is envoy.matching.inputs.transport_socket_filter_state
	// (envoy/extensions/matching/common_inputs/transport_socket/v3/FilterStateInput),
	// NOT the generic "envoy.matching.inputs.filter_state"
	// (envoy/extensions/matching/common_inputs/network/v3/FilterStateInput).
	//
	// The distinction is critical:
	//   - The generic input (filter_state) reads from StreamInfo::filterState()
	//     on the connection. It is registered for network/HTTP matcher contexts
	//     (filter chain matching, route matching, etc.).
	//   - The transport-socket-specific input (transport_socket_filter_state)
	//     reads from TransportSocketOptions::downstreamSharedFilterStateObjects(),
	//     which are the filter state objects propagated from the downstream
	//     connection via SharedWithUpstream. It is registered for the
	//     transport_socket_matcher context on a cluster.
	//
	// Using the generic name in a cluster transport_socket_matcher causes the
	// input factory to be resolved for the wrong matcher data type
	// (TransportSocketMatchingData instead of NetworkMatchingData). The factory
	// returns nullopt (no value), the exact-match never fires, and evaluation
	// falls to OnNoMatch — selecting the node identity for every upstream
	// connection regardless of source pod. Per-source cert selection is silently
	// broken. For tcp_proxy upstream connections the wrong socket can also cause
	// TLS handshake failures (wrong client cert vs SAN-pinned peer expectation),
	// manifesting as upstream_cx_total staying 0 because connections open but
	// immediately reset before the pool records them.
	filterStateInputName = "envoy.matching.inputs.transport_socket_filter_state"
	// transportSocketNameActionName is the matcher action extension that selects
	// a named transport socket.
	transportSocketNameActionName = "envoy.matching.action.transport_socket.name"
)

// UpstreamTransportSocketMatches returns one transport socket match per unique
// SPIFFE ID among the local workloads. Each match presents that workload's
// client certificate (over SPIRE-served SDS) for upstream mTLS. The match name
// is the SPIFFE ID itself so the matcher's TransportSocketNameAction can
// reference it; pods sharing a service account share a single match.
//
// Matches are emitted in sorted SPIFFE-ID order regardless of input order:
// transport_socket_matches is a repeated field, so its order is part of the
// cluster's bytes — and the delta-xDS cache decides "changed" by hashing those
// bytes. Callers build the ID list from map iteration; without the sort every
// snapshot bump (e.g. an SVID rotation) reshuffled the field, made every
// service cluster hash as changed, and sent Envoy a full CDS replace + EDS
// re-warm cycle each time (observed as `cds: N added/updated, skipped 0
// unmodified` + `initial fetch timed out` every push, and unbounded proxy
// memory growth under long-lived downstream connections).
func UpstreamTransportSocketMatches(spiffeIDs []string, validationContextName string, sanURIs []string, sni string) []*clusterv3.Cluster_TransportSocketMatch {
	unique := make([]string, 0, len(spiffeIDs))
	seen := make(map[string]struct{}, len(spiffeIDs))
	for _, id := range spiffeIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Strings(unique)

	matches := make([]*clusterv3.Cluster_TransportSocketMatch, 0, len(unique))
	for _, id := range unique {
		matches = append(matches, &clusterv3.Cluster_TransportSocketMatch{
			Name:            id,
			Match:           &structpb.Struct{},
			TransportSocket: UpstreamTransportSocket(id, validationContextName, sanURIs, sni),
		})
	}
	return matches
}

// UpstreamTransportSocketMatcher returns a matcher keyed on the
// aether.network.network_namespace filter state (set on each pod's outbound
// listener and shared with the upstream connection). Its exact_match_map maps
// each local pod's network namespace to a TransportSocketNameAction naming that
// pod's SPIFFE ID — the match name used by UpstreamTransportSocketMatches — so
// the upstream connection presents the originating pod's certificate.
//
// Returns nil when no valid netns→SPIFFE-ID entries exist: an empty
// exact_match_map fails proto validation (`MatchMapValidationError.Map: value
// must contain at least 1 pair(s)`), making Envoy NACK the entire CDS push —
// observed on agents starting before any local workload mapping exists, and
// permanent on nodes with zero managed pods (e2e 2026-06-10). Callers fall
// back to a plain transport socket.
func UpstreamTransportSocketMatcher(netnsToSpiffeID map[string]string) *matcherv3.Matcher {
	m := make(map[string]*matcherv3.Matcher_OnMatch, len(netnsToSpiffeID))
	for netns, id := range netnsToSpiffeID {
		if netns == "" || id == "" {
			continue
		}
		m[netns] = transportSocketNameOnMatch(id)
	}
	if len(m) == 0 {
		return nil
	}

	return &matcherv3.Matcher{
		MatcherType: &matcherv3.Matcher_MatcherTree_{
			MatcherTree: &matcherv3.Matcher_MatcherTree{
				Input: &xdscorev3.TypedExtensionConfig{
					Name:        filterStateInputName,
					TypedConfig: config.TypedConfig(&tsinputsv3.FilterStateInput{Key: networkNamespaceFilterStateKey}),
				},
				TreeType: &matcherv3.Matcher_MatcherTree_ExactMatchMap{
					ExactMatchMap: &matcherv3.Matcher_MatcherTree_MatchMap{
						Map: m,
					},
				},
			},
		},
	}
}

// UpstreamTCPTransportSocketMatches is UpstreamTransportSocketMatches for TCP floor
// clusters: each match uses UpstreamTCPTransportSocket (ALPN "aether-tcp") instead
// of UpstreamTransportSocket ("h2"). Match names are the bare SPIFFE ID — identical
// to the HTTP matches — because the shared UpstreamTransportSocketMatcher selects by
// the source pod's SPIFFE ID, and transport_socket_matches are scoped per cluster
// (the HTTP <svc> and TCP tcp:<svc> clusters are distinct, so the names never
// collide). A ":tcp" suffix would never be selected (the matcher emits the bare ID),
// leaving the floor connection on the default socket with no "aether-tcp" ALPN, so
// the inbound floor chain wouldn't match and the mTLS connection would reset.
func UpstreamTCPTransportSocketMatches(spiffeIDs []string, validationContextName string, sanURIs []string, sni string) []*clusterv3.Cluster_TransportSocketMatch {
	unique := make([]string, 0, len(spiffeIDs))
	seen := make(map[string]struct{}, len(spiffeIDs))
	for _, id := range spiffeIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Strings(unique)

	matches := make([]*clusterv3.Cluster_TransportSocketMatch, 0, len(unique))
	for _, id := range unique {
		matches = append(matches, &clusterv3.Cluster_TransportSocketMatch{
			Name:            id,
			Match:           &structpb.Struct{},
			TransportSocket: UpstreamTCPTransportSocket(id, validationContextName, sanURIs, sni),
		})
	}
	return matches
}

// transportSocketNameOnMatch builds an OnMatch that selects the named transport
// socket from the cluster's transport_socket_matches.
func transportSocketNameOnMatch(socketName string) *matcherv3.Matcher_OnMatch {
	return &matcherv3.Matcher_OnMatch{
		OnMatch: &matcherv3.Matcher_OnMatch_Action{
			Action: &xdscorev3.TypedExtensionConfig{
				Name:        transportSocketNameActionName,
				TypedConfig: config.TypedConfig(&tsinputsv3.TransportSocketNameAction{Name: socketName}),
			},
		},
	}
}
