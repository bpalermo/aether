// Package meshconfig hosts the MeshConfig CRD machinery that runs in the
// registrar: a validating admission webhook (protovalidate) and a reconciler
// that projects the cluster-scoped MeshConfig custom resource into the
// ConfigMap the agent (and registrar) consume. See
// docs/proposals/015_mesh-config.md.
package meshconfig

import (
	"encoding/json"
	"fmt"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/bpalermo/aether/common/config"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

const (
	// Group/Version/Kind of the MeshConfig custom resource. The API is v1 from the
	// start: the spec is the aether.config.v1.MeshConfig proto, whose
	// field-number-based evolution guarantees backward/forward compatibility, so
	// no alpha/beta churn is needed.
	Group   = "config.aether.io"
	Version = "v1"
	Kind    = "MeshConfig"
	// Resource is the lowercase plural used in RBAC and the REST path.
	Resource = "meshconfigs"

	// SingletonName is the only MeshConfig name the controller acts on; the CRD
	// is a cluster-scoped singleton.
	SingletonName = "default"

	// ConfigMapKey is the key under which the projected config is written.
	ConfigMapKey = "mesh-config.yaml"

	// DefaultMeshConfigMapName is the ConfigMap the reconciler projects into and
	// that the agent and registrar mount.
	DefaultMeshConfigMapName = "aether-mesh-config"
)

// ProtoFromSpec converts a MeshConfig CR `.spec` (decoded JSON/YAML object) into
// a validated, defaulted MeshConfig proto. It reuses the exact pipeline the file
// loader uses (protojson strict → protovalidate → defaults), so the webhook, the
// reconciler, and the agent/registrar file load all agree on what is valid.
func ProtoFromSpec(spec map[string]any) (*configv1.MeshConfig, error) {
	// spec is already a decoded object; marshal back to JSON (valid YAML) and run
	// it through the shared loader.
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal MeshConfig spec: %w", err)
	}
	return config.Parse(raw)
}

// RenderConfigMapData serializes a validated MeshConfig to the YAML document the
// binaries load, returning the ConfigMap `data` map.
func RenderConfigMapData(cfg *configv1.MeshConfig) (map[string]string, error) {
	// protojson then JSON→YAML so the projected document matches the protojson
	// field names the loader expects (and round-trips through config.Parse).
	jsonBytes, err := protojson.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal MeshConfig: %w", err)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("convert MeshConfig to YAML: %w", err)
	}
	return map[string]string{ConfigMapKey: string(yamlBytes)}, nil
}
