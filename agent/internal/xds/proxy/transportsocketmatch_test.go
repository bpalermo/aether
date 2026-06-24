package proxy

import (
	"testing"

	tsinputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/transport_socket/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestUpstreamTransportSocketMatches(t *testing.T) {
	idA := "spiffe://aether.internal/ns/aether-test/sa/echo"
	idB := "spiffe://aether.internal/ns/aether-test/sa/client"

	// Duplicate idA (two pods, same service account) collapses to one match;
	// empty IDs are skipped.
	matches := UpstreamTransportSocketMatches([]string{idA, idB, idA, ""}, "spiffe://aether.internal", nil, "")

	require.Len(t, matches, 2)
	names := []string{matches[0].GetName(), matches[1].GetName()}
	assert.ElementsMatch(t, []string{idA, idB}, names)

	for _, m := range matches {
		assert.NotNil(t, m.GetMatch(), "match criteria must be an empty struct, not nil")
		require.NotNil(t, m.GetTransportSocket())

		var ctx transport_sockets_v3.UpstreamTlsContext
		require.NoError(t, proto.Unmarshal(m.GetTransportSocket().GetTypedConfig().GetValue(), &ctx))
		// Each match presents its own SPIFFE ID's client cert.
		assert.Equal(t, m.GetName(), ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()[0].GetName())
		assert.Equal(t, "spiffe://aether.internal", ctx.GetCommonTlsContext().GetValidationContextSdsSecretConfig().GetName())
	}
}

func TestUpstreamTransportSocketMatcher(t *testing.T) {
	netnsA := "/var/run/netns/cni-a"
	idA := "spiffe://aether.internal/ns/aether-test/sa/echo"

	matcher := UpstreamTransportSocketMatcher(map[string]string{netnsA: idA, "": "x", "y": ""})

	tree := matcher.GetMatcherTree()
	require.NotNil(t, tree)

	// Input must use the transport-socket-specific FilterStateInput extension
	// (envoy.matching.inputs.transport_socket_filter_state), NOT the generic
	// network/HTTP filter_state input. The transport-socket variant reads from
	// TransportSocketOptions::downstreamSharedFilterStateObjects() — the objects
	// propagated from the downstream connection via SharedWithUpstream. The
	// generic variant reads from StreamInfo::filterState() directly and is
	// registered for network/HTTP matcher contexts, not transport-socket
	// contexts; using it in a cluster transport_socket_matcher causes the input
	// to return nullopt silently, the exact-match never fires, and all
	// connections fall to OnNoMatch (node identity, never per-source cert).
	assert.Equal(t, filterStateInputName, tree.GetInput().GetName(),
		"must be envoy.matching.inputs.transport_socket_filter_state, not the generic filter_state")
	var input tsinputsv3.FilterStateInput
	require.NoError(t, proto.Unmarshal(tree.GetInput().GetTypedConfig().GetValue(), &input))
	assert.Equal(t, networkNamespaceFilterStateKey, input.GetKey())

	// Only the valid netns->id pair is present; the action names the SPIFFE ID.
	m := tree.GetExactMatchMap().GetMap()
	require.Len(t, m, 1)
	onMatch, ok := m[netnsA]
	require.True(t, ok)

	action := onMatch.GetAction()
	assert.Equal(t, transportSocketNameActionName, action.GetName())
	var nameAction tsinputsv3.TransportSocketNameAction
	require.NoError(t, proto.Unmarshal(action.GetTypedConfig().GetValue(), &nameAction))
	assert.Equal(t, idA, nameAction.GetName())
}

// TestUpstreamTransportSocketMatcherExtensionName is a regression test for the
// transport-socket-specific FilterStateInput extension name. Using the wrong
// extension name ("envoy.matching.inputs.filter_state", the generic network/HTTP
// input) in a cluster transport_socket_matcher causes Envoy to silently return
// nullopt from the input (wrong data type for TransportSocketMatchingData), so
// the exact-match never fires and every upstream connection falls to OnNoMatch.
// This results in all egress presenting the node identity rather than the
// originating pod's, and for TCP floor clusters causes TLS handshake failures
// (client cert SAN mismatch), manifesting as upstream_cx_total = 0.
func TestUpstreamTransportSocketMatcherExtensionName(t *testing.T) {
	matcher := UpstreamTransportSocketMatcher(map[string]string{"/var/run/netns/cni-a": "spiffe://aether.internal/ns/ns/sa/sa"})
	name := matcher.GetMatcherTree().GetInput().GetName()
	assert.Equal(t, "envoy.matching.inputs.transport_socket_filter_state", name,
		"must be the transport-socket-scoped FilterStateInput, not the generic network filter_state input")
	assert.NotEqual(t, "envoy.matching.inputs.filter_state", name,
		"the generic filter_state input silently returns nullopt in a transport_socket_matcher context")
}

// TestUpstreamTransportSocketMatchesDeterministic verifies the matches are
// byte-identical regardless of input order: transport_socket_matches order is
// part of the cluster's delta-xDS version hash, and callers build the ID list
// from map iteration — a reshuffled order made every service cluster hash as
// changed on every snapshot bump (full CDS replace + EDS re-warm per push).
func TestUpstreamTransportSocketMatchesDeterministic(t *testing.T) {
	idA := "spiffe://aether.internal/ns/aether-test/sa/echo"
	idB := "spiffe://aether.internal/ns/aether-test/sa/client"
	idC := "spiffe://aether.internal/ns/aether-test/sa/loadgen"

	a := UpstreamTransportSocketMatches([]string{idC, idA, idB}, "spiffe://aether.internal", nil, "")
	b := UpstreamTransportSocketMatches([]string{idB, idC, idA, idB}, "spiffe://aether.internal", nil, "")

	require.Len(t, a, 3)
	require.Len(t, b, 3)
	// Sorted output order, independent of input order.
	assert.Equal(t, []string{idB, idA, idC}, []string{a[0].GetName(), a[1].GetName(), a[2].GetName()})
	for i := range a {
		assert.True(t, proto.Equal(a[i], b[i]), "match %d must be identical across input orders", i)
	}
}
