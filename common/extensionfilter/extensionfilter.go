// Package extensionfilter is the single source of truth for the proxy-extension
// escape hatch (proposal 025): the allow-list of Envoy HTTP filters aether supports
// and the in-process, fail-closed validation of an HTTPFilter's opaque typed_config.
//
// It lives in common/ (not agent/internal) because BOTH the agent (the GAMMA
// reconciler + the xDS proxy builders) AND the controller (the admission webhook,
// which cannot import the agent's internal packages) depend on it.
package extensionfilter

import (
	"errors"
	"fmt"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"google.golang.org/protobuf/proto"
)

// allowed maps each allow-listed Envoy HTTP filter name to a builder for an EMPTY
// default config of the filter's HCM type. The set is the filters the aether proxy
// build actually compiles in (proxy/bazel/extension_config/extensions_build_config.bzl):
// a payload for any other filter would proto-validate yet NACK at runtime, so the
// webhook rejects it. The default config is behaviour-neutral — carried only on the
// default-disabled HCM chain entry (the per-route typed_per_filter_config supplies the
// real config). Keep in lockstep with the proxy build's extensions (proposals 010/011).
var allowed = map[string]func() proto.Message{
	"envoy.filters.http.header_to_metadata": func() proto.Message { return &header_to_metadatav3.Config{} },
	"envoy.filters.http.header_mutation":    func() proto.Message { return &header_mutationv3.HeaderMutation{} },
}

// Allowed reports whether name is a supported escape-hatch filter.
func Allowed(name string) bool {
	_, ok := allowed[name]
	return ok
}

// DefaultConfig returns an empty default config message for an allow-listed filter
// (the neutral config carried on its default-disabled HCM entry), or (nil, false).
func DefaultConfig(name string) (proto.Message, bool) {
	b, ok := allowed[name]
	if !ok {
		return nil, false
	}
	return b(), true
}

// ValidateSpec validates an HTTPFilterSpec fail-closed, IN-PROCESS (no Envoy binary):
//   - spec.filter is non-empty and allow-listed (a filter the proxy build compiles in);
//   - spec.scope is not CHAIN (deferred to a later milestone — route scope only);
//   - spec.typedConfig resolves by its `@type` to a known Envoy message (else unknown/
//     malformed → reject), and that message passes its protoc-gen-validate constraints
//     (the same `(validate.rules)` Envoy enforces at config load).
//
// It does NOT prove the config composes with the live chain (ordering, filter
// interactions) — that's the optional `envoy --mode validate` CI gate. Returns nil
// when the spec is admissible.
func ValidateSpec(spec *configprotov1.HTTPFilterSpec) error {
	if spec == nil {
		return errors.New("spec is required")
	}
	name := spec.GetFilter()
	if name == "" {
		return errors.New("spec.filter is required")
	}
	if !Allowed(name) {
		return fmt.Errorf("spec.filter %q is not a supported extension filter (not in aether's allow-list / proxy build)", name)
	}
	if spec.GetScope() == configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
		return errors.New("spec.scope CHAIN is not yet supported; use route scope (ExtensionRef)")
	}
	tc := spec.GetTypedConfig()
	if tc == nil {
		return errors.New("spec.typedConfig is required")
	}
	// UnmarshalNew resolves the Any's @type against the linked proto registry and
	// decodes it — an unknown/unsupported @type or malformed bytes fail here.
	msg, err := tc.UnmarshalNew()
	if err != nil {
		return fmt.Errorf("spec.typedConfig @type is unknown or malformed: %w", err)
	}
	// protoc-gen-validate: the Envoy go-control-plane types carry ValidateAll().
	if v, ok := msg.(interface{ ValidateAll() error }); ok {
		if err := v.ValidateAll(); err != nil {
			return fmt.Errorf("spec.typedConfig failed validation: %w", err)
		}
	}
	return nil
}
