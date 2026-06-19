// Package config loads the proxy MeshConfig (aether.config.v1.MeshConfig) from a
// YAML document — typically a mounted ConfigMap projected from the MeshConfig CR
// — validating it with protovalidate. System-wide settings (OTEL, SPIRE, mesh
// domain) are aether (umbrella-chart) config inherited as flags, not part of
// MeshConfig; see docs/proposals/015_mesh-config.md.
package config

import (
	"fmt"
	"os"

	"buf.build/go/protovalidate"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

// Load reads, parses and validates a MeshConfig from the file at path.
//
// The pipeline is YAML -> JSON -> protojson (strict: unknown fields are an
// error, so a typo or stale key fails loudly) -> protovalidate. Inheritance of
// unset proxy overrides from the aether system config is the caller's job (the
// agent), which is the only place that holds both.
func Load(path string) (*configv1.MeshConfigSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mesh config %q: %w", path, err)
	}
	return Parse(raw)
}

// Parse runs the YAML->proto->validate pipeline on an in-memory document. Load is
// the file-backed wrapper; Parse exists for tests and for callers that already
// hold the bytes.
func Parse(raw []byte) (*configv1.MeshConfigSpec, error) {
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("mesh config is not valid YAML: %w", err)
	}

	cfg := &configv1.MeshConfigSpec{}
	// DiscardUnknown defaults to false: an unrecognized field is rejected rather
	// than silently dropped, so typos and removed keys surface at startup.
	if err := protojson.Unmarshal(jsonBytes, cfg); err != nil {
		return nil, fmt.Errorf("mesh config does not match the MeshConfig schema: %w", err)
	}

	if err := protovalidate.Validate(cfg); err != nil {
		return nil, fmt.Errorf("mesh config failed validation: %w", err)
	}

	return cfg, nil
}
