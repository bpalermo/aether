// Package export holds the cross-cluster ServiceExport value type shared by the
// registry interface and its backends. It is a leaf package (no registry
// imports) so a backend implementation under registry/internal/* can return it
// without an import cycle through registry/etcd.go.
package export

// ServiceExport is one cluster's declaration that a mesh service is consumable
// across the clusterset (Kubernetes MCS-API). It is the cross-cluster view a
// registrar projects into local ServiceImports + clusterset VIPs.
type ServiceExport struct {
	// Service is the mesh service name (the registry service key).
	Service string
	// Namespace is the exporting cluster's namespace for the service. The MCS
	// model keys imports by (namespace, name); a local ServiceImport materializes
	// into this namespace.
	Namespace string
	// Cluster is the origin cluster that exported the service (proposal 006
	// partition). Used for ServiceImport.status.clusters and conflict detection.
	Cluster string
}
