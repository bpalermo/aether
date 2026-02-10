package constants

const (
	annotationAetherPrefix         = "aether.io/"
	annotationAetherEndpointPrefix = "endpoint." + annotationAetherPrefix

	AnnotationEndpointPort   = annotationAetherEndpointPrefix + "port"
	AnnotationEndpointWeight = annotationAetherEndpointPrefix + "weight"

	AnnotationAetherEndpointMetadataPrefix = "metadata." + annotationAetherEndpointPrefix
)
