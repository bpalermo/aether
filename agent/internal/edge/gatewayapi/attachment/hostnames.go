package attachment

import (
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// BuildGatewayListenerHostnames extracts the listener hostnames for each Gateway
// into a map keyed by "<namespace>/<name>". A listener with no hostname is
// represented by an empty string "". The returned value is used to compute
// effective route hostnames via EffectiveHostnames.
//
// Keys: "ns/name" for the per-Gateway union (used by routes without a
// sectionName), and "ns/name/sectionName" for per-section lookup (used when
// a parentRef specifies a sectionName). The per-section value contains only
// that listener's hostname so EffectiveHostnames scopes the route to exactly
// the hostnames of the listener it attached to.
func BuildGatewayListenerHostnames(gws []gatewayv1.Gateway) map[string][]string {
	out := make(map[string][]string, len(gws))
	for _, gw := range gws {
		gwKey := gw.Namespace + "/" + gw.Name
		seen := make(map[string]struct{}, len(gw.Spec.Listeners))
		var hostnames []string
		for _, ln := range gw.Spec.Listeners {
			h := ""
			if ln.Hostname != nil {
				h = string(*ln.Hostname)
			}
			// Per-section key: "ns/name/sectionName" → exactly this listener's hostname.
			sectionKey := gwKey + "/" + string(ln.Name)
			out[sectionKey] = []string{h}
			// Per-gateway key: union of all listener hostnames (deduplicated).
			if _, ok := seen[h]; !ok {
				seen[h] = struct{}{}
				hostnames = append(hostnames, h)
			}
		}
		out[gwKey] = hostnames
	}
	return out
}

// AttachedHostnameLookupKeys returns the hostname-lookup keys for
// EffectiveHostnames. When a parentRef specifies a sectionName the key is
// "ns/name/sectionName" so EffectiveHostnames uses only that listener's
// hostname; otherwise it uses the gateway-level "ns/name" key (union of all
// listener hostnames). This implements Gateway API §hostname-intersection:
// "If the listener section name and/or port is specified, Hostnames must match
// only that listener."
func AttachedHostnameLookupKeys(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[GatewayKey]struct{}) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		gk := GatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[gk]; !ok {
			continue
		}
		gwKeyStr := gk.Namespace + "/" + gk.Name
		var lookupKey string
		if p.SectionName != nil && *p.SectionName != "" {
			lookupKey = gwKeyStr + "/" + string(*p.SectionName)
		} else {
			lookupKey = gwKeyStr
		}
		if _, dup := seen[lookupKey]; !dup {
			seen[lookupKey] = struct{}{}
			keys = append(keys, lookupKey)
		}
	}
	return keys
}

// EffectiveHostnames computes the Gateway API effective hostname set for a route:
//
//	effective = ∪(over attached Gateways) of { route.Hostnames ∩ listenerHostnames(gw) }
//
// Gateway API intersection rules (per spec):
//   - A listener hostname "" (no hostname) admits ALL route hostnames unchanged.
//   - A route hostname "" (no hostnames on the route) inherits the listener's
//     hostname(s); a listener "" × route "" = "*" (catch-all, no constraint).
//   - exact == exact: they match; result is that exact hostname.
//   - listener "*.example.com" ∩ route "a.example.com": route is more specific → result "a.example.com".
//   - route "*.example.com" ∩ listener "a.example.com": listener is more specific → result "a.example.com".
//
// The return value replaces vh.Hosts in the reconciler so downstream code (the
// cache's buildEdgeVhostsLocked) sees only the hosts the route ACTUALLY matches.
// A nil/empty return means the route has no valid attachment (no listener admits
// any of its declared hosts) and must be discarded by the caller.
//
// Multi-Gateway: takes the union of intersections across all attached Gateways,
// deduplicating. This is a correct first cut (see implementation note).
func EffectiveHostnames(routeHosts []string, gwKeys []string, gwListenerHostnames map[string][]string) []string {
	seen := make(map[string]struct{})
	var result []string
	add := func(h string) {
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			result = append(result, h)
		}
	}

	for _, gwKey := range gwKeys {
		lnHostnames, ok := gwListenerHostnames[gwKey]
		if !ok {
			continue
		}
		for _, lh := range lnHostnames {
			if catchAll := intersectListenerHostname(lh, routeHosts, add); catchAll {
				return nil
			}
		}
	}
	return result
}

// intersectListenerHostname merges one listener hostname into the effective set
// via add. Returns true when the combination is a true catch-all (both listener
// and route have no hostname), which signals the caller to return nil immediately.
func intersectListenerHostname(lh string, routeHosts []string, add func(string)) bool {
	if lh != "" {
		// Listener has a hostname (exact or wildcard).
		if len(routeHosts) == 0 {
			// Route has no hostname constraint: inherits the listener's hostname.
			add(lh)
			return false
		}
		// Intersect each route hostname against this listener hostname.
		for _, rh := range routeHosts {
			if h, ok := hostnameIntersect(lh, rh); ok {
				add(h)
			}
		}
		return false
	}
	// Listener has no hostname restriction: admit all route hostnames.
	if len(routeHosts) == 0 {
		// route "" + listener "" = "*" (true catch-all). Return nil immediately —
		// no specific hostname from any other listener can narrow a catch-all back
		// down. (HTTPRouteRedirectPortAndScheme, 443 subtests)
		return true
	}
	for _, rh := range routeHosts {
		add(rh)
	}
	return false
}

// hostnameIntersect returns the more-specific of two hostnames if they match,
// and reports whether they intersect at all. The Gateway API rules are:
//   - exact == exact → match, result = that hostname.
//   - "*.example.com" vs "a.example.com" → match (specific wins), result = "a.example.com".
//   - "*.example.com" vs "*.other.com" → no match.
//   - "*.example.com" vs "*.example.com" → match, result = "*.example.com".
//   - "" (no hostname = catch-all) matches everything — callers must handle the
//     empty-string case before calling this function.
func hostnameIntersect(a, b string) (string, bool) {
	if a == b {
		return a, true
	}
	// a is a wildcard, b is specific: a matches b if b ends with a[1:].
	if strings.HasPrefix(a, "*.") && !strings.HasPrefix(b, "*.") {
		if strings.HasSuffix(b, a[1:]) {
			return b, true // b is more specific
		}
		return "", false
	}
	// b is a wildcard, a is specific.
	if strings.HasPrefix(b, "*.") && !strings.HasPrefix(a, "*.") {
		if strings.HasSuffix(a, b[1:]) {
			return a, true // a is more specific
		}
		return "", false
	}
	// Both are exact but different, or both wildcards but different — no match.
	return "", false
}
