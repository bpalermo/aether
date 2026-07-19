package attachment

import (
	"github.com/bpalermo/aether/common/referencegrant"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// GatewayKey identifies a Gateway by namespace+name. Since the edge
// reconciles cluster-wide, Gateway names alone are no longer unique (two
// namespaces may each have a Gateway "edge"), so route attachment must match on
// the full namespaced name.
type GatewayKey struct {
	Namespace string
	Name      string
}

// GatewayListenerKey identifies a listener within a gateway by
// namespace+name+protocol+port. Used to scope TCPRoute/TLSRoute parentRef port
// matching.
type GatewayListenerKey struct {
	Gateway  GatewayKey
	Port     uint32
	Protocol gatewayv1.ProtocolType
}

// AttachedToOurGateway reports whether the given parentRefs (belonging to a route
// in routeNamespace) include a reference to one of our Gateways. A parentRef's
// namespace defaults to the route's own namespace when unset (per the Gateway API
// spec), so cross-namespace attachment is matched correctly.
func AttachedToOurGateway(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[GatewayKey]struct{}) bool {
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		key := GatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[key]; ok {
			return true
		}
	}
	return false
}

// AttachedGatewayKeys returns the "<ns>/<name>" keys of OUR Gateways a route's
// parentRefs attach to. Used to scope a vhost to its actual Gateways' route tables
// (Phase 2 assignment by attachment, not by cert).
func AttachedGatewayKeys(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[GatewayKey]struct{}) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		key := GatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[key]; !ok {
			continue
		}
		s := key.Namespace + "/" + key.Name
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		keys = append(keys, s)
	}
	return keys
}

// parentRefIsGateway reports whether a parentRef targets a Gateway (the default
// group/kind, or explicitly the Gateway API Gateway kind).
func parentRefIsGateway(p gatewayv1.ParentReference) bool {
	if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
		return false
	}
	if p.Kind != nil && string(*p.Kind) != "Gateway" {
		return false
	}
	return true
}

// parentRefNamespace resolves a parentRef namespace, defaulting to the referring
// route's namespace when the ref leaves it unset (Gateway API local-default rule).
func parentRefNamespace(ns *gatewayv1.Namespace, routeNamespace string) string {
	if ns != nil && *ns != "" {
		return string(*ns)
	}
	return routeNamespace
}

// GatewayParentPorts returns the distinct ports of our Gateway listeners that
// match the given protocol and are referenced by the given parentRefs (belonging
// to a route in routeNamespace). Used to scope TCPRoute/TLSRoute attachments to
// the exact Gateway listener ports. A parentRef namespace defaults to the route's
// own namespace when unset.
func GatewayParentPorts(
	parentRefs []gatewayv1.ParentReference,
	routeNamespace string,
	gateways map[GatewayKey]struct{},
	protocol gatewayv1.ProtocolType,
	listenerKeys map[GatewayListenerKey]struct{},
) []uint32 {
	seen := map[uint32]struct{}{}
	var ports []uint32
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		gk := GatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[gk]; !ok {
			continue
		}
		ports = collectParentRefPorts(p, gk, protocol, listenerKeys, seen, ports)
	}
	return ports
}

// collectParentRefPorts appends the matching ports for a single parentRef/gateway
// pair, deduplicating via seen.
func collectParentRefPorts(
	p gatewayv1.ParentReference,
	gk GatewayKey,
	protocol gatewayv1.ProtocolType,
	listenerKeys map[GatewayListenerKey]struct{},
	seen map[uint32]struct{},
	ports []uint32,
) []uint32 {
	if p.Port != nil {
		port := uint32(*p.Port)
		key := GatewayListenerKey{Gateway: gk, Port: port, Protocol: protocol}
		if _, ok := listenerKeys[key]; ok {
			if _, dup := seen[port]; !dup {
				seen[port] = struct{}{}
				ports = append(ports, port)
			}
		}
		return ports
	}
	// No port specified: include all matching-protocol listeners on this gateway.
	for k := range listenerKeys {
		if k.Gateway == gk && k.Protocol == protocol {
			if _, dup := seen[k.Port]; !dup {
				seen[k.Port] = struct{}{}
				ports = append(ports, k.Port)
			}
		}
	}
	return ports
}

// OurGatewayParentRefs returns the parentRefs of a route (in routeNamespace) that
// point at one of our Gateways — the only entries we own status for. The parentRef
// namespace defaults to the route's namespace.
func OurGatewayParentRefs(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[GatewayKey]struct{}) []gatewayv1.ParentReference {
	var out []gatewayv1.ParentReference
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		key := GatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[key]; !ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// firstBackendService returns the name of the first core-Service backendRef that is
// admissible: a same-namespace ref, or a cross-namespace ref permitted by a
// ReferenceGrant. Ungranted cross-namespace refs are skipped (RefNotPermitted →
// dropped from the data plane), so a rule whose only backend is ungranted yields no
// backend (and, with no redirect, no route).
func firstBackendService(refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) string {
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue // only core Services in Phase 1
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		if !BackendPermitted(b.Namespace, routeNamespace, "HTTPRoute", string(b.Name), grants) {
			continue
		}
		return string(b.Name)
	}
	return ""
}

// firstBackendPort returns the port of the first admissible (same-namespace or
// granted cross-namespace) backendRef, so the port matches the backend
// firstBackendService selects.
func firstBackendPort(refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) uint32 {
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		if !BackendPermitted(b.Namespace, routeNamespace, "HTTPRoute", string(b.Name), grants) {
			continue
		}
		if b.Port != nil {
			return uint32(*b.Port)
		}
	}
	return 0
}

// firstBackendNamespace returns the resolved namespace of the first admissible
// (same-namespace or granted cross-namespace) backendRef. Same-namespace refs
// default to the route's own namespace when backendRef.Namespace is unset.
// Matches the selection logic of firstBackendService so they return data for
// the same ref.
func firstBackendNamespace(refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) string {
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		if !BackendPermitted(b.Namespace, routeNamespace, "HTTPRoute", string(b.Name), grants) {
			continue
		}
		if b.Namespace != nil && string(*b.Namespace) != "" {
			return string(*b.Namespace)
		}
		return routeNamespace
	}
	return routeNamespace
}

// DerefBackendNamespace returns the backendRef namespace ("" when unset).
func DerefBackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}

// BackendPermitted reports whether a backendRef is allowed onto the data plane: a
// same-namespace ref always is; a cross-namespace ref needs a matching ReferenceGrant
// in the backend's namespace whose from matches the route and whose to allows the
// Service. routeKind is the referring route's kind (HTTPRoute/TCPRoute/TLSRoute).
func BackendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := DerefBackendNamespace(backendNamespace)
	if !referencegrant.CrossNamespace(ns, routeNamespace) {
		return true
	}
	return referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
}
