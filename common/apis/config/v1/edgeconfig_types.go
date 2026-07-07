package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EdgeConfigKind is the CRD kind.
const EdgeConfigKind = "EdgeConfig"

// EdgeConfig is a namespaced custom resource carrying the edge-hardening + HTTP/3
// settings for a set of edge Gateways (proposal 029). Resolved per edge instance via
// the native Gateway API parametersRef chain: GatewayClass.spec.parametersRef (fleet
// default) + Gateway.spec.infrastructure.parametersRef (per-instance override),
// merged with proto.Merge. Every field has a best-practice compiled default. Its
// `.spec` is the protobuf EdgeConfigSpec.
type EdgeConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the protobuf EdgeConfigSpec, serialized via protojson (see the jsonshim).
	Spec *configv1.EdgeConfigSpec `json:"spec,omitempty"`

	Status EdgeConfigStatus `json:"status,omitempty"`
}

// EdgeConfigStatus reports the last admission/validation result.
type EdgeConfigStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EdgeConfigList is the list type for EdgeConfig.
type EdgeConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeConfig `json:"items"`
}
