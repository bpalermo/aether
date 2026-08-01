// Package config loads the proxy MeshConfig (aether.config.v1.MeshConfig) from a
// YAML document — typically a mounted ConfigMap projected from the MeshConfig CR
// — validating it with protovalidate. System-wide settings (OTEL, SPIRE, mesh
// domain) are aether (umbrella-chart) config inherited as flags, not part of
// MeshConfig; see docs/proposals/015_mesh-config.md.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"buf.build/go/protovalidate"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

// Load reads, parses and validates a MeshConfig from the file at path.
//
// The pipeline is YAML -> JSON -> protojson (lenient: unknown fields are ignored
// for forward-compatibility across versions) -> protovalidate. Inheritance of
// unset proxy overrides from the aether system config is the caller's job (the
// agent), which is the only place that holds both.
//
// A missing file is NOT an error: the MeshConfig (proxy observability overrides)
// is optional, so its absence means "inherit everything from the aether system
// config". This decouples startup from the controller having projected the
// ConfigMap yet, and lets deployments without a mounted config (e.g. e2e) run.
func Load(path string) (*configv1.MeshConfigSpec, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &configv1.MeshConfigSpec{}, nil
	}
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
	// DiscardUnknown: ignore unknown fields rather than reject them. This is the
	// protobuf forward-compatibility contract — an older agent must not fail to
	// load a ConfigMap a newer controller projected with a field it doesn't know
	// (mixed versions during a rollout). protovalidate checks the known fields.
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(jsonBytes, cfg); err != nil {
		return nil, fmt.Errorf("mesh config does not match the MeshConfig schema: %w", err)
	}

	if err := protovalidate.Validate(cfg); err != nil {
		return nil, fmt.Errorf("mesh config failed validation: %w", err)
	}

	return cfg, nil
}
