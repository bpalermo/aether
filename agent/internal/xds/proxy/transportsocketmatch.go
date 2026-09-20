package proxy

import (
	"aethermesh.dev/agent/internal/xds/config"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	matcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	tsinputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/transport_socket/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// PER-SOURCE CERTIFICATE SELECTION NO LONGER LIVES HERE (issue #842).
//
// Until #842 every mesh service cluster carried a `transport_socket_matches`
// list with one entry per local SPIFFE ID and a `transport_socket_matcher`
// whose exact_match_map named each of them, keyed on the source-identity filter
// state via envoy.matching.inputs.transport_socket_filter_state. That structure
// is gone: a cluster now carries ONE transport socket whose
// custom_tls_certificate_selector resolves the client certificate per
// connection (upstreamCertSelector, transportsocket.go).
//
// Three consequences worth knowing before reviving anything like it:
//
//   - A cluster's bytes no longer depend on the node's identity set AT ALL.
//     #815 release two made them invariant under pod churn WITHIN a
//     ServiceAccount; the first pod of a new ServiceAccount arriving, and the
//     last one leaving, still rewrote every mesh cluster and cost one 15 s
//     EDS re-warm each. Those two events are now free as well.
//   - The per-match stat `cluster.<c>.<match>.total_match_count` disappears
//     with the matches. See docs/runbook.md, "#842": the replacement
//     no-match signal is the source-side access log's `source_spiffe_id`
//     (absent/"-" is precisely the condition that makes the certificate mapper
//     fall back to default_value) plus the destination-side
//     `downstream_peer_uri_san`, which was already the authoritative check.
//   - The "wrong matcher input name" trap is gone with the input: the generic
//     envoy.matching.inputs.filter_state reads StreamInfo filter state and
//     silently returns nullopt in a transport-socket matcher, which used to
//     mean every connection took on_no_match and presented the node identity
//     (#301). The mapper has the same failure mode for a different reason — it
//     hardcodes its lookup name — which is why
//     SourceIdentityCertMapperFilterStateKey is spelled out in one place.
//
// What REMAINS here is the proposal 019 waypoint split, which is not about
// identity at all: a cross-cluster endpoint must be dialed with a structured
// SNI and a local one with the port SNI, and SNI is a property of the transport
// socket, not of the certificate. That needs two sockets and a matcher on the
// CHOSEN ENDPOINT's metadata — two fixed entries, independent of how many
// identities the node hosts.

const (
	// transportSocketNameActionName is the matcher action extension that selects
	// a named transport socket.
	transportSocketNameActionName = "envoy.matching.action.transport_socket.name"
	// endpointMetadataInputName reads the CHOSEN upstream endpoint's metadata in
	// the transport-socket-matcher context (Envoy TransportSocketMatchingData,
	// after LB selection). Used to branch on the waypoint tag stamped on
	// cross-cluster endpoints (proposal 019) so a single cluster can present a
	// different SNI per endpoint.
	endpointMetadataInputName = "envoy.matching.inputs.endpoint_metadata"

	// localSocketName / waypointSocketName are the ONLY two transport-socket
	// match names the mesh emits since #842, and only on a waypoint-enabled
	// cluster. They are deliberately short constants rather than identities:
	// a match name ends up in the Envoy stat `cluster.<c>.<name>.total_match_count`,
	// and SPIFFE-ID-named matches used to put one metric family per
	// ServiceAccount into Prometheus' name index (the 2026-09-19 17:32Z trap the
	// chart's aether.transport_socket_match tag regex exists to undo).
	localSocketName    = "local"
	waypointSocketName = "waypoint"
)

// WaypointTransportSocketMatches returns the two transport sockets a
// waypoint-enabled mesh cluster carries: the LOCAL one (port SNI) and the
// WAYPOINT one (structured SNI). Both resolve the client certificate through
// the same per-connection certificate selector, so they differ in exactly one
// field — the SNI — which is the whole reason two sockets are still needed.
//
// The list is a fixed two entries whatever the node hosts, so it contributes
// nothing to cluster churn.
func WaypointTransportSocketMatches(nodeSpiffeID, validationContextName string, sanURIs []string, sni, waypointSNI string) []*clusterv3.Cluster_TransportSocketMatch {
	return []*clusterv3.Cluster_TransportSocketMatch{
		{
			Name:            localSocketName,
			Match:           &structpb.Struct{},
			TransportSocket: MeshUpstreamTransportSocket(nodeSpiffeID, validationContextName, sanURIs, sni),
		},
		{
			Name:            waypointSocketName,
			Match:           &structpb.Struct{},
			TransportSocket: MeshUpstreamTransportSocket(nodeSpiffeID, validationContextName, sanURIs, waypointSNI),
		},
	}
}

// WaypointTransportSocketMatcher branches on the CHOSEN endpoint's envoy.lb
// "waypoint" metadata (proposal 019 Design A): a waypoint-tagged
// (cross-cluster) endpoint selects the waypoint socket's structured SNI, every
// other endpoint the local socket's port SNI.
//
// Before #842 this was a two-LEVEL matcher — waypoint metadata, then source
// identity — because the second level chose the certificate. The certificate
// selector does that now, so only the first level survives and the matcher is
// a constant.
func WaypointTransportSocketMatcher() *matcherv3.Matcher {
	return &matcherv3.Matcher{
		MatcherType: &matcherv3.Matcher_MatcherTree_{
			MatcherTree: &matcherv3.Matcher_MatcherTree{
				Input: &xdscorev3.TypedExtensionConfig{
					Name: endpointMetadataInputName,
					TypedConfig: config.TypedConfig(&tsinputsv3.EndpointMetadataInput{
						Filter: envoyFilterMetadataSubsetNamespace,
						Path: []*tsinputsv3.EndpointMetadataInput_PathSegment{{
							Segment: &tsinputsv3.EndpointMetadataInput_PathSegment_Key{Key: subsetWaypointKey},
						}},
					}),
				},
				TreeType: &matcherv3.Matcher_MatcherTree_ExactMatchMap{
					ExactMatchMap: &matcherv3.Matcher_MatcherTree_MatchMap{
						Map: map[string]*matcherv3.Matcher_OnMatch{
							subsetWaypointValue: transportSocketNameOnMatch(waypointSocketName),
						},
					},
				},
			},
		},
		// Any endpoint without the waypoint tag: the local (port-SNI) socket.
		OnNoMatch: transportSocketNameOnMatch(localSocketName),
	}
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
