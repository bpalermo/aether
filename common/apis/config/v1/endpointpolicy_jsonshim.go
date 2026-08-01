package v1

import (
	"encoding/json"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// endpointPolicyShim is the wire shape of an EndpointPolicy: standard Kubernetes
// TypeMeta/ObjectMeta/Status with the spec held as raw JSON so it is (un)marshalled
// through protojson (which honours proto JSON names — targetRef, udsSocket) rather
// than encoding/json.
type endpointPolicyShim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              json.RawMessage      `json:"spec,omitempty"`
	Status            EndpointPolicyStatus `json:"status,omitempty"`
}

// MarshalJSON serializes the EndpointPolicy, encoding `.spec` with protojson.
func (in *EndpointPolicy) MarshalJSON() ([]byte, error) {
	shim := endpointPolicyShim{
		TypeMeta:   in.TypeMeta,
		ObjectMeta: in.ObjectMeta,
		Status:     in.Status,
	}
	if in.Spec != nil {
		raw, err := protojson.Marshal(in.Spec)
		if err != nil {
			return nil, err
		}
		shim.Spec = raw
	}
	return json.Marshal(shim)
}

// UnmarshalJSON parses an EndpointPolicy, decoding `.spec` with protojson.
//
// Decoding is LENIENT (DiscardUnknown) for forward-compatibility across rolling
// upgrades — the same contract as MeshConfig and HTTPFilter. Semantic validation
// (segment rules, the sun_path budget, the targetRef shape) is the admission
// webhook's job.
func (in *EndpointPolicy) UnmarshalJSON(data []byte) error {
	var shim endpointPolicyShim
	if err := json.Unmarshal(data, &shim); err != nil {
		return err
	}
	in.TypeMeta = shim.TypeMeta
	in.ObjectMeta = shim.ObjectMeta
	in.Status = shim.Status
	in.Spec = nil
	if len(shim.Spec) > 0 && string(shim.Spec) != "null" {
		spec := &configv1.EndpointPolicySpec{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(shim.Spec, spec); err != nil {
			return err
		}
		in.Spec = spec
	}
	return nil
}
