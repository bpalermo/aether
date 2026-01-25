package constants

const (
	aetherAnnotationPrefix         = "aether.io/"
	aetherEndpointAnnotationPrefix = "endpoint." + aetherAnnotationPrefix

	AetherEndpointMetadataAnnotationPrefix = "metadata." + aetherEndpointAnnotationPrefix

	AetherServiceAnnotation = aetherAnnotationPrefix + "service"

	AetherEndpointPortAnnotation   = aetherEndpointAnnotationPrefix + "port"
	AetherEndpointWeightAnnotation = aetherEndpointAnnotationPrefix + "weight"

	KubernetesTopologyRegionAnnotation = "topology.kubernetes.io/region"
	KubernetesTopologyZoneAnnotation   = "topology.kubernetes.io/zone"
)
