package proxy

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tsinputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/transport_socket/v3"
	filter_state_overridev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_mappers/filter_state_override/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// These used to cover UpstreamTransportSocketMatches / UpstreamTransportSocketMatcher
// — one socket and one exact_match_map entry per local SPIFFE ID. Issue #842
// deleted both: certificate selection moved into the transport socket itself
// (custom_tls_certificate_selector), so there is no per-identity list left to
// build, sort or de-duplicate. What follows covers the surface that replaced
// them, and keeps the two lessons those tests existed to enforce:
//
//   - NAMES MATCHED BY STRING FAIL OPEN. The old trap was
//     envoy.matching.inputs.filter_state vs
//     envoy.matching.inputs.transport_socket_filter_state (#301): the generic
//     input silently returns nullopt in a transport-socket matcher, every
//     connection takes on_no_match, and every workload's egress presents the
//     node identity. The new trap is the same shape in a different place — the
//     mapper's HARDCODED filter-state lookup key — so it is pinned here as a
//     literal, next to the two extension names (which are informational; see
//     TestUpstreamCertSelectorExtensionNames).
//   - DETERMINISM IS PROTOCOL-VISIBLE (#135). The old tests asserted sorted,
//     de-duplicated output because the matches were built from map iteration.
//     There is no longer any node-dependent input to reorder; the byte-equality
//     test in egress_test.go (TestInjectUpstreamMTLS_IndependentOfLocalWorkloads)
//     is the stronger successor.

// TestUpstreamCertSelectorExtensionNames pins the two extension names the
// per-connection certificate path is resolved by, plus the filter-state key the
// mapper looks up.
//
// ALL THREE FAIL SILENTLY, which is why they are pinned as literals here and
// not merely referenced:
//
//   - the SELECTOR and MAPPER names are informational. Envoy resolves both
//     extensions by the typed_config TYPE URL; a corrupted name was measured to
//     pass `envoy --mode validate` on the pinned proxy (2026-09-20). Nothing
//     downstream notices a rename.
//   - the FILTER-STATE KEY is worse: filter_state_override compares obj.name_
//     against the literal "envoy.tls.certificate_mappers.on_demand_secret" and,
//     finding nothing, returns default_value. Every connection then presents
//     the node identity, the handshake succeeds, and nothing errors.
func TestUpstreamCertSelectorExtensionNames(t *testing.T) {
	assert.Equal(t, "envoy.tls.certificate_selectors.on_demand_secret", onDemandCertSelectorName)
	assert.Equal(t, "envoy.tls.upstream_certificate_mappers.filter_state_override", filterStateCertMapperName)
	assert.Equal(t, "envoy.tls.certificate_mappers.on_demand_secret", SourceIdentityCertMapperFilterStateKey,
		"the mapper hardcodes this lookup name; it is Envoy's to choose, not ours")
	assert.Equal(t, "envoy.hashable_string", hashableStringFactory,
		"the non-hashable envoy.string factory keeps the identity out of the upstream pool key")
}

// TestUpstreamCertSelector pins the selector's wiring: ADS config source, the
// filter-state mapper, and a non-empty default_value (the proto requires
// min_len: 1, so an empty one would NACK the whole CDS push).
func TestUpstreamCertSelector(t *testing.T) {
	node := "spiffe://aether.internal/ns/aether-system/sa/aether-agent"
	sel := upstreamCertSelector(node, config.XDSConfigSourceADS())

	assert.Equal(t, onDemandCertSelectorName, sel.GetName())

	var onDemand on_demand_secretv3.Config
	require.NoError(t, proto.Unmarshal(sel.GetTypedConfig().GetValue(), &onDemand))
	require.NotNil(t, onDemand.GetConfigSource().GetAds())
	assert.Equal(t, []string{node}, onDemand.GetPrefetchSecretNames())

	require.NotNil(t, onDemand.GetCertificateMapper())
	assert.Equal(t, filterStateCertMapperName, onDemand.GetCertificateMapper().GetName())

	var mapper filter_state_overridev3.Config
	require.NoError(t, proto.Unmarshal(onDemand.GetCertificateMapper().GetTypedConfig().GetValue(), &mapper))
	assert.Equal(t, node, mapper.GetDefaultValue())
	assert.NotEmpty(t, mapper.GetDefaultValue(), "default_value is required (min_len: 1)")
}

// TestWaypointTransportSocketMatches pins the only transport_socket_matches the
// mesh still emits: exactly two, named by bounded constants, differing only in
// SNI, both resolving the certificate through the same selector.
func TestWaypointTransportSocketMatches(t *testing.T) {
	node := "spiffe://aether.internal/ns/aether-system/sa/aether-agent"
	matches := WaypointTransportSocketMatches(node, "spiffe://aether.internal", nil, "8080", "8080.echo.ns.mesh.local")

	require.Len(t, matches, 2, "a fixed two, independent of the node's identity set")
	assert.Equal(t, localSocketName, matches[0].GetName())
	assert.Equal(t, waypointSocketName, matches[1].GetName())

	for _, m := range matches {
		assert.NotNil(t, m.GetMatch(), "match criteria must be an empty struct, not nil")
		require.NotNil(t, m.GetTransportSocket())

		var ctx transport_sockets_v3.UpstreamTlsContext
		require.NoError(t, proto.Unmarshal(m.GetTransportSocket().GetTypedConfig().GetValue(), &ctx))
		// No statically named client certificate: the selector owns resolution.
		assert.Empty(t, ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs())
		require.NotNil(t, ctx.GetCommonTlsContext().GetCustomTlsCertificateSelector())
		assert.Equal(t, "spiffe://aether.internal", ctx.GetCommonTlsContext().GetValidationContextSdsSecretConfig().GetName())
	}

	assert.Equal(t, "8080", sniOf(t, matches[0]))
	assert.Equal(t, "8080.echo.ns.mesh.local", sniOf(t, matches[1]))
}

// TestWaypointTransportSocketMatcher pins the surviving matcher: ONE level, on
// the chosen endpoint's metadata, selecting a socket by name.
func TestWaypointTransportSocketMatcher(t *testing.T) {
	matcher := WaypointTransportSocketMatcher()

	tree := matcher.GetMatcherTree()
	require.NotNil(t, tree)
	assert.Equal(t, endpointMetadataInputName, tree.GetInput().GetName())

	m := tree.GetExactMatchMap().GetMap()
	require.Len(t, m, 1)
	onMatch, ok := m[subsetWaypointValue]
	require.True(t, ok)

	var nameAction tsinputsv3.TransportSocketNameAction
	require.NoError(t, proto.Unmarshal(onMatch.GetAction().GetTypedConfig().GetValue(), &nameAction))
	assert.Equal(t, transportSocketNameActionName, onMatch.GetAction().GetName())
	assert.Equal(t, waypointSocketName, nameAction.GetName())

	require.NoError(t, proto.Unmarshal(matcher.GetOnNoMatch().GetAction().GetTypedConfig().GetValue(), &nameAction))
	assert.Equal(t, localSocketName, nameAction.GetName(),
		"an endpoint without the waypoint tag takes the local (port-SNI) socket")
}

// TestWaypointTransportSocketMatcherDeterministic: the matcher and its matches
// are constants now, so repeated builds must be byte-identical. That is the
// #135 property (a reshuffled repeated field re-hashes the cluster and triggers
// a full CDS replace) made trivially true rather than merely tested for.
func TestWaypointTransportSocketMatcherDeterministic(t *testing.T) {
	node := "spiffe://aether.internal/ns/aether-system/sa/aether-agent"
	assert.True(t, proto.Equal(WaypointTransportSocketMatcher(), WaypointTransportSocketMatcher()))

	a := WaypointTransportSocketMatches(node, "spiffe://aether.internal", nil, "8080", "wp")
	b := WaypointTransportSocketMatches(node, "spiffe://aether.internal", nil, "8080", "wp")
	require.Len(t, a, len(b))
	for i := range a {
		assert.True(t, proto.Equal(a[i], b[i]), "match %d must be identical across builds", i)
	}
}

func sniOf(t *testing.T, m interface {
	GetTransportSocket() *corev3.TransportSocket
},
) string {
	t.Helper()
	var ctx transport_sockets_v3.UpstreamTlsContext
	require.NoError(t, proto.Unmarshal(m.GetTransportSocket().GetTypedConfig().GetValue(), &ctx))
	return ctx.GetSni()
}
