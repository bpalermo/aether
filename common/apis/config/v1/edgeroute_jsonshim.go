package v1

import (
	"encoding/json"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// edgeRouteShim is the wire shape of an EdgeRoute: standard Kubernetes
// TypeMeta/ObjectMeta/Status with the spec held as raw JSON so it round-trips
// through protojson (proto JSON names, presence, well-known types) rather than
// encoding/json. Mirrors meshConfigShim.
type edgeRouteShim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              json.RawMessage `json:"spec,omitempty"`
	Status            EdgeRouteStatus `json:"status,omitempty"`
}

// MarshalJSON serializes the EdgeRoute, encoding `.spec` with protojson.
func (in *EdgeRoute) MarshalJSON() ([]byte, error) {
	shim := edgeRouteShim{
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

// UnmarshalJSON parses an EdgeRoute, decoding `.spec` with protojson.
//
// Decoding is LENIENT (DiscardUnknown): unknown spec fields are ignored for
// rolling-upgrade forward-compatibility. Semantic validation is protovalidate's
// job. Mirrors MeshConfig.UnmarshalJSON.
func (in *EdgeRoute) UnmarshalJSON(data []byte) error {
	var shim edgeRouteShim
	if err := json.Unmarshal(data, &shim); err != nil {
		return err
	}
	in.TypeMeta = shim.TypeMeta
	in.ObjectMeta = shim.ObjectMeta
	in.Status = shim.Status
	in.Spec = nil
	if len(shim.Spec) > 0 && string(shim.Spec) != "null" {
		spec := &configv1.EdgeRouteSpec{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(shim.Spec, spec); err != nil {
			return err
		}
		in.Spec = spec
	}
	return nil
}
