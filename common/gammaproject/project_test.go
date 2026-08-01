package gammaproject

import (
	"testing"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

// TestServiceParents verifies Service parentRefs → route-target keys + ports (kind/group
// filtering, namespace inheritance, port pass-through).
func TestServiceParents(t *testing.T) {
	refs := []gatewayv1.ParentReference{
		{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-1", Port: ptr(gatewayv1.PortNumber(8080))},
		{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-2"}, // no port → 0
		{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"},  // ignored (not Service)
		{Name: "no-kind"}, // ignored (no kind)
	}
	assert.Equal(t, []ServiceParent{
		{Key: "team-a/svc-1", Port: 8080},
		{Key: "team-a/svc-2", Port: 0},
	}, ServiceParents(refs, "team-a"))
}

// TestProjectHTTPRule covers the shared projector's proto output: backends resolved to
// <ns>/<svc> keys + cluster names, path/header matches, header mutation, redirect, and
// an allow-listed ExtensionRef → ExtensionFilter.
func TestProjectHTTPRule(t *testing.T) {
	cfg, err := anypb.New(&configprotov1.HTTPFilterSpec{}) // any well-formed Any; allow-list+presence is what's checked
	require.NoError(t, err)
	httpFilters := map[string]*configprotov1.HTTPFilterSpec{
		"team-a/h2m": configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.header_mutation", TypedConfig: cfg,
		}.Build(),
	}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path:    &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/v2")},
			Headers: []gatewayv1.HTTPHeaderMatch{{Name: "x-canary", Value: "1"}},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo-v2"},
				Weight:                 ptr(int32(70)),
			},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{
			{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set: []gatewayv1.HTTPHeader{{Name: "X-Set", Value: "v"}},
			}},
			{Type: gatewayv1.HTTPRouteFilterExtensionRef, ExtensionRef: &gatewayv1.LocalObjectReference{
				Group: "config.aether.io", Kind: "HTTPFilter", Name: "h2m",
			}},
		},
	}

	gr := ProjectHTTPRule(rule, "team-a", "HTTPRoute", "aether.internal", nil, httpFilters, nil)

	require.Len(t, gr.GetBackends(), 1)
	assert.Equal(t, "team-a/echo-v2", gr.GetBackends()[0].GetService())
	assert.Equal(t, "echo-v2.team-a.aether.internal", gr.GetBackends()[0].GetCluster())
	assert.Equal(t, uint32(70), gr.GetBackends()[0].GetWeight())

	require.Len(t, gr.GetMatches(), 1)
	assert.Equal(t, "/v2", gr.GetMatches()[0].GetPrefix())
	require.Len(t, gr.GetMatches()[0].GetHeaders(), 1)
	assert.Equal(t, "x-canary", gr.GetMatches()[0].GetHeaders()[0].GetName())

	require.NotNil(t, gr.GetHeaderMutation())
	require.Len(t, gr.GetHeaderMutation().GetSetRequest(), 1)
	assert.Equal(t, "X-Set", gr.GetHeaderMutation().GetSetRequest()[0].GetName())

	require.Len(t, gr.GetExtensionFilters(), 1)
	assert.Equal(t, "envoy.filters.http.header_mutation", gr.GetExtensionFilters()[0].GetName())
}

// TestServiceFilters covers M3 policy attachment: an HTTPFilter with a targetRef to a
// Service resolves for that service (same-namespace, allow-listed), and does not leak
// to other services or namespaces.
func TestServiceFilters(t *testing.T) {
	cfg, err := anypb.New(&configprotov1.HTTPFilterSpec{})
	require.NoError(t, err)
	withTarget := func(filter, svc string) *configprotov1.HTTPFilterSpec {
		return configprotov1.HTTPFilterSpec_builder{
			Filter: filter, TypedConfig: cfg,
			TargetRefs: []*configprotov1.PolicyTargetRef{
				configprotov1.PolicyTargetRef_builder{Kind: "Service", Name: svc}.Build(),
			},
		}.Build()
	}
	httpFilters := map[string]*configprotov1.HTTPFilterSpec{
		"team-a/h2m": withTarget("envoy.filters.http.header_mutation", "echo"),     // attaches to team-a/echo
		"team-a/h2t": withTarget("envoy.filters.http.header_to_metadata", "other"), // different svc
		"team-b/dup": withTarget("envoy.filters.http.header_mutation", "echo"),     // different ns, same svc name
		"team-a/lua": withTarget("envoy.filters.http.lua", "echo"),                 // targets echo but NOT allow-listed
	}

	got := ServiceFilters("team-a/echo", httpFilters)
	require.Len(t, got, 1, "only the same-namespace, allow-listed filter targeting echo applies")
	assert.Equal(t, "envoy.filters.http.header_mutation", got[0].GetName())

	assert.Empty(t, ServiceFilters("team-a/none", httpFilters), "a service with no targeting filter gets none")

	// The resolved service filters attach to every projected route of the service.
	route := ProjectHTTPRule(gatewayv1.HTTPRouteRule{}, "team-a", "HTTPRoute", "aether.internal", nil, nil, got)
	require.Len(t, route.GetExtensionFilters(), 1)
	assert.Equal(t, "envoy.filters.http.header_mutation", route.GetExtensionFilters()[0].GetName())
}

// TestProjectHTTPRule_RedirectAndExtensionRefSkips covers a RequestRedirect → no
// backend, and that a non-allow-listed / wrong-group ExtensionRef is skipped.
func TestProjectHTTPRule_RedirectAndExtensionRefSkips(t *testing.T) {
	cfg, err := anypb.New(&configprotov1.HTTPFilterSpec{})
	require.NoError(t, err)
	httpFilters := map[string]*configprotov1.HTTPFilterSpec{
		"ns/lua": configprotov1.HTTPFilterSpec_builder{Filter: "envoy.filters.http.lua", TypedConfig: cfg}.Build(),
	}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Value: ptr("/r")}}},
		Filters: []gatewayv1.HTTPRouteFilter{
			{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Hostname: ptr(gatewayv1.PreciseHostname("example.org")), StatusCode: ptr(301),
			}},
			{Type: gatewayv1.HTTPRouteFilterExtensionRef, ExtensionRef: &gatewayv1.LocalObjectReference{
				Group: "config.aether.io", Kind: "HTTPFilter", Name: "lua", // not allow-listed
			}},
		},
	}
	gr := ProjectHTTPRule(rule, "ns", "HTTPRoute", "aether.internal", nil, httpFilters, nil)
	require.NotNil(t, gr.GetRedirect())
	assert.Equal(t, "example.org", gr.GetRedirect().GetHostname())
	assert.Equal(t, int32(301), gr.GetRedirect().GetStatusCode())
	assert.Empty(t, gr.GetExtensionFilters(), "non-allow-listed ExtensionRef is skipped")
}

// TestServiceChainFilter covers M4 CHAIN scope: a CHAIN-scope HTTPFilter with a
// Service targetRef resolves as the service-wide filter; ROUTE-scope filters and
// other namespaces/services don't; ServiceFilters (route-level) skips CHAIN specs.
func TestServiceChainFilter(t *testing.T) {
	cfg, err := anypb.New(&configprotov1.HTTPFilterSpec{})
	require.NoError(t, err)
	chain := func(filter, svc string) *configprotov1.HTTPFilterSpec {
		return configprotov1.HTTPFilterSpec_builder{
			Filter: filter, TypedConfig: cfg,
			Scope: configprotov1.HTTPFilterSpec_SCOPE_CHAIN,
			TargetRefs: []*configprotov1.PolicyTargetRef{
				configprotov1.PolicyTargetRef_builder{Kind: "Service", Name: svc}.Build(),
			},
		}.Build()
	}
	httpFilters := map[string]*configprotov1.HTTPFilterSpec{
		"team-a/chain-hm": chain("envoy.filters.http.header_mutation", "echo"),
		"team-b/other-ns": chain("envoy.filters.http.header_mutation", "echo"), // wrong ns
	}

	got := ServiceChainFilter("team-a/echo", httpFilters)
	require.NotNil(t, got)
	assert.Equal(t, "envoy.filters.http.header_mutation", got.GetName())
	assert.Nil(t, ServiceChainFilter("team-a/none", httpFilters))

	// ServiceFilters (route-level, M3) must SKIP chain-scope specs.
	assert.Empty(t, ServiceFilters("team-a/echo", httpFilters), "CHAIN specs are vhost-level, never per-route")

	// Deterministic tie-break: two chain filters (webhook-enforced not to happen) →
	// key order picks team-a/aaa.
	httpFilters["team-a/aaa"] = chain("envoy.filters.http.header_to_metadata", "echo")
	tie := ServiceChainFilter("team-a/echo", httpFilters)
	require.NotNil(t, tie)
	assert.Equal(t, "envoy.filters.http.header_to_metadata", tie.GetName())

	// Typed authoring form renders through the shared Render path.
	typedSpec := configprotov1.HTTPFilterSpec_builder{
		Scope: configprotov1.HTTPFilterSpec_SCOPE_CHAIN,
		TargetRefs: []*configprotov1.PolicyTargetRef{
			configprotov1.PolicyTargetRef_builder{Kind: "Service", Name: "typed"}.Build(),
		},
		HeaderToMetadata: configprotov1.HeaderToMetadata_builder{Rules: []*configprotov1.HeaderToMetadata_Rule{
			configprotov1.HeaderToMetadata_Rule_builder{Header: "x-canary", MetadataKey: "canary"}.Build(),
		}}.Build(),
	}.Build()
	httpFilters["team-a/typed"] = typedSpec
	tf := ServiceChainFilter("team-a/typed", httpFilters)
	require.NotNil(t, tf)
	assert.Equal(t, "envoy.filters.http.header_to_metadata", tf.GetName())
	assert.NotNil(t, tf.GetConfig())
}
