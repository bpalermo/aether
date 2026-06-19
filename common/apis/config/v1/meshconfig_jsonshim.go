package v1

import (
	"encoding/json"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// meshConfigShim is the wire shape of a MeshConfig: standard Kubernetes
// TypeMeta/ObjectMeta/Status with the spec held as raw JSON so it can be
// (un)marshalled through protojson rather than encoding/json (which would not
// honour proto JSON names, optional presence, or well-known types).
type meshConfigShim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              json.RawMessage  `json:"spec,omitempty"`
	Status            MeshConfigStatus `json:"status,omitempty"`
}

// MarshalJSON serializes the MeshConfig, encoding `.spec` with protojson.
func (in *MeshConfig) MarshalJSON() ([]byte, error) {
	shim := meshConfigShim{
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

// UnmarshalJSON parses a MeshConfig, decoding `.spec` with protojson.
//
// Decoding is LENIENT (DiscardUnknown): unknown spec fields are ignored, not
// rejected. This is the protobuf forward-compatibility contract and is required
// for safe rolling upgrades — an older controller (or agent) must not fail to
// decode a CR/ConfigMap that a newer version wrote with a field it doesn't know.
// Semantic validation of known fields is protovalidate's job.
func (in *MeshConfig) UnmarshalJSON(data []byte) error {
	var shim meshConfigShim
	if err := json.Unmarshal(data, &shim); err != nil {
		return err
	}
	in.TypeMeta = shim.TypeMeta
	in.ObjectMeta = shim.ObjectMeta
	in.Status = shim.Status
	in.Spec = nil
	if len(shim.Spec) > 0 && string(shim.Spec) != "null" {
		spec := &configv1.MeshConfigSpec{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(shim.Spec, spec); err != nil {
			return err
		}
		in.Spec = spec
	}
	return nil
}
