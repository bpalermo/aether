package proxy

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

const h2mFilter = "envoy.filters.http.header_to_metadata"

func h2mConfig(t *testing.T) *header_to_metadatav3.Config {
	t.Helper()
	return &header_to_metadatav3.Config{
		RequestRules: []*header_to_metadatav3.Config_Rule{{
			Header: "x-tenant",
			OnHeaderPresent: &header_to_metadatav3.Config_KeyValuePair{
				MetadataNamespace: "aether",
				Key:               "tenant",
			},
		}},
	}
}

// TestExtensionFilterAllowed locks the escape-hatch allow-list: header_to_metadata is
// compiled into the aether proxy build (subset routing uses it), arbitrary filters are not.
func TestExtensionFilterAllowed(t *testing.T) {
	assert.True(t, ExtensionFilterAllowed(h2mFilter), "header_to_metadata is compiled in → allow-listed")
	assert.False(t, ExtensionFilterAllowed("envoy.filters.http.lua"), "lua is not in the proxy build → not allow-listed")
	assert.False(t, ExtensionFilterAllowed(""), "empty name is not allow-listed")
}

// TestExtensionHTTPFilter_DefaultDisabled verifies the HCM entry is present-but-disabled
// (so it's inert on non-referencing routes; per-route typed_per_filter_config re-enables it).
func TestExtensionHTTPFilter_DefaultDisabled(t *testing.T) {
	f := extensionHTTPFilter(h2mFilter, config.TypedConfig(&header_to_metadatav3.Config{}))
	assert.Equal(t, h2mFilter, f.GetName())
	assert.True(t, f.GetDisabled(), "escape-hatch filter must be default-disabled in the chain")
	require.NotNil(t, f.GetTypedConfig(), "disabled entry still needs a (neutral) typed_config")
}

// TestCollectExtensionFilters_UnionDedupAllowlist verifies the HCM union is deduped,
// allow-list-filtered, and nil when empty.
func TestCollectExtensionFilters_UnionDedupAllowlist(t *testing.T) {
	cfg := config.TypedConfig(h2mConfig(t))
	rules := []GammaRoute{
		{ExtensionFilters: []ExtensionFilter{{Name: h2mFilter, Config: cfg}}},
		{ExtensionFilters: []ExtensionFilter{
			{Name: h2mFilter, Config: cfg},                // dup → collapsed
			{Name: "envoy.filters.http.lua", Config: cfg}, // not allow-listed → skipped
		}},
		{}, // no extension filters
	}
	got := CollectExtensionFilters(rules)
	require.Len(t, got, 1, "deduped to the single allow-listed filter")
	assert.Equal(t, h2mFilter, got[0].GetName())
	assert.True(t, got[0].GetDisabled())

	assert.Nil(t, CollectExtensionFilters([]GammaRoute{{}, {}}), "no extension filters → nil (no chain additions)")
}

// TestExtensionPerFilterConfig verifies per-route emission: allow-listed + non-nil only,
// keyed by filter name; nil when nothing to attach.
func TestExtensionPerFilterConfig(t *testing.T) {
	cfg := config.TypedConfig(h2mConfig(t))
	got := extensionPerFilterConfig([]ExtensionFilter{
		{Name: h2mFilter, Config: cfg},
		{Name: "envoy.filters.http.lua", Config: cfg}, // not allow-listed → dropped
		{Name: h2mFilter, Config: nil},                // nil config → dropped
	})
	require.Len(t, got, 1)
	assert.Same(t, cfg, got[h2mFilter], "route carries the opaque per-route config verbatim")

	assert.Nil(t, extensionPerFilterConfig(nil), "no filters → nil typed_per_filter_config")
}

// TestBuildOutboundServiceVirtualHost_EmitsExtensionConfig verifies a GammaRoute with an
// ExtensionFilter produces a route whose typed_per_filter_config carries it.
func TestBuildOutboundServiceVirtualHost_EmitsExtensionConfig(t *testing.T) {
	cfg := config.TypedConfig(h2mConfig(t))
	vh := BuildOutboundServiceVirtualHost("echo.team-a.aether.internal", []string{"echo"}, []GammaRoute{{
		Matches:          []GammaMatch{{Prefix: "/"}},
		ExtensionFilters: []ExtensionFilter{{Name: h2mFilter, Config: cfg}},
	}})
	require.NotEmpty(t, vh.GetRoutes())
	// First route is the rule's; the trailing catch-all carries no per-filter config.
	assert.Same(t, cfg, vh.GetRoutes()[0].GetTypedPerFilterConfig()[h2mFilter])
	assert.Nil(t, vh.GetRoutes()[len(vh.GetRoutes())-1].GetTypedPerFilterConfig(), "catch-all has no extension config")
}

// TestApplyServiceChainFilter covers the vhost-level enablement (025 M4 CHAIN):
// the filter lands in vhost typed_per_filter_config; nil/non-allow-listed are no-ops.
func TestApplyServiceChainFilter(t *testing.T) {
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)

	vh := &routev3.VirtualHost{Name: "svc"}
	ApplyServiceChainFilter(vh, &ExtensionFilter{Name: "envoy.filters.http.header_mutation", Config: cfg})
	require.Contains(t, vh.GetTypedPerFilterConfig(), "envoy.filters.http.header_mutation")

	// Non-allow-listed → no-op.
	vh2 := &routev3.VirtualHost{Name: "svc2"}
	ApplyServiceChainFilter(vh2, &ExtensionFilter{Name: "envoy.filters.http.lua", Config: cfg})
	assert.Empty(t, vh2.GetTypedPerFilterConfig())
	// Nil filter → no-op.
	ApplyServiceChainFilter(vh2, nil)
	assert.Empty(t, vh2.GetTypedPerFilterConfig())
}

// TestCollectExtensionFilters_Extra covers the chain-filter union: extras (not
// referenced by any rule) still get their default-disabled HCM entries, deduped
// against rule-referenced filters.
func TestCollectExtensionFilters_Extra(t *testing.T) {
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	rules := []GammaRoute{{ExtensionFilters: []ExtensionFilter{{Name: "envoy.filters.http.header_mutation", Config: cfg}}}}
	out := CollectExtensionFilters(
		rules,
		ExtensionFilter{Name: "envoy.filters.http.header_mutation", Config: cfg},    // dup of rule's
		ExtensionFilter{Name: "envoy.filters.http.header_to_metadata", Config: cfg}, // new
	)
	require.Len(t, out, 2, "dedup across rules+extras; both allow-listed filters present")
}
