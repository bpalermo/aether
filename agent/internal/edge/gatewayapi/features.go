package gatewayapi

import (
	"slices"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"
)

// aetherSupportedFeaturesList is the honest set of Gateway API conformance
// features the aether edge implements (docs/conformance/gateway-api-features.md).
// The upstream conformance suite reads GatewayClass.status.supportedFeatures to
// decide which suites to run vs skip, so advertising ONLY what we implement makes
// unsupported tests (mirror, query/header-match flags) skip cleanly instead of
// failing. The named feature constants come from the pinned gateway-api module
// (sigs.k8s.io/gateway-api/pkg/features).
//
//   - Gateway / GatewayPort8080 — north-south Gateways on the standard test port.
//
//   - HTTPRoute — the edge (parentRef=Gateway) and GAMMA (parentRef=Service) route
//     type. NOTE: GRPCRoute is deliberately NOT advertised on the GatewayClass:
//     aether serves GRPCRoute only east-west via GAMMA (parentRef=Service), which is
//     a mesh feature with no GatewayClass, so the north-south Gateway does not serve
//     GRPCRoute. Advertising SupportGRPCRoute here makes the GATEWAY-GRPC suite send
//     gRPC traffic through a Gateway the edge cannot serve (run→fail/timeout).
//
//   - HTTPRouteMethodMatching — gRPC method match → path; HTTP method match.
//
//   - HTTPRouteResponseHeaderModification — ResponseHeaderModifier filter (route level).
//
//   - HTTPRouteRequestTimeout — HTTPRoute timeouts.request.
//
//   - HTTPRoutePortRedirect / HTTPRouteSchemeRedirect / HTTPRoutePathRedirect —
//     the RequestRedirect filter (port/scheme/path redirects; 301/302 status codes),
//     implemented on edge + GAMMA HTTPRoute (proxy.gammaRedirectAction). Host redirect
//     carries no separate feature flag (host+status is core). The 303/307/308 redirect
//     status-code features are NOT advertised — only 301 (MOVED_PERMANENTLY) and 302
//     (FOUND) are implemented.
//
//   - HTTPRouteHostRewrite / HTTPRoutePathRewrite — the URLRewrite filter (host +
//     path rewrite), implemented on edge + GAMMA HTTPRoute (proxy.applyURLRewrite).
//
//   - ReferenceGrant — cross-namespace backendRef admission + status enforcement
//     (ungranted cross-ns refs report ResolvedRefs=False/RefNotPermitted and are
//     dropped). Resolution stays namespace-blind today (proposal 020 Part 1).
//
// Path match (Exact/PathPrefix), header match (Exact), weighted backends, and the
// RequestHeaderModifier filter are part of HTTPRoute *core* conformance and carry
// no separate feature flag. Request mirroring is deliberately omitted (not
// implemented) so its suite skips. Query-param / header-match feature flags are
// intentionally not advertised here yet — that matching lands in a separate change.
var aetherSupportedFeaturesList = []features.FeatureName{
	features.SupportGateway,
	features.SupportGatewayPort8080,
	features.SupportHTTPRoute,
	features.SupportHTTPRouteHostRewrite,
	features.SupportHTTPRouteMethodMatching,
	features.SupportHTTPRoutePathRedirect,
	features.SupportHTTPRoutePathRewrite,
	features.SupportHTTPRoutePortRedirect,
	features.SupportHTTPRouteRequestTimeout,
	features.SupportHTTPRouteResponseHeaderModification,
	features.SupportHTTPRouteSchemeRedirect,
	features.SupportReferenceGrant,
}

// aetherSupportedFeatures returns the supportedFeatures to publish on the
// GatewayClass status, sorted ascending by name (the API requires the list be
// sorted in ascending alphabetical order by the Name key).
func aetherSupportedFeatures() []gatewayv1.SupportedFeature {
	names := make([]string, 0, len(aetherSupportedFeaturesList))
	for _, f := range aetherSupportedFeaturesList {
		names = append(names, string(f))
	}
	slices.Sort(names)
	out := make([]gatewayv1.SupportedFeature, 0, len(names))
	for _, n := range names {
		out = append(out, gatewayv1.SupportedFeature{Name: gatewayv1.FeatureName(n)})
	}
	return out
}

// supportedFeaturesEqual reports whether two supportedFeatures lists carry the
// same set of feature names (used to avoid no-op status writes / reconcile loops).
func supportedFeaturesEqual(a, b []gatewayv1.SupportedFeature) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}
