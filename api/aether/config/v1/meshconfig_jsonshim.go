package configv1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/protobuf/encoding/protojson"
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

// UnmarshalJSON parses a MeshConfig, decoding `.spec` with protojson. Decoding is
// strict (unknown spec fields are rejected) so the schema is the proto exactly.
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
		spec := &MeshConfigSpec{}
		if err := protojson.Unmarshal(shim.Spec, spec); err != nil {
			return err
		}
		in.Spec = spec
	}
	return nil
}
