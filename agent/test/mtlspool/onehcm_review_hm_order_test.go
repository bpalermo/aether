package mtlspool

// A service-wide header_mutation and an HTTPRoute rule's header_mutation
// that write the SAME header (found by the adversarial review of #1703).
// Natively (the virtual host's config on the virtual host, the rule's on the
// route) the pinned proxy applies the levels most-specific FIRST
// (header_mutation.cc reverses getAllPerFilterConfig unless
// most_specific_header_mutations_wins), so the service's write lands last.
// cap_http keeps the two levels native under the full name; the short name
// folds them onto the route (extensionfilter.CombineLevels), which must put
// the route's mutations first: the captured path, the proxy port (the
// loopback listener's out_http, native levels) and the short name must agree.

import (
	"testing"

	"aethermesh.dev/common/extensionfilter"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/require"
)

func rvSet(key, value string) *header_mutationv3.HeaderMutationPerRoute {
	return &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: key, Value: value}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}}}},
	}}
}

func TestOneHCM_LiveHeaderMutationSameHeaderBothLevels(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	flLevels(t, r, extensionfilter.HeaderMutationFilterName, flAny(t, rvSet("x-fl-service", "service")), flAny(t, rvSet("x-fl-service", "route")))
	captured := flGet(capture, ppMesh+":8080")
	viaProxy := flGet(proxyPort, ppMesh+":8080")
	t.Logf("MEASURED captured dial (compiled cap_http) Host %s:8080 -> %s", ppMesh, captured)
	t.Logf("MEASURED proxy port (native levels)        Host %s:8080 -> %s", ppMesh, viaProxy)
	require.NoError(t, captured.err)
	require.NoError(t, viaProxy.err)
	require.Equal(t, 200, captured.status)
	require.Equal(t, 200, viaProxy.status)
	require.Equal(t, viaProxy.header.Get("X-Fl-Service"), captured.header.Get("X-Fl-Service"),
		"the same request under the full name gets a different service/route header_mutation outcome on the captured path than on the proxy port")
	require.Equal(t, "service", viaProxy.header.Get("X-Fl-Service"), "natively the virtual host's write lands last")
	flSame(t, "the same header at both levels", capture, proxyPort, "X-Fl-Service")
}
