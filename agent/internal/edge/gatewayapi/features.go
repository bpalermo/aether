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
// unsupported tests (redirect/rewrite/mirror, ReferenceGrant) skip cleanly instead
// of failing. The named feature constants come from the pinned gateway-api module
// (sigs.k8s.io/gateway-api/pkg/features).
//
//   - Gateway / GatewayPort8080 — north-south Gateways on the standard test port.
//
//   - HTTPRoute / GRPCRoute — both route types (edge HTTPRoute, GAMMA HTTPRoute+GRPCRoute).
//
//   - HTTPRouteMethodMatching — gRPC method match → path; HTTP method match.
//
//   - HTTPRouteResponseHeaderModification — ResponseHeaderModifier filter (route level).
//
//   - HTTPRouteRequestTimeout — HTTPRoute timeouts.request.
//
//   - ReferenceGrant — cross-namespace backendRef admission + status enforcement
//     (ungranted cross-ns refs report ResolvedRefs=False/RefNotPermitted and are
//     dropped). Resolution stays namespace-blind today (proposal 020 Part 1).
//
// Path match (Exact/PathPrefix), header match (Exact), weighted backends, and the
// RequestHeaderModifier filter are part of HTTPRoute *core* conformance and carry
// no separate feature flag. Redirect/rewrite/mirror are deliberately omitted (not
// implemented) so their suites skip.
var aetherSupportedFeaturesList = []features.FeatureName{
	features.SupportGateway,
	features.SupportGatewayPort8080,
	features.SupportGRPCRoute,
	features.SupportHTTPRoute,
	features.SupportHTTPRouteMethodMatching,
	features.SupportHTTPRouteRequestTimeout,
	features.SupportHTTPRouteResponseHeaderModification,
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
