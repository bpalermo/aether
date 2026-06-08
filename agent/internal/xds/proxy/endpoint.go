package proxy

import (
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
