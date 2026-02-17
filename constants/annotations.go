package constants

const (
	// Aether
	annotationAetherPrefix         = "aether.io/"
	annotationAetherEndpointPrefix = "endpoint." + annotationAetherPrefix

	AnnotationEndpointPort   = annotationAetherEndpointPrefix + "port"
	AnnotationEndpointWeight = annotationAetherEndpointPrefix + "weight"

	AnnotationAetherEndpointMetadataPrefix = "metadata." + annotationAetherEndpointPrefix

	// KubernetesNodes

	AnnotationKubernetesNodeTopologyRegion = "topology.kubernetes.io/region"
	AnnotationKubernetesNodeTopologyZone   = "topology.kubernetes.io/zone"
)
