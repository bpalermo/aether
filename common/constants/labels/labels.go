// Package labels defines the cross-tree Aether Kubernetes labels and the
// mesh-service / clusterset label+annotation domain used to mark and link the
// generated selectorless Services the registrar owns.
package labels

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

	// AnnotationMeshService links a generated Service to the mesh service it
	// fronts. The agent reads it to map a captured ClusterIP (original-dst)
	// back to the registry-backed EDS cluster — endpoints stay in the registry,
	// the Service is a pure name/VIP/identity handle.
	AnnotationMeshService = aetherLabelPrefix + "/service"

	// AnnotationMeshPort ("aether.io/port") is a name the mesh no longer gives
	// a meaning to. Through chart 2.7.0 the registrar stamped it on every
	// generated mesh Service with the port the pods declare, for a chart 2.6
	// agent; a service's ports are the Service's own ports now. The mesh
	// Service generator does not write it and REMOVES it from the Services it
	// owns (registrar/internal/services, staleServiceAnnotations), which is
	// why the name is kept. The MCS import generator still writes it on the
	// clusterset VIP Service, with the mesh HTTP port; nothing reads it there.
	AnnotationMeshPort = aetherLabelPrefix + "/port"

	// LabelClustersetService marks a generated, selectorless clusterset.local
	// Service that fronts a cross-cluster mesh service as a ClusterSet VIP
	// (Kubernetes MCS-API, proposals 018 + 006). The registrar owns these — it
	// lists/prunes by this label, separately from the per-cluster mesh VIPs
	// (LabelMeshService) so the two projections never clobber each other.
	LabelClustersetService = aetherLabelPrefix + "/clusterset-service"

	// AnnotationClustersetImport links a generated clusterset VIP Service back to
	// the MCS ServiceImport (and the same-named mesh service) it provides the
	// ClusterSetIP for. The registrar reads it to copy the K8s-allocated ClusterIP
	// onto the ServiceImport's spec.IPs, so <svc>.<ns>.svc.clusterset.local
	// resolves to the clusterset VIP rather than the per-cluster mesh VIP.
	AnnotationClustersetImport = aetherLabelPrefix + "/clusterset-import"

	// ClustersetServiceNameSuffix is appended to the mesh service name to derive
	// the distinct name of the generated clusterset VIP Service. The clusterset VIP
	// is a SEPARATE k8s Service (its own ClusterIP) from the per-cluster mesh VIP
	// (the same-named transparent-capture Service), so the two coexist: the mesh
	// Service backs <svc>.<ns>.svc.cluster.local, the clusterset VIP backs the
	// ServiceImport's <svc>.<ns>.svc.clusterset.local address.
	ClustersetServiceNameSuffix = "-clusterset"

	// LabelManagedServiceImport marks a ServiceImport materialized by the
	// registrar from the registry's clusterset-wide export view. The registrar
	// owns these — it lists/prunes ServiceImports by this label, never touching
	// a ServiceImport an operator or another controller authored.
	LabelManagedServiceImport = aetherLabelPrefix + "/managed-service-import"

	// AnnotationMeshAppProtocol ("aether.io/app-protocol") is a name the mesh
	// no longer gives a meaning to. Through chart 2.7.0 the registrar stamped
	// it on every generated mesh Service ("http", "tcp" or "udp": ONE protocol
	// for the whole service), and a chart 2.6 agent read it to choose between
	// an HTTP and a raw-TCP capture chain. A port's protocol is the
	// ServicePort's appProtocol and the registry's port_protocols; a service
	// has none. Nothing writes or reads the annotation; the mesh Service
	// generator REMOVES it from the Services it owns, which is why the name is
	// kept.
	AnnotationMeshAppProtocol = aetherLabelPrefix + "/app-protocol"
)
