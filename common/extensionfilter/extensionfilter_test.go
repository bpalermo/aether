package extensionfilter

import (
	"testing"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
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
		{"chain scope without targetRef", spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_CHAIN, validPerRoute), "CHAIN (service-wide always-on) requires a targetRef"},
		{"chain scope with Service targetRef", withTarget(spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_CHAIN, validPerRoute), "echo"), ""},
		{"both authoring forms", withTyped(spec("envoy.filters.http.header_mutation", configprotov1.HTTPFilterSpec_SCOPE_ROUTE, validPerRoute)), "exactly one authoring form"},
		{"typed form valid", withTyped(&configprotov1.HTTPFilterSpec{}), ""},
		{"typed form empty rule", func() *configprotov1.HTTPFilterSpec {
			sp := &configprotov1.HTTPFilterSpec{}
			sp.SetHeaderToMetadata(configprotov1.HeaderToMetadata_builder{Rules: []*configprotov1.HeaderToMetadata_Rule{
				configprotov1.HeaderToMetadata_Rule_builder{Header: "x"}.Build(),
			}}.Build())
			return sp
		}(), "header and metadataKey are required"},
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

// withTarget adds a Service targetRef to a spec (builder helpers for the M4 cases).
func withTarget(sp *configprotov1.HTTPFilterSpec, svc string) *configprotov1.HTTPFilterSpec {
	sp.SetTargetRefs([]*configprotov1.PolicyTargetRef{
		configprotov1.PolicyTargetRef_builder{Kind: "Service", Name: svc}.Build(),
	})
	return sp
}

// withTyped sets the typed headerToMetadata authoring form.
func withTyped(sp *configprotov1.HTTPFilterSpec) *configprotov1.HTTPFilterSpec {
	sp.SetHeaderToMetadata(configprotov1.HeaderToMetadata_builder{Rules: []*configprotov1.HeaderToMetadata_Rule{
		configprotov1.HeaderToMetadata_Rule_builder{Header: "x-canary", MetadataKey: "canary"}.Build(),
	}}.Build())
	return sp
}

// TestRender covers the typed→Envoy render and the opaque passthrough.
func TestRender(t *testing.T) {
	// Typed form renders to header_to_metadata with envoy.lb default namespace.
	sp := withTyped(&configprotov1.HTTPFilterSpec{})
	name, cfg, err := Render(sp)
	require.NoError(t, err)
	assert.Equal(t, HeaderToMetadataFilterName, name)
	msg, err := cfg.UnmarshalNew()
	require.NoError(t, err)
	h2m, ok := msg.(*header_to_metadatav3.Config)
	require.True(t, ok)
	require.Len(t, h2m.GetRequestRules(), 1)
	assert.Equal(t, "x-canary", h2m.GetRequestRules()[0].GetHeader())
	assert.Equal(t, "canary", h2m.GetRequestRules()[0].GetOnHeaderPresent().GetKey())
	assert.Equal(t, "envoy.lb", h2m.GetRequestRules()[0].GetOnHeaderPresent().GetMetadataNamespace())

	// Opaque passthrough.
	op := &configprotov1.HTTPFilterSpec{}
	op.SetFilter("envoy.filters.http.header_mutation")
	a, _ := anypb.New(&header_to_metadatav3.Config{})
	op.SetTypedConfig(a)
	name2, cfg2, err := Render(op)
	require.NoError(t, err)
	assert.Equal(t, "envoy.filters.http.header_mutation", name2)
	assert.Same(t, a, cfg2)
}

// TestExtAuthzForm (027 M2): typed-only authoring, render both modes, opaque rejected.
func TestExtAuthzForm(t *testing.T) {
	withEA := func(ctx map[string]string, disabled bool) *configprotov1.HTTPFilterSpec {
		sp := &configprotov1.HTTPFilterSpec{}
		sp.SetExtAuthz(configprotov1.ExtAuthzRoute_builder{
			ContextExtensions: ctx, Disabled: disabled,
		}.Build())
		return sp
	}
	// Valid: enable with context extensions.
	require.NoError(t, ValidateSpec(withEA(map[string]string{"policy": "x"}, false)))
	// Valid: per-route exemption.
	require.NoError(t, ValidateSpec(withEA(nil, true)))
	// Invalid: disabled + context extensions.
	require.ErrorContains(t, ValidateSpec(withEA(map[string]string{"p": "x"}, true)), "mutually exclusive")
	// Invalid: opaque ext_authz.
	op := &configprotov1.HTTPFilterSpec{}
	op.SetFilter(ExtAuthzFilterName)
	a, _ := anypb.New(&header_to_metadatav3.Config{})
	op.SetTypedConfig(a)
	require.ErrorContains(t, ValidateSpec(op), "typed extAuthz form")

	// Render: check_settings mode.
	name, cfg, err := Render(withEA(map[string]string{"policy": "x"}, false))
	require.NoError(t, err)
	assert.Equal(t, ExtAuthzFilterName, name)
	msg, err := cfg.UnmarshalNew()
	require.NoError(t, err)
	pr := msg.(*ext_authzv3.ExtAuthzPerRoute)
	assert.Equal(t, "x", pr.GetCheckSettings().GetContextExtensions()["policy"])
	// Render: disabled mode.
	_, cfg2, err := Render(withEA(nil, true))
	require.NoError(t, err)
	msg2, _ := cfg2.UnmarshalNew()
	assert.True(t, msg2.(*ext_authzv3.ExtAuthzPerRoute).GetDisabled())

	// The union never emits a chain entry for ext_authz (system entry owns the chain).
	_, ok := DefaultConfig(ExtAuthzFilterName)
	assert.False(t, ok, "ext_authz must have no default chain config")
	assert.True(t, Allowed(ExtAuthzFilterName), "but TPFC emission is allowed")
}
