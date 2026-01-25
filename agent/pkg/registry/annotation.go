package registry

import (
	"strconv"

	"github.com/bpalermo/aether/agent/pkg/constants"
)

const (
	defaultEndpointRegion        = "unknown"
	defaultEndpointZone          = "unknown"
	defaultEndpointPort   uint32 = 8080
	defaultEndpointWeight        = 1024
)

func GetServiceFromAnnotations(annotations map[string]string) string {
	if service, ok := annotations[constants.AetherServiceAnnotation]; ok {
		return service
	}
	return ""
}

func GetEndpointPortOrDefault(annotations map[string]string) uint32 {
	if portStr, ok := annotations[constants.AetherEndpointPortAnnotation]; ok {
		if port, err := strconv.ParseUint(portStr, 10, 32); err == nil {
			return uint32(port)
		}
	}
	return defaultEndpointPort
}

func GetEndpointWeightOrDefault(annotations map[string]string) uint32 {
	if weightStr, ok := annotations[constants.AetherEndpointWeightAnnotation]; ok {
		if weight, err := strconv.ParseUint(weightStr, 10, 32); err == nil {
			return uint32(weight)
		}
	}
	return defaultEndpointWeight
}

func GetEndpointRegionOrDefault(annotations map[string]string) string {
	if region, ok := annotations[constants.KubernetesTopologyRegionAnnotation]; ok {
		return region
	}
	return defaultEndpointRegion
}

func GetEndpointZoneOrDefault(annotations map[string]string) string {
	if zone, ok := annotations[constants.KubernetesTopologyZoneAnnotation]; ok {
		return zone
	}
	return defaultEndpointZone
}

func GetEndpointMetadata(annotations map[string]string) map[string]string {
	metadata := map[string]string{}
	prefix := constants.AetherEndpointMetadataAnnotationPrefix
	for key, value := range annotations {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			metadataKey := key[len(prefix):]
			metadata[metadataKey] = value
		}
	}
	return metadata
}
