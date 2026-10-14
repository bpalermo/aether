package extensionfilter

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	rbac_configv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	rbac_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// levelCase is, for one allow-listed filter: a virtual-host config, a route
// config, and what the single combined config must be.
type levelCase struct {
	vhost, route proto.Message
	// check asserts the combined config; vhost and route are the Anys given.
	check func(t *testing.T, combined, vhost, route *anypb.Any)
}

func stamp(key string) *mutation_rulesv3.HeaderMutation {
	return &mutation_rulesv3.HeaderMutation{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{Key: key, Value: "ran"},
	}}}
}

// levelCases has one entry per allow-listed filter. A filter that is added to
// the allow-list and not here fails TestEveryAllowedFilterSaysHowItsLevelsCombine.
func levelCases() map[string]levelCase {
	routeWins := func(t *testing.T, combined, _, route *anypb.Any) {
		t.Helper()
		assert.True(t, proto.Equal(route, combined), "the most specific level is the whole config: the route's")
	}
	return map[string]levelCase{
		HeaderToMetadataFilterName: {
			vhost: &header_to_metadatav3.Config{RequestRules: []*header_to_metadatav3.Config_Rule{{Header: "x-vhost"}}},
			route: &header_to_metadatav3.Config{RequestRules: []*header_to_metadatav3.Config_Rule{{Header: "x-route"}}},
			check: routeWins,
		},
		RBACFilterName: {
			vhost: &rbac_filterv3.RBACPerRoute{Rbac: &rbac_filterv3.RBAC{Rules: &rbac_configv3.RBAC{Action: rbac_configv3.RBAC_DENY}}},
			route: &rbac_filterv3.RBACPerRoute{Rbac: &rbac_filterv3.RBAC{Rules: &rbac_configv3.RBAC{Action: rbac_configv3.RBAC_ALLOW}}},
			check: routeWins,
		},
		HeaderMutationFilterName: {
			vhost: &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
				RequestMutations: []*mutation_rulesv3.HeaderMutation{stamp("x-vhost-req")}, ResponseMutations: []*mutation_rulesv3.HeaderMutation{stamp("x-vhost")},
			}},
			route: &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
				ResponseMutations: []*mutation_rulesv3.HeaderMutation{stamp("x-route")},
			}},
			check: func(t *testing.T, combined, _, _ *anypb.Any) {
				t.Helper()
				var got header_mutationv3.HeaderMutationPerRoute
				require.NoError(t, combined.UnmarshalTo(&got))
				keys := func(ms []*mutation_rulesv3.HeaderMutation) []string {
					var out []string
					for _, m := range ms {
						out = append(out, m.GetAppend().GetHeader().GetKey())
					}
					return out
				}
				assert.Equal(t, []string{"x-route", "x-vhost"}, keys(got.GetMutations().GetResponseMutations()), "every level applies, the route's first (header_mutation.cc reverses the levels): the virtual host's write lands last")
				assert.Equal(t, []string{"x-vhost-req"}, keys(got.GetMutations().GetRequestMutations()))
			},
		},
		ExtAuthzFilterName: {
			vhost: &ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{
				ContextExtensions: map[string]string{"tenant": "a", "tier": "service"}, DisableRequestBodyBuffering: true,
			}}},
			route: &ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{
				ContextExtensions: map[string]string{"tier": "route", "action": "write"},
			}}},
			check: func(t *testing.T, combined, _, _ *anypb.Any) {
				t.Helper()
				var got ext_authzv3.ExtAuthzPerRoute
				require.NoError(t, combined.UnmarshalTo(&got))
				assert.False(t, got.GetDisabled())
				assert.Equal(t, map[string]string{"tenant": "a", "tier": "route", "action": "write"}, got.GetCheckSettings().GetContextExtensions(),
					"the union, the route's entry winning a key")
				assert.False(t, got.GetCheckSettings().GetDisableRequestBodyBuffering(), "the other check settings are the route's")
			},
		},
	}
}

// TestEveryAllowedFilterSaysHowItsLevelsCombine: the allow-list, the table of
// level semantics and this test's cases have the same keys. A filter cannot be
// allow-listed without saying what its virtual-host and route configs amount
// to together, and without a case here that holds it.
func TestEveryAllowedFilterSaysHowItsLevelsCombine(t *testing.T) {
	cases := levelCases()
	assert.ElementsMatch(t, slices.Collect(maps.Keys(allowed)), slices.Collect(maps.Keys(levels)), "allow-list vs level semantics")
	assert.ElementsMatch(t, slices.Collect(maps.Keys(allowed)), slices.Collect(maps.Keys(cases)), "allow-list vs the cases of this test")

	for name := range allowed {
		t.Run(name, func(t *testing.T) {
			sem, ok := LevelSemanticsOf(name)
			require.True(t, ok)
			tc := cases[name]
			vhost, err := anypb.New(tc.vhost)
			require.NoError(t, err)
			route, err := anypb.New(tc.route)
			require.NoError(t, err)
			vhostBefore, routeBefore := proto.Clone(vhost), proto.Clone(route)

			combined, err := CombineLevels(name, vhost, route)
			require.NoError(t, err)
			require.NotNil(t, combined)
			tc.check(t, combined, vhost, route)
			if sem != MostSpecificLevelWins {
				assert.False(t, proto.Equal(route, combined), "the route's entry alone drops the virtual host's level")
			}
			assert.True(t, proto.Equal(vhostBefore, vhost), "the virtual host's config was written to")
			assert.True(t, proto.Equal(routeBefore, route), "the route's config was written to")

			// One level alone is that level.
			only, err := CombineLevels(name, vhost, nil)
			require.NoError(t, err)
			assert.Same(t, vhost, only)
			only, err = CombineLevels(name, nil, route)
			require.NoError(t, err)
			assert.Same(t, route, only)
		})
	}
}

// TestCombineLevels_ExtAuthz: the cases of the filter's own merge.
func TestCombineLevels_ExtAuthz(t *testing.T) {
	settings := func(ctx map[string]string) *anypb.Any {
		a, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{ContextExtensions: ctx}}})
		require.NoError(t, err)
		return a
	}
	disabled, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_Disabled{Disabled: true}})
	require.NoError(t, err)
	decode := func(a *anypb.Any) *ext_authzv3.ExtAuthzPerRoute {
		var out ext_authzv3.ExtAuthzPerRoute
		require.NoError(t, a.UnmarshalTo(&out))
		return &out
	}

	// The route disables the check: disabled, whatever the service says.
	got, err := CombineLevels(ExtAuthzFilterName, settings(map[string]string{"k": "v"}), disabled)
	require.NoError(t, err)
	assert.True(t, decode(got).GetDisabled())

	// The service disables it and the route has settings: checked, with the
	// route's settings (the more specific level decides "disabled").
	got, err = CombineLevels(ExtAuthzFilterName, disabled, settings(map[string]string{"k": "route"}))
	require.NoError(t, err)
	assert.False(t, decode(got).GetDisabled())
	assert.Equal(t, map[string]string{"k": "route"}, decode(got).GetCheckSettings().GetContextExtensions())

	// The route has settings with no context: the service's context stays.
	got, err = CombineLevels(ExtAuthzFilterName, settings(map[string]string{"tenant": "a"}), settings(nil))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"tenant": "a"}, decode(got).GetCheckSettings().GetContextExtensions())
}

// TestCombineLevels_FailsClosed: a filter that is not allow-listed, or a config
// that does not decode as the filter's, is an error and never a guess.
func TestCombineLevels_FailsClosed(t *testing.T) {
	a, err := anypb.New(&corev3.Metadata{})
	require.NoError(t, err)
	_, err = CombineLevels("envoy.filters.http.not_allowed", a, a)
	require.Error(t, err)
	for _, name := range []string{HeaderMutationFilterName, ExtAuthzFilterName} {
		_, err = CombineLevels(name, a, a)
		require.Errorf(t, err, "%s: a config of another type was combined", name)
	}
	_, ok := LevelSemanticsOf("envoy.filters.http.not_allowed")
	assert.False(t, ok)
}

// TestCombineLevels_SameBytesEveryTime: a combined config is part of a route
// table, and a table's bytes are its version. The same two levels must combine
// to the same bytes on every call, map fields included.
func TestCombineLevels_SameBytesEveryTime(t *testing.T) {
	ctx := func(n int, tag string) map[string]string {
		m := map[string]string{}
		for i := range n {
			m[fmt.Sprintf("%s-%02d", tag, i)] = tag
		}
		return m
	}
	vhost, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{ContextExtensions: ctx(12, "service")}}})
	require.NoError(t, err)
	route, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{ContextExtensions: ctx(12, "route")}}})
	require.NoError(t, err)
	first, err := CombineLevels(ExtAuthzFilterName, vhost, route)
	require.NoError(t, err)
	for range 50 {
		again, err := CombineLevels(ExtAuthzFilterName, vhost, route)
		require.NoError(t, err)
		require.Equal(t, first.GetValue(), again.GetValue(), "the same two levels combined to different bytes")
	}
}

// TestCombineExtAuthz_AsTheProxyMergesTheLevels holds combineExtAuthz to
// FilterConfigPerRoute(less_specific, more_specific) at the pinned Envoy
// (source/extensions/filters/http/ext_authz/ext_authz.cc): a service override
// is the route's or none, never inherited from the virtual host; the context
// extensions are the union even when the route sets no check settings; and
// emit_client_span is the route's when set, else the virtual host's.
func TestCombineExtAuthz_AsTheProxyMergesTheLevels(t *testing.T) {
	override := &corev3.GrpcService{TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "service-override"}}}
	vhost, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{
		ContextExtensions: map[string]string{"tier": "service"},
		ServiceOverride:   &ext_authzv3.CheckSettings_GrpcService{GrpcService: override},
	}}})
	require.NoError(t, err)
	combine := func(route *ext_authzv3.ExtAuthzPerRoute) *ext_authzv3.ExtAuthzPerRoute {
		t.Helper()
		r, err := anypb.New(route)
		require.NoError(t, err)
		out, err := CombineLevels(ExtAuthzFilterName, vhost, r)
		require.NoError(t, err)
		var got ext_authzv3.ExtAuthzPerRoute
		require.NoError(t, out.UnmarshalTo(&got))
		return &got
	}

	got := combine(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{
		ContextExtensions: map[string]string{"action": "write"},
	}}})
	assert.Nil(t, got.GetCheckSettings().GetGrpcService(), "the virtual host's service override is not inherited")
	assert.Equal(t, map[string]string{"tier": "service", "action": "write"}, got.GetCheckSettings().GetContextExtensions())

	// A route that only enables the filter: the virtual host's context
	// extensions still apply (the merged config takes them from both levels).
	got = combine(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_Disabled{Disabled: false}})
	assert.False(t, got.GetDisabled())
	assert.Equal(t, map[string]string{"tier": "service"}, got.GetCheckSettings().GetContextExtensions())
	assert.Nil(t, got.GetCheckSettings().GetGrpcService())

	// A route that disables the check is the whole answer.
	got = combine(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_Disabled{Disabled: true}})
	assert.True(t, got.GetDisabled())

	// The route's own override is kept.
	mine := &corev3.GrpcService{TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "route-override"}}}
	got = combine(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{
		ServiceOverride: &ext_authzv3.CheckSettings_GrpcService{GrpcService: mine},
	}}})
	assert.Equal(t, "route-override", got.GetCheckSettings().GetGrpcService().GetEnvoyGrpc().GetClusterName())
}
