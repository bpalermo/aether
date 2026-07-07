package v1

import (
	"encoding/json"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// edgeConfigShim is the wire shape of an EdgeConfig: standard Kubernetes
// TypeMeta/ObjectMeta/Status with the spec held as raw JSON so it is (un)marshalled
// through protojson (which honours proto JSON names, the google.protobuf.Any
// `@type` envelope of spec.typedConfig, and well-known types) rather than
// encoding/json.
type edgeConfigShim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              json.RawMessage  `json:"spec,omitempty"`
	Status            EdgeConfigStatus `json:"status,omitempty"`
}

// MarshalJSON serializes the EdgeConfig, encoding `.spec` with protojson.
func (in *EdgeConfig) MarshalJSON() ([]byte, error) {
	shim := edgeConfigShim{
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

// UnmarshalJSON parses an EdgeConfig, decoding `.spec` with protojson.
//
// Decoding is LENIENT (DiscardUnknown) for forward-compatibility across rolling
// upgrades — same contract as MeshConfig. The EdgeConfigSpec decodes by proto JSON names;
// its `@type`; an unknown/garbage `@type` surfaces here (and is rejected cleanly by
// the admission webhook). Semantic validation of the payload is proto-validate's job.
func (in *EdgeConfig) UnmarshalJSON(data []byte) error {
	var shim edgeConfigShim
	if err := json.Unmarshal(data, &shim); err != nil {
		return err
	}
	in.TypeMeta = shim.TypeMeta
	in.ObjectMeta = shim.ObjectMeta
	in.Status = shim.Status
	in.Spec = nil
	if len(shim.Spec) > 0 && string(shim.Spec) != "null" {
		spec := &configv1.EdgeConfigSpec{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(shim.Spec, spec); err != nil {
			return err
		}
		in.Spec = spec
	}
	return nil
}
