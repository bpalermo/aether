// Package l4project projects Gateway API L4 route backendRefs (TCPRoute, TLSRoute,
// UDPRoute) into the data-plane weighted-backend model. It is the single source of
// truth shared by the node agent's capture-path L4 reconciler (proposal 018 Phase 3b:
// TCP floor chains, SNI-routed TLS chains, udp_proxy) AND the edge gateway's Gateway
// API reconciler (proposals 003/018/021: per-port edge TCP listeners and TLS
// passthrough chains). Keeping one projector avoids drift between how the mesh and
// the edge admit, weight, and name the same backendRef.
//
// It is the L4 sibling of common/gammaproject, which owns the HTTPRoute/GRPCRoute
// (L7) projection into the registryv1.GammaRoute proto. L4 backends are not exported
// cross-cluster, so this package stays a plain Go model rather than a proto.
//
// Pure: no agent/edge internals. The caller supplies a ClusterNameFunc because the
// data-plane cluster naming ("tcp:" vs "udp:" prefixes) lives in the agent's Envoy
// resource builders.
package l4project

import (
	"aethermesh.dev/common/referencegrant"
	"aethermesh.dev/common/serviceref"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// Backend is one weighted backend of an L4 route.
type Backend struct {
	// Service is the namespace-qualified "<ns>/<svc>" serviceref key (020 Part 1,
	// for dependency-set tracking).
	Service string
	// Cluster is the resolved data-plane cluster name, as produced by the
	// ClusterNameFunc ("tcp:<svc>.<ns>.<meshDomain>" or "udp:<svc>.<ns>.<meshDomain>").
	Cluster string
	// Weight is the load-balancing weight. An UNSET backendRef weight defaults to 1;
	// an explicit 0 means DRAIN (no traffic) and is passed through unchanged — the
	// Envoy builders omit it from the weighted-cluster set, per Gateway API (#492).
	Weight uint32
}

// ClusterNameFunc resolves a backend's namespace-qualified "<ns>/<svc>" serviceref
// key into the data-plane cluster name the proxy should route to.
type ClusterNameFunc func(serviceKey string) string

// Backends converts a backendRef slice into the data-plane backend list.
//
// Admission (identical for the mesh capture path and the edge, and aligned with the
// L7 projector in common/gammaproject):
//   - a ref with a non-core group, or a kind other than Service, is skipped;
//   - a ref with an empty name is skipped;
//   - a cross-namespace ref without a matching ReferenceGrant is skipped
//     (RefNotPermitted: dropped from the data plane, mirroring the route's
//     ResolvedRefs status, which the reconcilers compute separately).
//
// Backends are NOT port-qualified: L4 routes reach a service's TCP/UDP cluster,
// whose EDS endpoints already carry the port (unlike the L7 path, where
// backendRef.port selects a per-port cluster).
//
// routeNamespace is the referring route's namespace and routeKind its kind
// (TCPRoute/TLSRoute/UDPRoute), both needed for the ReferenceGrant "from" match.
func Backends(
	refs []gatewayv1.BackendRef,
	routeNamespace, routeKind string,
	grants []gatewayv1beta1.ReferenceGrant,
	clusterName ClusterNameFunc,
) []Backend {
	backends := make([]Backend, 0, len(refs))
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		name := string(b.Name)
		if name == "" {
			continue
		}
		if !backendPermitted(b.Namespace, routeNamespace, routeKind, name, grants) {
			continue
		}
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		// 020 Part 1: the backend's data-plane cluster and dependency-set key are
		// namespace-qualified "<ns>/<svc>" (backendRef namespace if set, else the
		// route's). A split to a different backend service therefore resolves the
		// right registry cluster.
		key := backendServiceKey(b.Namespace, routeNamespace, name)
		backends = append(backends, Backend{
			Service: key,
			Cluster: clusterName(key),
			Weight:  weight,
		})
	}
	return backends
}

// backendServiceKey resolves a backendRef to its namespace-qualified "<ns>/<svc>"
// registry key (020 Part 1): the backendRef's own namespace when set, else the
// route's namespace.
func backendServiceKey(backendNamespace *gatewayv1.Namespace, routeNamespace, name string) string {
	ns := routeNamespace
	if bn := derefBackendNamespace(backendNamespace); bn != "" {
		ns = bn
	}
	return serviceref.New(ns, name).Key()
}

// backendPermitted reports whether a backendRef is allowed onto the data plane: a
// same-namespace ref always is; a cross-namespace ref needs a matching ReferenceGrant
// in the backend's namespace whose from matches the route and whose to allows the
// Service.
func backendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := derefBackendNamespace(backendNamespace)
	if !referencegrant.CrossNamespace(ns, routeNamespace) {
		return true
	}
	return referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
}

// derefBackendNamespace returns the backendRef namespace ("" when unset).
func derefBackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}
