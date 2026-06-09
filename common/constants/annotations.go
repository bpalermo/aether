// Package constants defines shared constants used throughout the Aether codebase.
// It includes Kubernetes labels, annotations, and configuration constants for the service mesh.
package constants

const (
	// Aether annotations
	annotationAetherPrefix         = "aether.io/"
	annotationAetherEndpointPrefix = "endpoint." + annotationAetherPrefix

	// AnnotationSpiffeID is the pod annotation key for specifying the workload's SPIFFE ID.
	// When set, this is used as the SDS secret name for the workload's TLS certificate.
	AnnotationSpiffeID = annotationAetherPrefix + "spiffe-id"

	// AnnotationEndpointPort is the pod annotation key for specifying the service port
	AnnotationEndpointPort = annotationAetherEndpointPrefix + "port"
	// AnnotationEndpointWeight is the pod annotation key for specifying endpoint weight in load balancing
	AnnotationEndpointWeight = annotationAetherEndpointPrefix + "weight"
	// AnnotationEndpointHealthPath is the pod annotation key for the HTTP path the
	// agent active-health-checks on the pod's application (delegated liveness).
	AnnotationEndpointHealthPath = annotationAetherEndpointPrefix + "health-path"

	// AnnotationEndpointHealthCheckMode is the pod annotation key selecting how
	// client proxies determine this endpoint's health. Unset or "active" (default)
	// makes each client actively health-check the endpoint's mesh readiness path;
	// "eds" makes clients rely on the EDS health status pushed by the node-local
	// agent (delegated liveness) instead of probing the endpoint.
	AnnotationEndpointHealthCheckMode = annotationAetherEndpointPrefix + "health-check-mode"
	// HealthCheckModeActive / HealthCheckModeEDS are the accepted annotation values.
	HealthCheckModeActive = "active"
	HealthCheckModeEDS    = "eds"

	// AnnotationAetherEndpointMetadataPrefix is the prefix for endpoint metadata annotations
	AnnotationAetherEndpointMetadataPrefix = "metadata." + annotationAetherEndpointPrefix

	// Kubernetes topology annotations
	// AnnotationKubernetesNodeTopologyRegion is the Kubernetes node label for the region
	AnnotationKubernetesNodeTopologyRegion = "topology.kubernetes.io/region"
	// AnnotationKubernetesNodeTopologyZone is the Kubernetes node label for the zone
	AnnotationKubernetesNodeTopologyZone = "topology.kubernetes.io/zone"
)
