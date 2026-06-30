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

	gr := ProjectHTTPRule(rule, "team-a", "HTTPRoute", "aether.internal", nil, httpFilters)

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
	gr := ProjectHTTPRule(rule, "ns", "HTTPRoute", "aether.internal", nil, httpFilters)
	require.NotNil(t, gr.GetRedirect())
	assert.Equal(t, "example.org", gr.GetRedirect().GetHostname())
	assert.Equal(t, int32(301), gr.GetRedirect().GetStatusCode())
	assert.Empty(t, gr.GetExtensionFilters(), "non-allow-listed ExtensionRef is skipped")
}
