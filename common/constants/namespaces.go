package constants

import "strings"

// IgnoredNamespaces are namespaces whose pods the service mesh never intercepts:
// the Kubernetes control plane, Aether's own components, and SPIRE (whose pods
// must not depend on the mesh/CNI to come up — otherwise the mesh and SPIRE
// deadlock each other).
var IgnoredNamespaces = map[string]struct{}{
	"kube-system":   {},
	"aether-system": {},
	"spire-mgmt":    {},
	"spire-server":  {},
	"spire-system":  {},
}

// IsIgnoredNamespace reports whether namespace is mesh-ignored (case-insensitive).
func IsIgnoredNamespace(namespace string) bool {
	_, ok := IgnoredNamespaces[strings.ToLower(namespace)]
	return ok
}
