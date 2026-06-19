// Package meshconfig hosts the MeshConfig CRD machinery that runs in the
// aether-controller: a validating admission webhook (protovalidate) and a
// reconciler that projects the singleton MeshConfig custom resource into the
// ConfigMap the agent consumes. It works against the typed MeshConfig object
// (api/aether/config/v1), not unstructured. See
// docs/proposals/015_mesh-config.md.
package meshconfig

import (
	"fmt"

	"buf.build/go/protovalidate"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

const (
	// SingletonName is the only MeshConfig name the controller acts on; the CRD
	// is a cluster-scoped singleton.
	SingletonName = "default"

	// ConfigMapKey is the key under which the projected config is written.
	ConfigMapKey = "mesh-config.yaml"

	// DefaultMeshConfigMapName is the ConfigMap the reconciler projects into and
	// that the agent mounts.
	DefaultMeshConfigMapName = "aether-mesh-config"
)

// Validate runs protovalidate on a MeshConfig spec. A nil spec (the proxy
// inherits everything from the aether config) is valid.
func Validate(spec *configv1.MeshConfigSpec) error {
	if spec == nil {
		return nil
	}
	if err := protovalidate.Validate(spec); err != nil {
		return fmt.Errorf("MeshConfig spec failed validation: %w", err)
	}
	return nil
}

// RenderConfigMapData serializes a MeshConfig spec to the YAML document the agent
// loads, returning the ConfigMap `data` map. A nil spec renders an empty document
// (the agent then inherits everything from the aether config).
func RenderConfigMapData(spec *configv1.MeshConfigSpec) (map[string]string, error) {
	if spec == nil {
		return map[string]string{ConfigMapKey: "{}\n"}, nil
	}
	// protojson then JSON->YAML so the projected document uses the protojson field
	// names the agent's loader expects (and round-trips through config.Parse).
	jsonBytes, err := protojson.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal MeshConfig spec: %w", err)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("convert MeshConfig spec to YAML: %w", err)
	}
	return map[string]string{ConfigMapKey: string(yamlBytes)}, nil
}
