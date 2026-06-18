// Package config loads the mesh-wide aether configuration (aether.config.v1.
// MeshConfig) from a YAML document — typically a mounted ConfigMap — validating
// it with protovalidate. Per-instance/topology settings stay command-line flags
// and are not part of MeshConfig; see docs/proposals/015_mesh-config.md.
package config

import (
	"fmt"
	"os"

	"buf.build/go/protovalidate"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

const (
	defaultAccessLogSuccessSampleRate  uint32  = 100
	defaultProxyTraceSampleRate        float64 = 0.01
	defaultControlPlaneTraceSampleRate float64 = 0.1
)

// Load reads, parses, validates and defaults a MeshConfig from the file at path.
//
// The pipeline is YAML -> JSON -> protojson (strict: unknown fields are an
// error, so a typo or stale key fails loudly) -> protovalidate -> defaults.
func Load(path string) (*configv1.MeshConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mesh config %q: %w", path, err)
	}
	return Parse(raw)
}

// Parse runs the YAML->proto->validate->default pipeline on an in-memory
// document. Load is the file-backed wrapper; Parse exists for tests and for
// callers that already hold the bytes.
func Parse(raw []byte) (*configv1.MeshConfig, error) {
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("mesh config is not valid YAML: %w", err)
	}

	cfg := &configv1.MeshConfig{}
	// DiscardUnknown defaults to false: an unrecognized field is rejected rather
	// than silently dropped, so typos and removed keys surface at startup.
	if err := protojson.Unmarshal(jsonBytes, cfg); err != nil {
		return nil, fmt.Errorf("mesh config does not match the MeshConfig schema: %w", err)
	}

	if err := protovalidate.Validate(cfg); err != nil {
		return nil, fmt.Errorf("mesh config failed validation: %w", err)
	}

	withDefaults(cfg)
	return cfg, nil
}

// withDefaults fills the optional rate/percentage fields that have a non-zero
// historical default. Booleans and strings default to their zero value, which
// matches the previous flag defaults (all off / empty). Only the proto3
// `optional` fields need this — an explicit 0 in the document is preserved.
func withDefaults(cfg *configv1.MeshConfig) {
	t := cfg.GetTelemetry()
	if t == nil {
		return
	}
	if al := t.GetAccessLogs(); al != nil && al.SuccessSampleRate == nil {
		al.SuccessSampleRate = ptr(defaultAccessLogSuccessSampleRate)
	}
	if pt := t.GetProxyTracing(); pt != nil && pt.SampleRate == nil {
		pt.SampleRate = ptr(defaultProxyTraceSampleRate)
	}
	if tr := t.GetTracing(); tr != nil && tr.SampleRate == nil {
		tr.SampleRate = ptr(defaultControlPlaneTraceSampleRate)
	}
}

func ptr[T any](v T) *T { return &v }
