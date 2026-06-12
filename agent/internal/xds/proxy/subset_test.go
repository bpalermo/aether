package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"

	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidSubsetKey pins key hygiene: keys become HTTP header suffixes and
// envoy.lb metadata keys.
func TestValidSubsetKey(t *testing.T) {
	for key, want := range map[string]bool{
		"version": true, "shard-id": true, "v2": true,
		"ip": false, "pod": false, "cluster": false, "namespace": false, // reserved
		"": false, "-x": false, "x-": false, "UPPER": false, "a.b": false, "a_b": false,
	} {
		assert.Equal(t, want, ValidSubsetKey(key), "key %q", key)
	}
}

// TestBuildSubsetHeadersExtension verifies the shared ECDS resource: fixed
// ip/pod pinning rules first, one x-aether-subset-<key> rule per provider
// key, all writing envoy.lb dynamic metadata.
func TestBuildSubsetHeadersExtension(t *testing.T) {
	ext := BuildSubsetHeadersExtension([]string{"shard", "version"})
	assert.Equal(t, SubsetHeadersFilterName, ext.GetName())

	cfg := &header_to_metadatav3.Config{}
	require.NoError(t, ext.GetTypedConfig().UnmarshalTo(cfg))
	rules := cfg.GetRequestRules()
	require.Len(t, rules, 4)
	assert.Equal(t, "x-aether-ip", rules[0].GetHeader())
	assert.Equal(t, "ip", rules[0].GetOnHeaderPresent().GetKey())
	assert.Equal(t, "x-aether-pod", rules[1].GetHeader())
	assert.Equal(t, "x-aether-subset-shard", rules[2].GetHeader())
	assert.Equal(t, "shard", rules[2].GetOnHeaderPresent().GetKey())
	assert.Equal(t, "x-aether-subset-version", rules[3].GetHeader())
	for _, r := range rules {
		assert.Equal(t, "envoy.lb", r.GetOnHeaderPresent().GetMetadataNamespace())
		assert.False(t, r.GetRemove(), "subset headers stay visible to the callee")
	}
}

// TestSubsetHeadersHttpFilter verifies the filter is ECDS-discovered with a
// non-blocking default config (ip/pod rules only).
func TestSubsetHeadersHttpFilter(t *testing.T) {
	f := subsetHeadersHttpFilter()
	assert.Equal(t, SubsetHeadersFilterName, f.GetName())
	cd := f.GetConfigDiscovery()
	require.NotNil(t, cd)
	assert.True(t, cd.GetApplyDefaultConfigWithoutWarming(), "listeners must never block on ECDS")
	require.NotNil(t, cd.GetConfigSource())
	assert.IsType(t, &corev3.ConfigSource_Ads{}, cd.GetConfigSource().GetConfigSourceSpecifier())
	def := &header_to_metadatav3.Config{}
	require.NoError(t, cd.GetDefaultConfig().UnmarshalTo(def))
	assert.Len(t, def.GetRequestRules(), 2, "default = ip/pod pinning only")
}
