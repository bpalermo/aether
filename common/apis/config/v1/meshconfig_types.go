// Package v1 holds the hand-written, typed MeshConfig Kubernetes CRD object whose
// `.spec` is the protobuf aether.config.v1.MeshConfigSpec. JSON serialization
// (jsonshim) and DeepCopy are hand-written here too, rather than reading the CR as
// unstructured. The proto lives in //api (generated); this is the only
// hand-written API Go. See docs/proposals/015_mesh-config.md.
package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group/version of the MeshConfig CRD. The API is v1 from
// the start: proto field-number evolution gives backward/forward compatibility.
var GroupVersion = schema.GroupVersion{Group: "config.aether.io", Version: "v1"}

// MeshConfigKind is the CRD kind.
const MeshConfigKind = "MeshConfig"

// SchemeBuilder registers the MeshConfig types with a runtime.Scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the MeshConfig types to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	return SchemeBuilder.AddToScheme(s)
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &MeshConfig{}, &MeshConfigList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// MeshConfig is the cluster-scoped singleton custom resource carrying the proxy
// data-plane observability override. Its `.spec` is the protobuf MeshConfigSpec.
type MeshConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the protobuf MeshConfigSpec, serialized via protojson (see the
	// jsonshim). A nil spec means "inherit everything from the aether config".
	Spec *configv1.MeshConfigSpec `json:"spec,omitempty"`

	Status MeshConfigStatus `json:"status,omitempty"`
}

// MeshConfigStatus reports the last projection result.
type MeshConfigStatus struct {
	// Conditions follows the standard metav1.Condition convention; the controller
	// sets a "Projected" condition.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MeshConfigList is the list type for MeshConfig.
type MeshConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MeshConfig `json:"items"`
}
