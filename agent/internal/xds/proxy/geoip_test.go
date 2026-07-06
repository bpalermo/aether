package proxy

import (
	"testing"

	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	geoip_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/geoip/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	geoip_maxmindv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/geoip_providers/maxmind/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGeoStripHTTPFilter (028): the strip removes EVERY reserved x-geo-* header —
// Envoy's geoip filter passes client-supplied values through on a lookup miss, so
// this must run before it, unconditionally.
func TestGeoStripHTTPFilter(t *testing.T) {
	f := GeoStripHTTPFilter()
	cfg := &header_mutationv3.HeaderMutation{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(cfg))
	var removed []string
	for _, m := range cfg.GetMutations().GetRequestMutations() {
		removed = append(removed, m.GetRemove())
	}
	assert.ElementsMatch(t, []string{GeoHeaderCountry, GeoHeaderCity, GeoHeaderASN, GeoHeaderAnon}, removed)
	// sanity: they render as remove actions, not appends
	for _, m := range cfg.GetMutations().GetRequestMutations() {
		_, isRemove := m.GetAction().(*mutation_rulesv3.HeaderMutation_Remove)
		assert.True(t, isRemove)
	}
}

// TestGeoipHTTPFilter (028): MaxMind provider with the requested header mapping and
// the filter-level XFF config mirroring the edge topology fact.
func TestGeoipHTTPFilter(t *testing.T) {
	f := GeoipHTTPFilter(GeoipConfig{
		CityDBPath:        "/etc/aether/geoip/GeoLite2-City.mmdb",
		Headers:           []string{"country", "city"},
		XffNumTrustedHops: 2,
	})
	cfg := &geoip_filterv3.Geoip{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(cfg))
	assert.Equal(t, uint32(2), cfg.GetXffConfig().GetXffNumTrustedHops())
	mm := &geoip_maxmindv3.MaxMindConfig{}
	require.NoError(t, cfg.GetProvider().GetTypedConfig().UnmarshalTo(mm))
	assert.Equal(t, "/etc/aether/geoip/GeoLite2-City.mmdb", mm.GetCityDbPath())
	h := mm.GetCommonProviderConfig().GetGeoHeadersToAdd()
	assert.Equal(t, GeoHeaderCountry, h.GetCountry())
	assert.Equal(t, GeoHeaderCity, h.GetCity())

	// No XFF hops → no XffConfig (immediate downstream address used).
	f2 := GeoipHTTPFilter(GeoipConfig{CityDBPath: "/db", Headers: []string{"country"}})
	cfg2 := &geoip_filterv3.Geoip{}
	require.NoError(t, f2.GetTypedConfig().UnmarshalTo(cfg2))
	assert.Nil(t, cfg2.GetXffConfig())
}

// TestBuildEdgeGatewayHTTPListener_Geo: the geo filters land after readiness, before
// the router, and HCM xff_num_trusted_hops is set in lockstep.
func TestBuildEdgeGatewayHTTPListener_Geo(t *testing.T) {
	filters := []*http_connection_managerv3.HttpFilter{
		GeoStripHTTPFilter(),
		GeoipHTTPFilter(GeoipConfig{CityDBPath: "/db", Headers: []string{"country"}, XffNumTrustedHops: 1}),
	}
	l := BuildEdgeGatewayHTTPListener("ns", "gw", 18150, false, filters, 1)
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	require.GreaterOrEqual(t, len(hcm.GetHttpFilters()), 4)
	names := make([]string, 0, len(hcm.GetHttpFilters()))
	for _, f := range hcm.GetHttpFilters() {
		names = append(names, f.GetName())
	}
	assert.Equal(t, geoStripFilterName, names[1], "strip immediately after readiness")
	assert.Equal(t, geoipFilterName, names[2], "geoip after the strip")
	assert.Equal(t, uint32(1), hcm.GetXffNumTrustedHops())
}
