package referencegrant

import (
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
