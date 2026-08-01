// Package referencegrant implements the Gateway API ReferenceGrant check used to
// admit cross-namespace backendRefs (conformance item: GATEWAY-HTTP core,
// cross-namespace route tests).
//
// A backendRef whose namespace is set and differs from the referring route's
// namespace is "cross-namespace". Per the Gateway API spec it is permitted ONLY
// when a ReferenceGrant exists IN THE BACKEND'S namespace whose spec.from includes
// {group: gateway.networking.k8s.io, kind: <route kind>, namespace: <route ns>} and
// whose spec.to includes {group: "", kind: Service} (optionally a specific name).
// Without a matching grant the reference MUST NOT be permitted.
//
// CAVEAT: aether resolves backends by NAME via the registry and is namespace-blind
// today (the data-plane cluster name is <svc>.<meshDomain>; proposal 020 Part 1 will
// make it <ns>/<svc>). So this is admission + status enforcement, NOT a change to the
// resolution path: an ungranted cross-namespace backendRef reports
// ResolvedRefs=False / RefNotPermitted and is DROPPED from the route; a granted one
// resolves exactly as today.
package referencegrant

import (
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// PermitsBackend reports whether the given set of ReferenceGrants (which the caller
// should list cluster-wide) permits a cross-namespace reference from a route of kind
// fromKind/fromGroup in fromNamespace to a Service named toName in toNamespace.
//
// A grant matches when it lives in toNamespace AND has a spec.from entry matching
// {fromGroup, fromKind, fromNamespace} AND a spec.to entry matching the core Service
// {group: "", kind: Service} whose name is empty (all Services) or equal to toName.
//
// Callers must only invoke this for genuinely cross-namespace refs; same-namespace
// refs are always permitted and need no grant (see CrossNamespace).
func PermitsBackend(
	grants []gatewayv1beta1.ReferenceGrant,
	fromGroup, fromKind, fromNamespace string,
	toNamespace, toName string,
) bool {
	for i := range grants {
		g := &grants[i]
		if g.Namespace != toNamespace {
			continue
		}
		if !fromMatches(g.Spec.From, fromGroup, fromKind, fromNamespace) {
			continue
		}
		if toMatches(g.Spec.To, toName) {
			return true
		}
	}
	return false
}

// PermitsSecret reports whether the grants permit a cross-namespace reference from
// a Gateway in fromNamespace to a core Secret named toName in toNamespace (the
// Gateway listener certificateRef case). A grant matches when it lives in
// toNamespace, has a spec.from entry matching {gateway.networking.k8s.io, Gateway,
// fromNamespace}, and a spec.to entry for the core Secret {group: "", kind: Secret}
// whose name is empty (all Secrets) or equal to toName.
//
// Callers must only invoke this for genuinely cross-namespace refs; same-namespace
// refs are always permitted and need no grant (see CrossNamespace).
func PermitsSecret(
	grants []gatewayv1beta1.ReferenceGrant,
	fromNamespace string,
	toNamespace, toName string,
) bool {
	for i := range grants {
		g := &grants[i]
		if g.Namespace != toNamespace {
			continue
		}
		if !fromMatches(g.Spec.From, "gateway.networking.k8s.io", "Gateway", fromNamespace) {
			continue
		}
		if toMatchesSecret(g.Spec.To, toName) {
			return true
		}
	}
	return false
}

// toMatchesSecret reports whether any spec.to entry allows the core Secret named
// toName. A to-entry with no Name matches every Secret; a named entry matches only
// that Secret.
func toMatchesSecret(to []gatewayv1.ReferenceGrantTo, toName string) bool {
	for _, t := range to {
		if string(t.Group) != "" || string(t.Kind) != "Secret" {
			continue
		}
		if t.Name == nil || string(*t.Name) == "" || string(*t.Name) == toName {
			return true
		}
	}
	return false
}

// fromMatches reports whether any spec.from entry trusts {group, kind, namespace}.
func fromMatches(from []gatewayv1.ReferenceGrantFrom, group, kind, namespace string) bool {
	for _, f := range from {
		if string(f.Group) == group && string(f.Kind) == kind && string(f.Namespace) == namespace {
			return true
		}
	}
	return false
}

// toMatches reports whether any spec.to entry allows the core Service named toName.
// A to-entry with no Name matches every Service of the kind; a named entry matches
// only that Service.
func toMatches(to []gatewayv1.ReferenceGrantTo, toName string) bool {
	for _, t := range to {
		// Only the core ("") group Service kind is a valid backend target here.
		if string(t.Group) != "" || string(t.Kind) != "Service" {
			continue
		}
		if t.Name == nil || string(*t.Name) == "" || string(*t.Name) == toName {
			return true
		}
	}
	return false
}

// CrossNamespace reports whether a backendRef namespace (the dereferenced
// backendRef.Namespace, "" when unset) makes the reference cross-namespace relative
// to the referring route's namespace. An unset or equal namespace is same-namespace
// (always permitted, no grant required).
func CrossNamespace(backendNamespace, routeNamespace string) bool {
	return backendNamespace != "" && backendNamespace != routeNamespace
}
