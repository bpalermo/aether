// Package serviceref defines the namespace-qualified identity of a mesh service
// (proposal 020 Part 1). A mesh service is identified by the pair
// (namespace, name) — where name is the workload's ServiceAccount — and this
// package is the single source of truth for how that identity is rendered as:
//
//   - a registry / map KEY:   "<namespace>/<name>"        (Key)
//   - the mesh FQDN:          "<name>.<namespace>.<meshDomain>"   (FQDN)
//   - the standard k8s FQDN:  "<name>.<namespace>.svc.cluster.local" (ClusterLocalFQDN)
//
// Centralizing these formats here keeps the namespace from being re-derived from
// a union or smuggled through bare strings — the exact trap proposal 020 calls
// out. It is a leaf package (no aether imports) so the registry, agent, CNI, and
// registrar can all share it without import cycles.
package serviceref

import "strings"

// ServiceRef is the namespace-qualified identity of a mesh service. Name is the
// workload's ServiceAccount (aether's identity unit); Namespace is the
// Kubernetes namespace the workload runs in.
type ServiceRef struct {
	Namespace string
	Name      string
}

// New returns a ServiceRef for the given namespace and name (ServiceAccount).
func New(namespace, name string) ServiceRef {
	return ServiceRef{Namespace: namespace, Name: name}
}

// Key returns the registry / map key "<namespace>/<name>". This is the
// namespace-qualified key that replaces the old namespace-free ServiceAccount
// key (proposal 020 Part 1, hard cutover).
func (r ServiceRef) Key() string {
	return r.Namespace + "/" + r.Name
}

// FQDN returns the mesh fully-qualified name "<name>.<namespace>.<meshDomain>" —
// the cluster name / SNI / route-target authority the proxy uses mesh-internally.
func (r ServiceRef) FQDN(meshDomain string) string {
	return r.Name + "." + r.Namespace + "." + meshDomain
}

// ClusterLocalFQDN returns the standard Kubernetes name
// "<name>.<namespace>.svc.cluster.local", which the transparent-capture path
// also honors so apps reaching a mesh service by its ordinary k8s DNS name are
// captured (proposal 020 Part 1).
func (r ServiceRef) ClusterLocalFQDN() string {
	return r.Name + "." + r.Namespace + ".svc.cluster.local"
}

// String returns the Key form, so a ServiceRef formats sensibly in logs.
func (r ServiceRef) String() string {
	return r.Key()
}

// ParseKey parses a "<namespace>/<name>" key back into a ServiceRef. ok is false
// when s is not a well-formed key (missing or empty namespace/name) — callers
// must treat a malformed key as a hard error, never as a namespace-free name.
func ParseKey(s string) (ref ServiceRef, ok bool) {
	ns, name, found := strings.Cut(s, "/")
	if !found || ns == "" || name == "" {
		return ServiceRef{}, false
	}
	// A second "/" would mean an ambiguous key (namespaces and SA names are DNS
	// labels and contain no "/"), so reject it rather than silently mis-split.
	if strings.Contains(name, "/") {
		return ServiceRef{}, false
	}
	return ServiceRef{Namespace: ns, Name: name}, true
}
