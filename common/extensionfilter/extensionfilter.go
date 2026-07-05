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
	"google.golang.org/protobuf/types/known/anypb"
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

// HeaderToMetadataFilterName is the Envoy filter the typed header_to_metadata
// authoring form renders to.
const HeaderToMetadataFilterName = "envoy.filters.http.header_to_metadata"

// defaultMetadataNamespace is where header_to_metadata rules write when the rule
// omits a namespace — "envoy.lb", the subset-routing namespace (aether's use case).
const defaultMetadataNamespace = "envoy.lb"

// ValidateSpec validates an HTTPFilterSpec fail-closed, IN-PROCESS (no Envoy binary):
//   - exactly ONE authoring form is set: typed (headerToMetadata) XOR opaque
//     (filter + typedConfig) — proposal 025 M4 typed promotion;
//   - opaque form: spec.filter is allow-listed and spec.typedConfig resolves by its
//     `@type` to a known Envoy message (else unknown/malformed → reject) passing its
//     protoc-gen-validate constraints (the same `(validate.rules)` Envoy enforces);
//   - typed form: rules are structurally valid (rendered internally, always
//     allow-listed by construction);
//   - scope CHAIN (M4: service-wide always-on) requires a Service targetRef — the
//     "one CHAIN filter per service" invariant is enforced by the webhook (it needs
//     the cluster view), not here.
//
// It does NOT prove the config composes with the live chain (ordering, filter
// interactions) — that's the optional `envoy --mode validate` CI gate. Returns nil
// when the spec is admissible.
func ValidateSpec(spec *configprotov1.HTTPFilterSpec) error {
	if spec == nil {
		return errors.New("spec is required")
	}
	typed := spec.GetHeaderToMetadata() != nil
	opaque := spec.GetFilter() != "" || spec.GetTypedConfig() != nil
	if typed && opaque {
		return errors.New("set exactly one authoring form: headerToMetadata (typed) OR filter+typedConfig (opaque), not both")
	}
	if !typed && !opaque {
		return errors.New("spec must set an authoring form: headerToMetadata (typed) or filter+typedConfig (opaque)")
	}
	if spec.GetScope() == configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
		if !targetsAService(spec) {
			return errors.New("spec.scope CHAIN (service-wide always-on) requires a targetRef of kind Service")
		}
	}
	if typed {
		for i, r := range spec.GetHeaderToMetadata().GetRules() {
			if r.GetHeader() == "" || r.GetMetadataKey() == "" {
				return fmt.Errorf("headerToMetadata.rules[%d]: header and metadataKey are required", i)
			}
		}
		return nil
	}
	name := spec.GetFilter()
	if name == "" {
		return errors.New("spec.filter is required")
	}
	if !Allowed(name) {
		return fmt.Errorf("spec.filter %q is not a supported extension filter (not in aether's allow-list / proxy build)", name)
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

// targetsAService reports whether the spec has a targetRef of kind Service (core group).
func targetsAService(spec *configprotov1.HTTPFilterSpec) bool {
	for _, t := range spec.GetTargetRefs() {
		if (t.GetGroup() == "" || t.GetGroup() == "core") && t.GetKind() == "Service" {
			return true
		}
	}
	return false
}

// Render resolves an HTTPFilterSpec's authoring form to the concrete Envoy filter
// name + typed config (proposal 025 M4): the typed headerToMetadata form is rendered
// to envoy.filters.http.header_to_metadata config; the opaque form passes through
// verbatim. Assumes ValidateSpec passed; callers on unvalidated input must check the
// error.
func Render(spec *configprotov1.HTTPFilterSpec) (string, *anypb.Any, error) {
	if h2m := spec.GetHeaderToMetadata(); h2m != nil {
		cfg := &header_to_metadatav3.Config{}
		for _, r := range h2m.GetRules() {
			ns := r.GetMetadataNamespace()
			if ns == "" {
				ns = defaultMetadataNamespace
			}
			cfg.RequestRules = append(cfg.RequestRules, &header_to_metadatav3.Config_Rule{
				Header: r.GetHeader(),
				OnHeaderPresent: &header_to_metadatav3.Config_KeyValuePair{
					MetadataNamespace: ns,
					Key:               r.GetMetadataKey(),
					Type:              header_to_metadatav3.Config_STRING,
				},
			})
		}
		a, err := anypb.New(cfg)
		if err != nil {
			return "", nil, fmt.Errorf("render headerToMetadata: %w", err)
		}
		return HeaderToMetadataFilterName, a, nil
	}
	return spec.GetFilter(), spec.GetTypedConfig(), nil
}
