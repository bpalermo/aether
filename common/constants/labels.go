package constants

const (
	aetherLabelPrefix = "aether.io"

	// LabelAetherManaged is the Kubernetes label used to opt pods into the Aether
	// service mesh. The value must be "true" for the pod to be managed.
	LabelAetherManaged = aetherLabelPrefix + "/managed"

	// TaintAgentNotReady is the startup taint that keeps workload pods off a node
	// until the aether agent's CNI is serving. The operator registers nodes with
	// it (kubelet --register-with-taints); the agent tolerates it and removes it
	// once its CNI socket is up, so a pod's CNI ADD can be handled (issue #261).
	TaintAgentNotReady = aetherLabelPrefix + "/agent-not-ready"

	// LabelMeshService marks a generated, selectorless k8s Service that fronts a
	// mesh service as a transparent-capture VIP/name handle (proposal 018, Phase
	// 3a). The registrar owns these — it lists/prunes by this label.
	LabelMeshService = aetherLabelPrefix + "/mesh-service"

	// AnnotationMeshService / AnnotationMeshPort link a generated Service to the
	// mesh service + app port it fronts. The agent reads them to map a captured
	// ClusterIP (original-dst) back to the registry-backed EDS cluster — endpoints
	// stay in the registry, the Service is a pure name/VIP/identity handle.
	AnnotationMeshService = aetherLabelPrefix + "/service"
	AnnotationMeshPort    = aetherLabelPrefix + "/port"

	// LabelClustersetService marks a generated, selectorless clusterset.local
	// Service that fronts a cross-cluster mesh service as a ClusterSet VIP
	// (Kubernetes MCS-API, proposals 018 + 006). The registrar owns these — it
	// lists/prunes by this label, separately from the per-cluster mesh VIPs
	// (LabelMeshService) so the two projections never clobber each other.
	LabelClustersetService = aetherLabelPrefix + "/clusterset-service"

	// LabelManagedServiceImport marks a ServiceImport materialized by the
	// registrar from the registry's clusterset-wide export view. The registrar
	// owns these — it lists/prunes ServiceImports by this label, never touching
	// a ServiceImport an operator or another controller authored.
	LabelManagedServiceImport = aetherLabelPrefix + "/managed-service-import"

	// AnnotationMeshAppProtocol records the application-layer protocol for a
	// generated mesh Service (proposal 018, Phase 3a TCP floor). Values: "http"
	// (default), "grpc", "tcp". The capture reconciler reads it to decide whether
	// to emit a per-ClusterIP TCP-proxy filter chain (non-HTTP services) or leave
	// the global HCM chain to handle traffic to that VIP (HTTP/gRPC services).
	AnnotationMeshAppProtocol = aetherLabelPrefix + "/app-protocol"
)
