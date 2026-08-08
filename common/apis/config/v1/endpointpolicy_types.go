package v1

import (
	configv1 "aethermesh.dev/api/aether/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EndpointPolicyKind is the CRD kind.
const EndpointPolicyKind = "EndpointPolicy"

// EndpointPolicy is a namespaced custom resource declaring how the node proxy
// delivers inbound traffic to a Service's pods (proposal 034 Phase 1b): today, the
// Unix socket to dial instead of TCP loopback. It attaches to a same-namespace
// Service with the Gateway API policy-attachment shape, like HTTPFilter.
//
// The pod annotation endpoint.aether.io/uds-socket takes precedence — this CR is
// the service-level default. Its `.spec` is the protobuf EndpointPolicySpec.
type EndpointPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the protobuf EndpointPolicySpec, serialized via protojson (see the jsonshim).
	Spec *configv1.EndpointPolicySpec `json:"spec,omitempty"`

	Status EndpointPolicyStatus `json:"status,omitempty"`
}

// EndpointPolicyStatus reports the last admission/validation result.
type EndpointPolicyStatus struct {
	// Conditions follows metav1.Condition; an "Accepted" condition carries the
	// admission verdict.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EndpointPolicyList is the list type for EndpointPolicy.
type EndpointPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EndpointPolicy `json:"items"`
}
