package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EdgeRouteKind is the CRD kind. EdgeRoute shares the config.aether.io/v1
// GroupVersion with MeshConfig (see meshconfig_types.go).
const EdgeRouteKind = "EdgeRoute"

// init registers the EdgeRoute types on the shared config.aether.io scheme
// builder alongside MeshConfig.
func init() {
	SchemeBuilder.Register(addEdgeRouteTypes)
}

func addEdgeRouteTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &EdgeRoute{}, &EdgeRouteList{})
	return nil
}

// EdgeRoute is a namespaced custom resource mapping external (north-south)
// hostnames to a mesh service for the edge proxy. Its `.spec` is the protobuf
// EdgeRouteSpec; the edge agent watches EdgeRoutes and serves them as listener
// routes (and scopes its registrar watch to the referenced services).
type EdgeRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the protobuf EdgeRouteSpec, serialized via protojson (see the
	// jsonshim). A nil spec is an inert route (no hosts, no service).
	Spec *configv1.EdgeRouteSpec `json:"spec,omitempty"`

	Status EdgeRouteStatus `json:"status,omitempty"`
}

// EdgeRouteStatus reports the route's acceptance by the edge.
type EdgeRouteStatus struct {
	// Conditions follows the standard metav1.Condition convention.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EdgeRouteList is the list type for EdgeRoute.
type EdgeRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeRoute `json:"items"`
}
