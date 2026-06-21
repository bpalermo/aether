package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// VirtualHostKind is the CRD kind. VirtualHost shares the config.aether.io/v1
// GroupVersion with MeshConfig (see meshconfig_types.go).
const VirtualHostKind = "VirtualHost"

// init registers the VirtualHost types on the shared config.aether.io scheme
// builder alongside MeshConfig.
func init() {
	SchemeBuilder.Register(addVirtualHostTypes)
}

func addVirtualHostTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &VirtualHost{}, &VirtualHostList{})
	return nil
}

// VirtualHost is a namespaced custom resource mapping an external (north-south)
// FQDN (or wildcard) to an ordered list of path-matched routes to mesh services,
// served by the edge proxy. Its `.spec` is the protobuf VirtualHostSpec; the
// edge agent watches VirtualHosts and serves them as listener routes (and scopes
// its registrar watch to the referenced backend services). It supersedes the
// removed EdgeRoute. See docs/proposals/017_virtual-host-l7-routing.md.
type VirtualHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the protobuf VirtualHostSpec, serialized via protojson (see the
	// jsonshim). A nil spec is an inert virtual host (no hosts, no routes).
	Spec *configv1.VirtualHostSpec `json:"spec,omitempty"`

	Status VirtualHostStatus `json:"status,omitempty"`
}

// VirtualHostStatus reports the virtual host's acceptance by the edge.
type VirtualHostStatus struct {
	// Conditions follows the standard metav1.Condition convention.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VirtualHostList is the list type for VirtualHost.
type VirtualHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualHost `json:"items"`
}
