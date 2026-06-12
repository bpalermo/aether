// Package constants defines shared constants used throughout the Aether codebase.
// It includes Kubernetes labels, annotations, and configuration constants for the service mesh.
package constants

const (
	// Aether annotations
	annotationAetherPrefix         = "aether.io/"
	annotationAetherEndpointPrefix = "endpoint." + annotationAetherPrefix
	// config.aether.io/* annotations describe what the pod CONSUMES (mesh
	// client configuration), distinct from endpoint.aether.io/* which states
	// endpoint registration facts (what the pod serves).
	annotationAetherConfigPrefix = "config." + annotationAetherPrefix

	// AnnotationConfigUpstreams is the pod annotation declaring the upstream
	// services the pod calls, as a comma-separated list of service names
	// (e.g. "svc-payments,svc-ledger"). The agent unions the annotations of
	// its local pods into the node dependency set and distributes
	// clusters/endpoints/routes only for that set (demand-scoped
	// distribution, proposal 004). Declared upstreams are warm before first
	// use; undeclared upstreams are loaded on demand (ODCDS) with one xDS
	// round-trip of first-request latency.
	AnnotationConfigUpstreams = annotationAetherConfigPrefix + "upstreams"

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
	// client proxies determine this endpoint's health. Unset or "eds" (default)
	// makes clients rely on the EDS health status pushed by the node-local agent
	// (delegated liveness): the endpoint registers UNHEALTHY, is promoted once
	// the local proxy sees the app pass its health check, and enters every
	// client pre-warmed (no per-client first-HC round). "active" opts the pod
	// back into per-client active health checking of its mesh readiness path.
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
