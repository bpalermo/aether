package referencegrant

import (
	"fmt"

	"aethermesh.dev/common/serviceref"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// BackendKey resolves a backendRef to its namespace-qualified "<ns>/<svc>"
// registry key (020 Part 1): the backendRef's own namespace when set, else the
// route's namespace.
func BackendKey(backendNamespace *gatewayv1.Namespace, routeNamespace, name string) string {
	ns := routeNamespace
	if bn := BackendNamespace(backendNamespace); bn != "" {
		ns = bn
	}
	return serviceref.New(ns, name).Key()
}

// BackendPermitted reports whether a backendRef is allowed onto the data plane: a
// same-namespace ref always is; a cross-namespace ref needs a matching ReferenceGrant
// in the backend's namespace whose from matches the route and whose to allows the
// Service.
func BackendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := BackendNamespace(backendNamespace)
	if !CrossNamespace(ns, routeNamespace) {
		return true
	}
	return PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
}

// BackendNamespace returns the backendRef namespace ("" when unset).
func BackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}

// ResolveBackends reports whether every backendRef is resolvable, as the
// (resolved, reason, message) triple a route's ResolvedRefs condition carries.
// aether resolves backends by NAME via the registry (namespace-free), so a valid
// core Service-kind ref with a non-empty name is resolved here; a genuinely-absent
// backend surfaces at runtime as no endpoints / 503, not a static ResolvedRefs
// failure. A k8s Service Get would be a false negative (the registry, not a k8s
// Service, backs the route). Only the ref *shape* — and, for cross-namespace refs,
// ReferenceGrant permission — is validated. routeKind is the referring route's kind
// (HTTPRoute/GRPCRoute/TCPRoute/TLSRoute/UDPRoute), used to match a grant's
// spec.from.kind. Shared by the node agent's gamma and l4route reconcilers.
func ResolveBackends(routeNamespace, routeKind string, refs []gatewayv1.BackendObjectReference, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
	for _, ref := range refs {
		if (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			return false, string(gatewayv1.RouteReasonInvalidKind), fmt.Sprintf("backendRef %q is not a core Service", ref.Name)
		}
		if string(ref.Name) == "" {
			return false, string(gatewayv1.RouteReasonBackendNotFound), "backendRef has an empty name"
		}
		if ns := BackendNamespace(ref.Namespace); CrossNamespace(ns, routeNamespace) &&
			!PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, string(ref.Name)) {
			return false, string(gatewayv1.RouteReasonRefNotPermitted),
				fmt.Sprintf("cross-namespace backendRef to Service %q in namespace %q is not permitted by any ReferenceGrant", ref.Name, ns)
		}
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"
}
