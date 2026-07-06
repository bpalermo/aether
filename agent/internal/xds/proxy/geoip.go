package proxy

import (
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	geoip_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/geoip/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	geoip_commonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/geoip_providers/common/v3"
	geoip_maxmindv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/geoip_providers/maxmind/v3"

	"github.com/bpalermo/aether/agent/internal/xds/config"
)

// Geo header names — the aether-RESERVED x-geo-* namespace (proposal 028). The edge
// strips these from every request BEFORE the geoip filter runs: Envoy's geoip filter
// only OVERWRITES a header on a successful lookup — on a miss (unknown/private IP,
// driver failure) a client-supplied value would pass through into the mesh.
const (
	GeoHeaderCountry = "x-geo-country"
	GeoHeaderCity    = "x-geo-city"
	GeoHeaderASN     = "x-geo-asn"
	GeoHeaderAnon    = "x-geo-anon"
)

// geoipFilterName is Envoy's geoip HTTP filter.
const geoipFilterName = "envoy.filters.http.geoip"

// geoStripFilterName names the reserved-namespace strip entry distinctly from any
// user header_mutation extension entries.
const geoStripFilterName = "envoy.filters.http.header_mutation.geo_strip"

// GeoStripHTTPFilter removes every reserved x-geo-* header from incoming requests.
// Emitted on the edge routing chains UNCONDITIONALLY (geoip enabled or not) so an
// internet client can never inject geolocation the mesh would trust.
func GeoStripHTTPFilter() *http_connection_managerv3.HttpFilter {
	remove := []string{GeoHeaderCountry, GeoHeaderCity, GeoHeaderASN, GeoHeaderAnon}
	mutations := make([]*mutation_rulesv3.HeaderMutation, 0, len(remove))
	for _, h := range remove {
		mutations = append(mutations, &mutation_rulesv3.HeaderMutation{
			Action: &mutation_rulesv3.HeaderMutation_Remove{Remove: h},
		})
	}
	cfg := &header_mutationv3.HeaderMutation{
		Mutations: &header_mutationv3.Mutations{RequestMutations: mutations},
	}
	f := httpFilter(geoStripFilterName, cfg)
	// The canonical filter type is header_mutation; the distinct name above only
	// disambiguates stats/config. Envoy resolves the filter by typed_config type.
	return f
}

// GeoipConfig is the edge geoip configuration (proposal 028).
type GeoipConfig struct {
	// CityDBPath is the MaxMind city-type mmdb (serves country + city).
	CityDBPath string
	// Headers selects which x-geo-* headers to emit ("country", "city").
	Headers []string
	// XffNumTrustedHops mirrors the HCM value (edge.xffNumTrustedHops): when >0 the
	// client address is derived from X-Forwarded-For.
	XffNumTrustedHops uint32
}

// GeoipHTTPFilter builds the geoip filter with the MaxMind provider. The header→
// field mapping only includes the requested headers; the strip entry has already
// removed all reserved names, so an un-emitted header is simply absent.
func GeoipHTTPFilter(gc GeoipConfig) *http_connection_managerv3.HttpFilter {
	headers := &geoip_commonv3.CommonGeoipProviderConfig_GeolocationHeadersToAdd{}
	for _, h := range gc.Headers {
		switch h {
		case "country":
			headers.Country = GeoHeaderCountry
		case "city":
			headers.City = GeoHeaderCity
		}
	}
	provider := &geoip_maxmindv3.MaxMindConfig{
		CityDbPath:           gc.CityDBPath,
		CommonProviderConfig: &geoip_commonv3.CommonGeoipProviderConfig{GeoHeadersToAdd: headers},
	}
	cfg := &geoip_filterv3.Geoip{
		Provider: &corev3.TypedExtensionConfig{
			Name:        "envoy.geoip_providers.maxmind",
			TypedConfig: config.TypedConfig(provider),
		},
	}
	if gc.XffNumTrustedHops > 0 {
		cfg.XffConfig = &geoip_filterv3.Geoip_XffConfig{XffNumTrustedHops: gc.XffNumTrustedHops}
	}
	return httpFilter(geoipFilterName, cfg)
}
