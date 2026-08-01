package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HTTPFilterKind is the CRD kind.
const HTTPFilterKind = "HTTPFilter"

// HTTPFilter is a namespaced custom resource carrying a proxy-extension escape-hatch
// filter (proposal 025): a named Envoy HTTP filter, configured opaquely, that an
// HTTPRoute/GRPCRoute rule references via a Gateway API `ExtensionRef`. Its `.spec`
// is the protobuf HTTPFilterSpec (opaque `typedConfig` Any). The aether-controller
// validates the body fail-closed (in-process proto-validate) before admission.
type HTTPFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the protobuf HTTPFilterSpec, serialized via protojson (see the jsonshim).
	Spec *configv1.HTTPFilterSpec `json:"spec,omitempty"`

	Status HTTPFilterStatus `json:"status,omitempty"`
}

// HTTPFilterStatus reports the last admission/validation result.
type HTTPFilterStatus struct {
	// Conditions follows metav1.Condition; the controller sets an "Accepted" condition
	// (false with a reason when proto-validate or the allow-list rejects the filter).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HTTPFilterList is the list type for HTTPFilter.
type HTTPFilterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HTTPFilter `json:"items"`
}
