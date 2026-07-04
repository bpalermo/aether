package extensionfilter

import (
	"testing"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestAllowedAndDefaultConfig(t *testing.T) {
	assert.True(t, Allowed("envoy.filters.http.header_mutation"))
	assert.True(t, Allowed("envoy.filters.http.header_to_metadata"))
	assert.False(t, Allowed("envoy.filters.http.lua"))

	c, ok := DefaultConfig("envoy.filters.http.header_mutation")
	require.True(t, ok)
	assert.IsType(t, &header_mutationv3.HeaderMutation{}, c)
	_, ok = DefaultConfig("envoy.filters.http.lua")
	assert.False(t, ok)
}

func TestValidateSpec(t *testing.T) {
	// A valid header_mutation per-route config (the route's typed_per_filter_config type).
	validPerRoute, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	h2mCfg, err := anypb.New(&header_to_metadatav3.Config{})
	require.NoError(t, err)

	spec := func(filter string, scope configprotov1.HTTPFilterSpec_Scope, tc *anypb.Any) *configprotov1.HTTPFilterSpec {
		return configprotov1.HTTPFilterSpec_builder{Filter: filter, Scope: scope, TypedConfig: tc}.Build()
	}

	tests := []struct {
		name    string
		spec    *configprotov1.HTTPFilterSpec
		wantErr string
	}{
		{"valid header_mutation", spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_ROUTE, validPerRoute), ""},
		{"valid header_to_metadata", spec("envoy.filters.http.header_to_metadata", configprotov1.HTTPFilterSpec_SCOPE_UNSPECIFIED, h2mCfg), ""},
		{"nil spec", nil, "spec is required"},
		{"empty filter", spec("", configprotov1.HTTPFilterSpec_SCOPE_ROUTE, h2mCfg), "spec.filter is required"},
		{"not allow-listed", spec("envoy.filters.http.lua", configprotov1.HTTPFilterSpec_SCOPE_ROUTE, h2mCfg), "not a supported extension filter"},
		{"chain scope", spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_CHAIN, validPerRoute), "CHAIN is not yet supported"},
		{"nil typedConfig", spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_ROUTE, nil), "spec.typedConfig is required"},
		{"unknown @type", spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_ROUTE, &anypb.Any{TypeUrl: "type.googleapis.com/does.not.Exist"}), "unknown or malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSpec(tc.spec)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
