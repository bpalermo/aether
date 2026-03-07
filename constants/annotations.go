// Package constants defines shared constants used throughout the Aether codebase.
// It includes Kubernetes labels, annotations, and configuration constants for the service mesh.
package constants

const (
	// Aether annotations
	annotationAetherPrefix         = "aether.io/"
	annotationAetherEndpointPrefix = "endpoint." + annotationAetherPrefix

	// AnnotationEndpointPort is the pod annotation key for specifying the service port
	AnnotationEndpointPort = annotationAetherEndpointPrefix + "port"
	// AnnotationEndpointWeight is the pod annotation key for specifying endpoint weight in load balancing
	AnnotationEndpointWeight = annotationAetherEndpointPrefix + "weight"

	// AnnotationAetherEndpointMetadataPrefix is the prefix for endpoint metadata annotations
	AnnotationAetherEndpointMetadataPrefix = "metadata." + annotationAetherEndpointPrefix

	// Kubernetes topology annotations
	// AnnotationKubernetesNodeTopologyRegion is the Kubernetes node label for the region
	AnnotationKubernetesNodeTopologyRegion = "topology.kubernetes.io/region"
	// AnnotationKubernetesNodeTopologyZone is the Kubernetes node label for the zone
	AnnotationKubernetesNodeTopologyZone = "topology.kubernetes.io/zone"
)
