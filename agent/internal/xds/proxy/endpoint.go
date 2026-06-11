package proxy

import (
	"sort"

	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
)

const (
	// envoyFilterMetadataSubsetNamespace is the Envoy metadata namespace for load balancing subsets
	envoyFilterMetadataSubsetNamespace = "envoy.lb"

	// subsetClusterKey is the metadata key for the cluster name
	subsetClusterKey = "cluster"
	// subsetIPKey is the metadata key for the endpoint IP address
	subsetIPKey = "ip"
	// subsetPodNamespaceKey is the metadata key for the pod namespace
	subsetPodNamespaceKey = "namespace"
	// subsetPodNameKey is the metadata key for the pod name
	subsetPodNameKey = "pod"
)

// NewClusterLoadAssignment creates an empty cluster load assignment for a service.
// Endpoints should be added to the Endpoints field.
func NewClusterLoadAssignment(serviceName string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: serviceName,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{},
	}
}

// SortLocalityLbEndpoints orders a load assignment's endpoints by their first
// endpoint's address. Endpoint order is part of the EDS resource's bytes, which
// the delta-xDS cache hashes to decide whether the resource changed — callers
// that rebuild assignments from maps (or from registry listings with unstable
// order) must sort so an unchanged endpoint set never hashes as changed.
func SortLocalityLbEndpoints(endpoints []*endpointv3.LocalityLbEndpoints) {
	sort.Slice(endpoints, func(i, j int) bool {
		return localityLbEndpointsKey(endpoints[i]) < localityLbEndpointsKey(endpoints[j])
	})
}

// localityLbEndpointsKey returns a stable ordering key for a LocalityLbEndpoints
// (the generators here emit one LbEndpoint per entry, keyed by its address).
func localityLbEndpointsKey(lle *endpointv3.LocalityLbEndpoints) string {
	if len(lle.GetLbEndpoints()) == 0 {
		return ""
	}
	addr := lle.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	return addr.GetAddress()
}
